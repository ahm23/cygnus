package core

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	storageTypes "atlas/x/storage/types"

	"cygnus/atlas"
	"cygnus/storage"

	"github.com/cosmos/cosmos-sdk/types/query"
	"github.com/rs/zerolog"
)

const (
	challengeRoundBlocks        int64 = 10
	challengeProofSpreadBlocks        = int(challengeRoundBlocks * 75 / 100)
	challengeRoundQueryAttempts       = 12
	challengeRoundQueryDelay          = 250 * time.Millisecond
)

// chainEventReceiver bridges atlas blockchain events to the storage manager.
type chainEventReceiver struct {
	atlas             *atlas.AtlasManager
	storage           *storage.StorageManager
	logger            zerolog.Logger
	latestBlockHeight atomic.Int64
	proofRoundMu      sync.Mutex
	proofRounds       map[int64]struct{}
}

var _ atlas.ChainEventReceiver = (*chainEventReceiver)(nil)

// === OnNewBlock is an event handler for new block events
func (r *chainEventReceiver) OnNewBlock(ctx context.Context, height int64) {
	r.latestBlockHeight.Swap(height)
	r.storage.RecordChainSync(time.Now().UTC())
}

func (r *chainEventReceiver) OnFileDeleted(ctx context.Context, fileID string) error {
	return r.storage.DeleteFile(ctx, fileID)
}

// === OnStartProofRound is an event handler for new proof round events
func (r *chainEventReceiver) OnStartProofRound(ctx context.Context, height int64, round string) error {
	r.logger.Debug().Str("round", round).Msg("Started new challenge round")
	challenges, err := r.queryProviderChallengesForRound(ctx, height, round)
	if err != nil {
		return err
	}

	r.scheduleChallengeProofs(ctx, height, round, challenges)

	return nil
}

func (r *chainEventReceiver) queryProviderChallengesForRound(ctx context.Context, roundStartHeight int64, round string) ([]*storageTypes.StorageChallenge, error) {
	var lastChallenges []*storageTypes.StorageChallenge

	for attempt := 1; attempt <= challengeRoundQueryAttempts; attempt++ {
		challenges, err := r.queryAllProviderChallenges(ctx)
		if err != nil {
			return nil, err
		}
		lastChallenges = challenges

		current, skippedOld, skippedFuture, skippedInvalid := r.filterRoundChallenges(challenges, roundStartHeight)
		minRound, maxRound, hasRound := challengeRoundBounds(challenges)
		r.logger.Debug().
			Int("attempt", attempt).
			Str("round", round).
			Int64("round_start_height", roundStartHeight).
			Int("challenge_count", len(challenges)).
			Int("current_round_challenges", len(current)).
			Int("skipped_old", skippedOld).
			Int("skipped_future", skippedFuture).
			Int("skipped_invalid", skippedInvalid).
			Int64("min_challenge_round", minRound).
			Int64("max_challenge_round", maxRound).
			Bool("has_challenge_rounds", hasRound).
			Msg("Fetched challenges for round")

		if len(current) > 0 || attempt == challengeRoundQueryAttempts {
			return challenges, nil
		}

		r.logger.Debug().
			Int("attempt", attempt).
			Str("round", round).
			Int64("round_start_height", roundStartHeight).
			Int("current_round_challenges", len(current)).
			Int("skipped_old", skippedOld).
			Int("skipped_future", skippedFuture).
			Int("skipped_invalid", skippedInvalid).
			Msg("Current-round challenges not visible yet; retrying challenge query")

		select {
		case <-ctx.Done():
			return lastChallenges, ctx.Err()
		case <-time.After(challengeRoundQueryDelay):
		}
	}

	return lastChallenges, nil
}

func (r *chainEventReceiver) queryAllProviderChallenges(ctx context.Context) ([]*storageTypes.StorageChallenge, error) {
	provider := r.atlas.Wallet.GetAddress()
	var challenges []*storageTypes.StorageChallenge
	var nextKey []byte

	for {
		request := &storageTypes.QueryChallengesRequest{
			Provider: provider,
			Pagination: &query.PageRequest{
				Key:   nextKey,
				Limit: query.DefaultLimit,
			},
		}

		var res *storageTypes.QueryChallengesResponse
		var err error
		for attempt := 0; attempt < 3; attempt++ {
			res, err = r.atlas.QueryClients.Storage.Challenges(ctx, request)
			if err == nil {
				break
			}
			time.Sleep(time.Duration(attempt+1) * time.Second)
		}
		if err != nil {
			return nil, err
		}

		challenges = append(challenges, res.Challenges...)
		if res.Pagination == nil || len(res.Pagination.NextKey) == 0 {
			return challenges, nil
		}
		nextKey = append(nextKey[:0], res.Pagination.NextKey...)
	}
}

