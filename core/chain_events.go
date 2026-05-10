package core

import (
	"context"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	storageTypes "atlas/x/storage/types"

	"cygnus/atlas"
	"cygnus/storage"

	"github.com/cosmos/cosmos-sdk/types/query"
	"go.uber.org/zap"
)

const (
	challengeRoundBlocks       int64 = 10
	challengeProofSpreadBlocks       = int(challengeRoundBlocks)
)

// chainEventReceiver bridges atlas blockchain events to the storage manager.
type chainEventReceiver struct {
	atlas             *atlas.AtlasManager
	storage           *storage.StorageManager
	logger            *zap.Logger
	latestBlockHeight atomic.Int64
}

var _ atlas.ChainEventReceiver = (*chainEventReceiver)(nil)

func (r *chainEventReceiver) OnNewBlock(ctx context.Context, height int64) error {
	r.recordBlockHeight(height)
	return nil
}

func (r *chainEventReceiver) OnFileDeleted(ctx context.Context, fileID string) error {
	return r.storage.DeleteFile(ctx, fileID)
}

func (r *chainEventReceiver) OnStartProofRound(ctx context.Context, height int64, roundOrData string) error {
	r.recordBlockHeight(height)
	r.storage.RecordChainSync(time.Now().UTC())
	r.logger.Info("Proof round started", zap.Int64("height", height), zap.String("round", roundOrData))

	challenges, err := r.queryAllProviderChallenges(ctx)
	if err != nil {
		return err
	}

	r.scheduleChallengeProofs(ctx, height, roundOrData, challenges)

	return nil
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
		r.logger.Info("No challenges to prove for proof round",
			zap.Int64("height", roundStartHeight),
			zap.String("round", round))
		return
	}

	currentHeight := r.currentBlockHeight(roundStartHeight)
	challenges, skippedOld, skippedFuture, skippedInvalid := r.filterCurrentRoundChallenges(challenges, currentHeight)
	if skippedOld > 0 || skippedFuture > 0 || skippedInvalid > 0 {
		r.logger.Info("Dropped stale challenge proofs before scheduling",
			zap.Int64("current_height", currentHeight),
			zap.Int64("active_round_start", currentChallengeRoundStart(currentHeight)),
			zap.Int("skipped_old", skippedOld),
			zap.Int("skipped_future", skippedFuture),
			zap.Int("skipped_invalid", skippedInvalid),
			zap.Int("remaining_challenges", len(challenges)))
	}
	if len(challenges) == 0 {
		r.logger.Info("No current-round challenges to prove",
			zap.Int64("height", roundStartHeight),
			zap.String("round", round))
		return
	}

	batches := make(map[int64][]*storageTypes.StorageChallenge)
	for i, challenge := range challenges {
		targetHeight := roundStartHeight + 1 + int64(i%challengeProofSpreadBlocks)
		// DeadlineHeight is still a valid block for challenge proofs; txs are
		// processed before the end-block missed-challenge cleanup runs.
		if challenge.DeadlineHeight > 0 && targetHeight > int64(challenge.DeadlineHeight) {
			targetHeight = int64(challenge.DeadlineHeight)
		}
		if targetHeight <= roundStartHeight {
			targetHeight = roundStartHeight + 1
		}
		batches[targetHeight] = append(batches[targetHeight], challenge)
	}

	r.logger.Info("Scheduled challenge proofs across upcoming blocks",
		zap.Int64("round_start_height", roundStartHeight),
		zap.String("round", round),
		zap.Int("challenge_count", len(challenges)),
		zap.Int("block_count", len(batches)))

	for targetHeight, batch := range batches {
		targetHeight := targetHeight
		batch := append([]*storageTypes.StorageChallenge(nil), batch...)
		go r.proveChallengeBatchAtHeight(ctx, round, targetHeight, batch)
	}
}

