package storage

import (
	"context"
	"cygnus/types"
	"encoding/hex"
	"fmt"
	"math"
	"time"

	"github.com/wealdtech/go-merkletree/v2"
	treeblake3 "github.com/wealdtech/go-merkletree/v2/blake3"
	"github.com/zeebo/blake3"
	"go.uber.org/zap"
)

// buildMerkleTree creates a Merkle tree from file chunks
func (sm *StorageManager) buildMerkleTree(ctx context.Context, data []byte) (*merkletree.MerkleTree, error) {
	sm.logger.Info("Building Merkle tree",
		zap.Int64("total_size", int64(len(data))),
		zap.Int64("chunk_size", types.ChunkSize))

	// Calculate number of chunks
	totalChunks := int(math.Ceil(float64(len(data)) / float64(types.ChunkSize)))

	// prepare merkle leaves
	var leaves [][]byte
	hasher := blake3.New()

	for i := 0; i < totalChunks; i++ {
		start := i * int(types.ChunkSize)
		end := start + int(types.ChunkSize)
		if end > len(data) {
			end = len(data)
		}
		chunk := data[start:end]

		// compute chunk hash for leaf
		hasher.Reset()
		hasher.Write(chunk)
		chunkHash := hasher.Sum(nil)

		fmt.Println("leaf:", hex.EncodeToString(chunkHash))
		fmt.Println("len:", len(chunk))
		// fmt.Println(chunk)
		leaves = append(leaves, chunkHash)
	}

	// create merkle tree
	tree, err := merkletree.NewUsing(
		leaves,
		treeblake3.New256(),
		false,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create merkle tree: %w", err)
	}
	fmt.Println(tree.Nodes)
	fmt.Println(tree.Sorted)
	fmt.Println(tree.String())
	merkleRoot := tree.Root()

	sm.logger.Debug("Merkle tree created",
		zap.String("root_hash", hex.EncodeToString(merkleRoot)),
		zap.Int("total_chunks", totalChunks))

	return tree, nil
}

//		TreeDepth:   int(math.Ceil(math.Log2(float64(totalChunks)))),

func (sm *StorageManager) cacheMerkleTree(ctx context.Context, fileID string, tree *merkletree.MerkleTree, fileSize int) error {
	treeData := map[string]interface{}{
		"root_hash": hex.EncodeToString(tree.Root()),
		"file_size": fileSize,
		"timestamp": time.Now().UTC(),
	}

	// [TODO]: cache leaves for reconstruction,
	// nodes instead maybe? idk.. performance impact to be analyzed
	key := MerkleKey(fileID)
	return sm.db.SetHash(ctx, key, treeData)
}

// [TODO]: remove this when done with CLI tests
func (sm *StorageManager) BuildMerkleTree(ctx context.Context, data []byte) (*merkletree.MerkleTree, error) {
	return sm.buildMerkleTree(ctx, data)
}