func (r *chainEventReceiver) scheduleChallengeProofs(ctx context.Context, roundStartHeight int64, round string, challenges []*storageTypes.StorageChallenge) {
	if len(challenges) == 0 {
		r.logger.Info().
			Int64("height", roundStartHeight).
			Str("round", round).
			Msg("No challenges to prove for proof round")
		return
	}

	currentHeight := maxInt64(r.latestBlockHeight.Load(), roundStartHeight)
	challenges, skippedOld, skippedFuture, skippedInvalid := r.filterRoundChallenges(challenges, roundStartHeight)
	if skippedOld > 0 || skippedFuture > 0 || skippedInvalid > 0 {
		r.logger.Info().
			Int64("round_start_height", roundStartHeight).
			Int64("current_height", currentHeight).
			Int("skipped_old", skippedOld).
			Int("skipped_future", skippedFuture).
			Int("skipped_invalid", skippedInvalid).
			Int("remaining_challenges", len(challenges)).
			Msg("Dropped stale challenge proofs before scheduling")
	}
	if len(challenges) == 0 {
		r.logger.Info().
			Int64("round_height", roundStartHeight).
			Str("round", round).
			Int("skipped_old", skippedOld).
			Int("skipped_future", skippedFuture).
			Int("skipped_invalid", skippedInvalid).
			Msg("No current-round challenges to prove")
		return
	}

	batches := make(map[int64][]*storageTypes.StorageChallenge)
	skippedExpired := 0
	firstTargetHeight := int64(0)
	lastTargetHeight := int64(0)
	for i, challenge := range challenges {
		firstTarget, lastTarget, ok := challengeTargetWindow(roundStartHeight, currentHeight, challenge)
		if !ok {
			skippedExpired++
			continue
		}
		if firstTargetHeight == 0 || firstTarget < firstTargetHeight {
			firstTargetHeight = firstTarget
		}
		if lastTarget > lastTargetHeight {
			lastTargetHeight = lastTarget
		}

		targetHeight := firstTarget + int64(i%int(lastTarget-firstTarget+1))
		batches[targetHeight] = append(batches[targetHeight], challenge)
	}
	if skippedExpired > 0 {
		r.logger.Info().
			Int64("current_height", currentHeight).
			Int("skipped_expired", skippedExpired).
			Int("remaining_challenges", len(challenges)-skippedExpired).
			Msg("Dropped expired challenge proofs before scheduling")
	}
	if len(batches) == 0 {
		r.logger.Info().
			Int64("round_start_height", roundStartHeight).
			Int64("current_height", currentHeight).
			Str("round", round).
			Msg("No schedulable current-round challenges to prove")
		return
	}

	r.logger.Info().
		Int64("current_height", currentHeight).
		Int64("first_target_height", firstTargetHeight).
		Int64("last_target_height", lastTargetHeight).
		Int("challenge_count", len(challenges)-skippedExpired).
		Int("block_count", len(batches)).
		Msg("Scheduled challenge proofs across upcoming blocks")

	for targetHeight, batch := range batches {
		targetHeight := targetHeight
		batch := append([]*storageTypes.StorageChallenge(nil), batch...)
		go r.proveChallengeBatchAtHeight(ctx, roundStartHeight, round, targetHeight, batch)
	}
}

// === proveChallengeBatchAtHeight waits for a given block height and submits proofs for a batch of challenges
func (r *chainEventReceiver) proveChallengeBatchAtHeight(ctx context.Context, roundStartHeight int64, round string, targetHeight int64, challenges []*storageTypes.StorageChallenge) {
	broadcastHeight := targetHeight - 1
	if broadcastHeight < 0 {
		broadcastHeight = 0
	}

	// wait for the desired height to submit a batch of proofs
	if err := r.atlas.WaitForBlockHeight(ctx, broadcastHeight); err != nil {
		r.logger.Error().
			Str("round", round).
			Int64("broadcast_height", broadcastHeight).
			Err(err).
			Msg("Failed waiting to submit scheduled challenge proofs")
		return
	}

	r.logger.Debug().
		Int("count", len(challenges)).
		Int64("height", broadcastHeight+1).
		Msg("Submitting challenge proofs")

	for _, challenge := range challenges {
		// soft-check that challenge has not expired
		currentHeight := r.latestBlockHeight.Load()
		if !r.isChallengeProveableForRound(challenge, roundStartHeight, currentHeight) {
			// Dev Note: this only happens when the provider is under extreme loads (typically long-rolling DDoS)
			r.logger.Info().
				Int64("round_start_height", roundStartHeight).
				Int64("current_height", currentHeight).
				Int64("target_height", targetHeight).
				Str("challenge_id", challenge.ChallengeId).
				Msg("Dropping stale challenge")
			continue
		}

		// prove file (respond to the challenge)
		err := r.storage.ProveFile(ctx, challenge.FileId, challenge.ChallengeId, int64(challenge.ChunkIndex))
		if err != nil {
			r.logger.Error().
				Str("file_id", challenge.FileId).
				Int64("chunk", int64(challenge.ChunkIndex)).
				Err(err).
				Msg("Failed to prove challenge")
			continue
		}
	}
}

