package core

import (
	"context"
	"time"

	storageTypes "nebulix/x/storage/types"

	"cygnus/atlas"
	"cygnus/storage"

	"go.uber.org/zap"
)

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

	for _, challenge := range res.Challenges {
		r.storage.RecordChainSync(time.Now().UTC())
		err := r.storage.ProveFile(ctx, challenge.FileId, challenge.ChallengeId, int64(challenge.ChunkIndex))
		if err != nil {
			r.logger.Error("Failed to prove challenge",
				zap.String("file_id", challenge.FileId),
				zap.String("challenge_id", challenge.ChallengeId),
				zap.Int64("chunk", int64(challenge.ChunkIndex)),
				zap.Error(err))
			continue
		}
	}

	return nil
}

func (r *chainEventReceiver) OnStartProofWindow(ctx context.Context, height int64, windowOrData string) error {
	// [TBD]: this chain-sync is pretty useless. remove it?
	r.storage.RecordChainSync(time.Now().UTC())
	r.logger.Info("Proof window started", zap.Int64("height", height), zap.String("window", windowOrData))
	return nil
}
