package core

import (
	"context"
	"fmt"

	storageTypes "nebulix/x/storage/types"

	"cygnus/atlas"
	"cygnus/storage"
)

// chainEventReceiver bridges atlas blockchain events to the storage manager.
type chainEventReceiver struct {
	atlas   *atlas.AtlasManager
	storage *storage.StorageManager
}

// Ensure chainEventReceiver implements atlas.ChainEventReceiver.
var _ atlas.ChainEventReceiver = (*chainEventReceiver)(nil)

func (r *chainEventReceiver) OnFileDeleted(ctx context.Context, fileID string) error {
	return r.storage.DeleteFile(ctx, fileID)
}

func (r *chainEventReceiver) OnStartProofRound(ctx context.Context, height int64, roundOrData string) error {
	request := &storageTypes.QueryChallengesRequest{
		Provider: r.atlas.Wallet.GetAddress(),
	}
	cl := r.atlas.QueryClients.Storage

	res, _ := cl.Challenges(context.Background(), request)
	// [TODO]: implement retry logic
	// [TODO]: implement endpoint swapping
	fmt.Println(res.Challenges)
	for _, challenge := range res.Challenges {
		err := r.storage.ProveFile(ctx, challenge.FileId, int64(challenge.ChunkIndex))
		fmt.Println("err:", err)
	}

	return nil
}

func (r *chainEventReceiver) OnStartProofWindow(ctx context.Context, height int64, windowOrData string) error {
	return nil
}