func (r *chainEventReceiver) claimProofRound(height int64) bool {
	r.proofRoundMu.Lock()
	defer r.proofRoundMu.Unlock()

	if r.proofRounds == nil {
		r.proofRounds = make(map[int64]struct{})
	}
	if _, ok := r.proofRounds[height]; ok {
		return false
	}
	r.proofRounds[height] = struct{}{}

	for roundHeight := range r.proofRounds {
		if roundHeight < height-(challengeRoundBlocks*3) {
			delete(r.proofRounds, roundHeight)
		}
	}

	return true
}

func (r *chainEventReceiver) filterRoundChallenges(challenges []*storageTypes.StorageChallenge, roundStartHeight int64) ([]*storageTypes.StorageChallenge, int, int, int) {
	filtered := make([]*storageTypes.StorageChallenge, 0, len(challenges))
	var skippedOld int
	var skippedFuture int
	var skippedInvalid int

	for _, challenge := range challenges {
		challengeRoundStart, ok := challengeRoundStartHeight(challenge)
		if !ok {
			skippedInvalid++
			continue
		}
		switch {
		case challengeRoundStart < roundStartHeight:
			skippedOld++
		case challengeRoundStart > roundStartHeight:
			skippedFuture++
		default:
			filtered = append(filtered, challenge)
		}
	}

	return filtered, skippedOld, skippedFuture, skippedInvalid
}

func challengeTargetWindow(roundStartHeight, currentHeight int64, challenge *storageTypes.StorageChallenge) (int64, int64, bool) {
	deadlineHeight := challengeDeadlineHeight(roundStartHeight, challenge)

	firstTarget := maxInt64(roundStartHeight+1, currentHeight+1)
	preferredLastTarget := minInt64(roundStartHeight+int64(challengeProofSpreadBlocks), deadlineHeight)
	lastTarget := preferredLastTarget
	if firstTarget > lastTarget {
		lastTarget = deadlineHeight
	}
	if firstTarget > lastTarget {
		return 0, 0, false
	}

	return firstTarget, lastTarget, true
}

func (r *chainEventReceiver) isChallengeProveableForRound(challenge *storageTypes.StorageChallenge, roundStartHeight, currentHeight int64) bool {
	challengeRoundStart, ok := challengeRoundStartHeight(challenge)
	if !ok || challengeRoundStart != roundStartHeight {
		return false
	}
	if currentHeight <= 0 {
		return true
	}
	return currentHeight <= challengeDeadlineHeight(roundStartHeight, challenge)
}

func challengeDeadlineHeight(roundStartHeight int64, challenge *storageTypes.StorageChallenge) int64 {
	deadlineHeight := roundStartHeight + challengeRoundBlocks
	if challenge != nil && challenge.DeadlineHeight > 0 && int64(challenge.DeadlineHeight) < deadlineHeight {
		return int64(challenge.DeadlineHeight)
	}
	return deadlineHeight
}

func challengeRoundStartHeight(challenge *storageTypes.StorageChallenge) (int64, bool) {
	if challenge == nil {
		return 0, false
	}
	if height, ok := challengeRoundStartHeightFromID(challenge.ChallengeId); ok {
		return height, true
	}
	if challenge.CreatedHeight > 0 {
		return int64(challenge.CreatedHeight), true
	}
	return 0, false
}

func challengeRoundStartHeightFromID(challengeID string) (int64, bool) {
	prefix, _, ok := strings.Cut(challengeID, "-")
	if !ok || prefix == "" {
		return 0, false
	}
	height, err := strconv.ParseInt(prefix, 10, 64)
	if err != nil || height < 0 {
		return 0, false
	}
	return height, true
}

func challengeRoundBounds(challenges []*storageTypes.StorageChallenge) (int64, int64, bool) {
	var minRound int64
	var maxRound int64
	var hasRound bool

	for _, challenge := range challenges {
		roundStart, ok := challengeRoundStartHeight(challenge)
		if !ok {
			continue
		}
		if !hasRound || roundStart < minRound {
			minRound = roundStart
		}
		if !hasRound || roundStart > maxRound {
			maxRound = roundStart
		}
		hasRound = true
	}

	return minRound, maxRound, hasRound
}

func minInt64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
