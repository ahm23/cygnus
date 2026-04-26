package storage

import (
	"bytes"
	"context"
	"cygnus/types"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"time"

	merkletree "github.com/ahm23/go-merkletree-xxh"
	"github.com/zeebo/blake3"
	"go.uber.org/zap"
)

func hashChunk(chunk []byte) []byte {
	hasher := blake3.New()
	_, _ = hasher.Write(chunk)
	return hasher.Sum(nil)
}

func buildMerkleTreeFromLeaves(leaves [][]byte) (*merkletree.MerkleTree, error) {
	if len(leaves) == 0 {
		return nil, fmt.Errorf("cannot build merkle tree with no leaves")
	}

	treeLeaves := make([][]byte, len(leaves))
	for i, leaf := range leaves {
		treeLeaves[i] = append([]byte(nil), leaf...)
	}

	// fmt.Println("Leaves:", leaves)

	tree, err := merkletree.New(
		&merkletree.Config{XXH128: true},
		treeLeaves,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create merkle tree: %w", err)
	}
	// fmt.Println(tree.Leaves)
	// fmt.Println(tree.Root)
	// fmt.Println(tree)

	return tree, nil
}

func (sm *StorageManager) buildMerkleTreeFromReader(ctx context.Context, reader io.Reader) (*merkletree.MerkleTree, int, error) {
	sm.logger.Info("Building Merkle tree from stream", zap.Int64("chunk_size", types.ChunkSize))

	buf := make([]byte, types.ChunkSize)
	var leaves [][]byte

	for {
		n, readErr := io.ReadFull(reader, buf)
		if readErr != nil && readErr != io.EOF && readErr != io.ErrUnexpectedEOF {
			return nil, 0, fmt.Errorf("failed to read merkle input: %w", readErr)
		}
		if n > 0 {
			leaves = append(leaves, hashChunk(buf[:n]))
		}
		if readErr == io.EOF || readErr == io.ErrUnexpectedEOF {
			break
		}
	}

	tree, err := buildMerkleTreeFromLeaves(leaves)
	if err != nil {
		return nil, 0, err
	}

	sm.logger.Info("Merkle tree created",
		zap.String("root_hash", hex.EncodeToString(tree.Root)),
		zap.Int("total_chunks", len(leaves)))

	return tree, len(leaves), nil
}

// buildMerkleTree creates a Merkle tree from file chunks.
func (sm *StorageManager) buildMerkleTree(ctx context.Context, data []byte) (*merkletree.MerkleTree, error) {
	tree, _, err := sm.buildMerkleTreeFromReader(ctx, bytes.NewReader(data))
	return tree, err
}

func (sm *StorageManager) buildMerkleTreeFromFile(ctx context.Context, filePath string) (*merkletree.MerkleTree, int, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to open file for merkle build: %w", err)
	}
	defer file.Close()

	return sm.buildMerkleTreeFromReader(ctx, file)
}

func (sm *StorageManager) cacheMerkleTree(ctx context.Context, fileID string, tree *merkletree.MerkleTree, fileSize int64, chunks int) error {
	treeData := map[string]interface{}{
		"root_hash":   hex.EncodeToString(tree.Root),
		"file_size":   fileSize,
		"chunk_count": chunks,
		"timestamp":   time.Now().UTC(),
	}

	key := MerkleKey(fileID)
	return sm.db.SetHash(ctx, key, treeData)
}

// BuildMerkleTree is kept for local verification and tests.
func (sm *StorageManager) BuildMerkleTree(ctx context.Context, data []byte) (*merkletree.MerkleTree, error) {
	return sm.buildMerkleTree(ctx, data)
}