func (r *chainEventReceiver) proveChallengeBatchAtHeight(ctx context.Context, round string, targetHeight int64, challenges []*storageTypes.StorageChallenge) {
	broadcastHeight := targetHeight - 1
	if broadcastHeight < 0 {
		broadcastHeight = 0
	}

	if err := r.atlas.WaitForHeight(ctx, broadcastHeight); err != nil {
		r.logger.Error("Failed waiting to submit scheduled challenge proofs",
			zap.String("round", round),
			zap.Int64("target_height", targetHeight),
			zap.Int64("broadcast_height", broadcastHeight),
			zap.Int("challenge_count", len(challenges)),
			zap.Error(err))
		return
	}

	r.logger.Info("Submitting scheduled challenge proofs",
		zap.String("round", round),
		zap.Int64("target_height", targetHeight),
		zap.Int64("broadcast_height", broadcastHeight),
		zap.Int("challenge_count", len(challenges)))

	if latestHeight, err := r.atlas.LatestBlockHeight(ctx); err != nil {
		r.logger.Warn("Failed to refresh latest block height before proving challenges",
			zap.Int64("target_height", targetHeight),
			zap.Error(err))
	} else {
		r.recordBlockHeight(latestHeight)
	}

	for _, challenge := range challenges {
		currentHeight := r.currentBlockHeight(targetHeight)
		if !r.isChallengeCurrent(challenge, currentHeight) {
			r.logger.Info("Dropping stale scheduled challenge proof",
				zap.Int64("current_height", currentHeight),
				zap.Int64("active_round_start", currentChallengeRoundStart(currentHeight)),
				zap.String("challenge_id", challenge.ChallengeId),
				zap.Uint64("created_height", challenge.CreatedHeight),
				zap.Int64("target_height", targetHeight))
			continue
		}

		r.storage.RecordChainSync(time.Now().UTC())
		err := r.storage.ProveFile(ctx, challenge.FileId, challenge.ChallengeId, int64(challenge.ChunkIndex))
		if err != nil {
			r.logger.Error("Failed to prove challenge",
				zap.String("file_id", challenge.FileId),
				zap.String("challenge_id", challenge.ChallengeId),
				zap.Int64("chunk", int64(challenge.ChunkIndex)),
				zap.Int64("target_height", targetHeight),
				zap.Error(err))
			continue
		}
	}
}

func (r *chainEventReceiver) OnStartProofWindow(ctx context.Context, height int64, windowOrData string) error {
	r.recordBlockHeight(height)
	// [TBD]: this chain-sync is pretty useless. remove it?
	r.storage.RecordChainSync(time.Now().UTC())
	r.logger.Info("Proof window started", zap.Int64("height", height), zap.String("window", windowOrData))
	return nil
}

func (r *chainEventReceiver) recordBlockHeight(height int64) {
	for {
		current := r.latestBlockHeight.Load()
		if height <= current {
			return
		}
		if r.latestBlockHeight.CompareAndSwap(current, height) {
			return
		}
	}
}

func (r *chainEventReceiver) currentBlockHeight(fallback int64) int64 {
	height := r.latestBlockHeight.Load()
	if height > 0 {
		return height
	}
	return fallback
}

func (r *chainEventReceiver) filterCurrentRoundChallenges(challenges []*storageTypes.StorageChallenge, currentHeight int64) ([]*storageTypes.StorageChallenge, int, int, int) {
	filtered := make([]*storageTypes.StorageChallenge, 0, len(challenges))
	var skippedOld int
	var skippedFuture int
	var skippedInvalid int

	activeRoundStart := currentChallengeRoundStart(currentHeight)
	for _, challenge := range challenges {
		challengeRoundStart, ok := challengeRoundStartHeight(challenge)
		if !ok {
			skippedInvalid++
			continue
		}
		switch {
		case challengeRoundStart < activeRoundStart:
			skippedOld++
		case challengeRoundStart > activeRoundStart:
			skippedFuture++
		default:
			filtered = append(filtered, challenge)
		}
	}

	return filtered, skippedOld, skippedFuture, skippedInvalid
}

func (r *chainEventReceiver) isChallengeCurrent(challenge *storageTypes.StorageChallenge, currentHeight int64) bool {
	challengeRoundStart, ok := challengeRoundStartHeight(challenge)
	return ok && challengeRoundStart == currentChallengeRoundStart(currentHeight)
}

func currentChallengeRoundStart(currentHeight int64) int64 {
	if currentHeight <= 0 {
		return 0
	}
	return (currentHeight / challengeRoundBlocks) * challengeRoundBlocks
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
