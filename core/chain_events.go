package core

import (
	"context"
	"time"

	storageTypes "atlas/x/storage/types"

	"cygnus/atlas"
	"cygnus/storage"

	"go.uber.org/zap"
)

const challengeProofSpreadBlocks = 9

// chainEventReceiver bridges atlas blockchain events to the storage manager.
type chainEventReceiver struct {
	atlas   *atlas.AtlasManager
	storage *storage.StorageManager
	logger  *zap.Logger
}

var _ atlas.ChainEventReceiver = (*chainEventReceiver)(nil)

func (r *chainEventReceiver) OnFileDeleted(ctx context.Context, fileID string) error {
	return r.storage.DeleteFile(ctx, fileID)
}

func (r *chainEventReceiver) OnStartProofRound(ctx context.Context, height int64, roundOrData string) error {
	r.storage.RecordChainSync(time.Now().UTC())
	r.logger.Info("Proof round started", zap.Int64("height", height), zap.String("round", roundOrData))

	request := &storageTypes.QueryChallengesRequest{
		Provider: r.atlas.Wallet.GetAddress(),
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
		return err
	}

	r.scheduleChallengeProofs(ctx, height, roundOrData, res.Challenges)

	return nil
}

func (r *chainEventReceiver) scheduleChallengeProofs(ctx context.Context, roundStartHeight int64, round string, challenges []*storageTypes.StorageChallenge) {
	if len(challenges) == 0 {
		r.logger.Info("No challenges to prove for proof round",
			zap.Int64("height", roundStartHeight),
			zap.String("round", round))
		return
	}

	batches := make(map[int64][]*storageTypes.StorageChallenge)
	for i, challenge := range challenges {
		targetHeight := roundStartHeight + 1 + int64(i%challengeProofSpreadBlocks)
		if challenge.DeadlineHeight > 0 && targetHeight >= int64(challenge.DeadlineHeight) {
			targetHeight = int64(challenge.DeadlineHeight) - 1
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

	for _, challenge := range challenges {
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
	// [TBD]: this chain-sync is pretty useless. remove it?
	r.storage.RecordChainSync(time.Now().UTC())
	r.logger.Info("Proof window started", zap.Int64("height", height), zap.String("window", windowOrData))
	return nil
}
