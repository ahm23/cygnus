package core

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	storageTypes "atlas/x/storage/types"

	"cygnus/atlas"
	"cygnus/storage"

	"github.com/cosmos/cosmos-sdk/types/query"
	"github.com/rs/zerolog/log"
)

const (
	challengeRoundBlocks        int64 = 10
	challengeProofSpreadBlocks        = int(challengeRoundBlocks * 75 / 100)
	challengeRoundQueryAttempts       = 3
	challengeRoundQueryDelay          = 1000 * time.Millisecond
)

// chainEventReceiver bridges atlas blockchain events to the storage manager.
type chainEventReceiver struct {
	atlas             *atlas.AtlasManager
	storage           *storage.StorageManager
	latestBlockHeight atomic.Int64
	proofRoundMu      sync.Mutex
	proofRounds       map[int64]struct{}
}

var _ atlas.ChainEventReceiver = (*chainEventReceiver)(nil)

// OnNewBlock is an event handler for new block events.
func (r *chainEventReceiver) OnNewBlock(ctx context.Context, height int64) {
	r.latestBlockHeight.Swap(height)
	r.storage.RecordChainSync(time.Now().UTC())
}

// OnFileDeleted is an event handler for file delete tx events.
func (r *chainEventReceiver) OnFileDeleted(ctx context.Context, fileID string) error {
	return r.storage.DeleteFile(ctx, fileID)
}

// OnProposalPassed is an event handler that refreshes the storage params after
// any governance proposal implementation. Waits for the next block before refreshing params.
func (r *chainEventReceiver) OnProposalPassed(ctx context.Context, proposalID uint64) error {
	nextHeight := r.latestBlockHeight.Load() + 1
	log.Info().
		Uint64("proposal_id", proposalID).
		Int64("next_height", nextHeight).
		Msg("Governance proposal passed; waiting for next block to refresh storage module params")

	if err := r.atlas.WaitForBlockHeight(ctx, nextHeight); err != nil {
		return fmt.Errorf("wait for block %d before refreshing params after proposal %d: %w", nextHeight, proposalID, err)
	}

	if err := r.atlas.RefreshStorageParams(ctx); err != nil {
		return fmt.Errorf("refresh storage params after proposal %d passed: %w", proposalID, err)
	}
	return nil
}

// OnStartProofRound is an event handler for new proof round events.
func (r *chainEventReceiver) OnStartProofRound(ctx context.Context, height int64, round string) error {
	// Skip if catch-up already processed this round.
	r.proofRoundMu.Lock()
	_, exists := r.proofRounds[height]
	if !exists {
		r.proofRounds[height] = struct{}{}
	}
	r.proofRoundMu.Unlock()
	if exists {
		log.Debug().
			Int64("round_start", height).
			Str("round", round).
			Msg("Proof round already processed by catch-up; skipping")
		return nil
	}

	log.Debug().Str("round", round).Msg("Started new challenge round")
	challenges, err := r.queryProviderChallengesForRound(ctx, height, round)
	if err != nil {
		return err
	}

	r.scheduleChallengeProofs(ctx, height, round, challenges)

	return nil
}

// catchUpChallengeRound queries challenges for the ongoing challenge round and schedules proofs.
// This is called on startup to recover missed rounds when the provider restarts mid-round.
func (r *chainEventReceiver) catchUpChallengeRound(ctx context.Context, currentHeight int64, proofRoundBlocks int64) {
	roundStartHeight := (currentHeight / proofRoundBlocks) * proofRoundBlocks
	if roundStartHeight <= 0 {
		return
	}
	round := strconv.FormatInt(roundStartHeight/proofRoundBlocks, 10)

	// Mark as processed immediately so the block-poller's OnStartProofRound
	// skips it when it eventually reaches the boundary.
	r.proofRoundMu.Lock()
	if _, ok := r.proofRounds[roundStartHeight]; ok {
		r.proofRoundMu.Unlock()
		return
	}
	r.proofRounds[roundStartHeight] = struct{}{}
	r.proofRoundMu.Unlock()

	// Prime height tracking so challenge scheduling and WaitForBlockHeight
	// use realistic timing instead of waiting for the first poller tick.
	r.latestBlockHeight.Swap(currentHeight)
	if r.atlas != nil {
		r.atlas.Height = currentHeight
	}
	r.storage.RecordChainSync(time.Now().UTC())

	log.Info().
		Int64("current_height", currentHeight).
		Int64("round_start", roundStartHeight).
		Str("round", round).
		Msg("Catching up on challenge round after restart")

	challenges, err := r.queryProviderChallengesForRound(ctx, roundStartHeight, round)
	if err != nil {
		log.Warn().Err(err).
			Int64("round_start", roundStartHeight).
			Str("round", round).
			Msg("Failed to query challenges during catch-up; proofs may be missed for this round")
		return
	}

	r.scheduleChallengeProofs(ctx, roundStartHeight, round, challenges)
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
		if len(current) > 0 || attempt == challengeRoundQueryAttempts {
			log.Debug().
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
			return challenges, nil
		}

		log.Debug().
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
		log.Info().
			Int64("height", roundStartHeight).
			Str("round", round).
			Msg("No challenges to prove for proof round")
		return
	}

	currentHeight := maxInt64(r.latestBlockHeight.Load(), roundStartHeight)
	challenges, skippedOld, skippedFuture, skippedInvalid := r.filterRoundChallenges(challenges, roundStartHeight)
	if skippedOld > 0 || skippedFuture > 0 || skippedInvalid > 0 {
		log.Info().
			Int64("round_start_height", roundStartHeight).
			Int64("current_height", currentHeight).
			Int("skipped_old", skippedOld).
			Int("skipped_future", skippedFuture).
			Int("skipped_invalid", skippedInvalid).
			Int("remaining_challenges", len(challenges)).
			Msg("Dropped stale challenge proofs before scheduling")
	}
	if len(challenges) == 0 {
		log.Info().
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
		log.Info().
			Int64("current_height", currentHeight).
			Int("skipped_expired", skippedExpired).
			Int("remaining_challenges", len(challenges)-skippedExpired).
			Msg("Dropped expired challenge proofs before scheduling")
	}
	if len(batches) == 0 {
		log.Info().
			Int64("round_start_height", roundStartHeight).
			Int64("current_height", currentHeight).
			Str("round", round).
			Msg("No schedulable current-round challenges to prove")
		return
	}

	log.Info().
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
		log.Error().
			Str("round", round).
			Int64("broadcast_height", broadcastHeight).
			Err(err).
			Msg("Failed waiting to submit scheduled challenge proofs")
		return
	}

	log.Debug().
		Int("count", len(challenges)).
		Int64("height", broadcastHeight+1).
		Msg("Submitting challenge proofs")

	for _, challenge := range challenges {
		// soft-check that challenge has not expired
		currentHeight := r.latestBlockHeight.Load()
		if !r.isChallengeProveableForRound(challenge, roundStartHeight, currentHeight) {
			// Dev Note: this only happens when the provider is under extreme loads (typically long-rolling DDoS)
			log.Info().
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
			log.Error().
				Str("file_id", challenge.FileId).
				Int64("chunk", int64(challenge.ChunkIndex)).
				Err(err).
				Msg("Failed to prove challenge")
			continue
		}
	}
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
