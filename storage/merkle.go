package storage

import (
	"bytes"
	"context"
	"cygnus/types"
	"encoding/hex"
	"fmt"
	"io"
	"os"

	merkletree "github.com/ahm23/go-merkletree-xxh"
	"github.com/zeebo/blake3"
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
	// sm.logger.Debug("Building Merkle tree from stream", zap.Int64("chunk_size", types.ChunkSize))

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

	// sm.logger.Info("Merkle tree created",
	// 	zap.String("root_hash", hex.EncodeToString(tree.Root)),
	// 	zap.Int("total_chunks", len(leaves)))

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

// cacheMerkleTree persists a merkle tree's leaf hashes and root hash.
func (sm *StorageManager) cacheMerkleTree(ctx context.Context, fileID string, tree *merkletree.MerkleTree) error {
	leaves := make([]string, len(tree.Leaves))
	for i, leaf := range tree.Leaves {
		leaves[i] = hex.EncodeToString(leaf)
	}

	treeData := map[string]interface{}{
		"root_hash": hex.EncodeToString(tree.Root),
		"leaves":    leaves,
	}

	key := MerkleKey(fileID)
	return sm.db.SetHash(ctx, key, treeData)
}

// loadCachedMerkleTree reconstructs a Merkle tree from cached leaf hashes.
// Returns an error if the cache entry is missing, has no leaves, or decoding fails.
func (sm *StorageManager) loadCachedMerkleTree(ctx context.Context, fileID string) (*merkletree.MerkleTree, error) {
	key := MerkleKey(fileID)

	hashData, err := sm.db.GetHash(ctx, key)
	if err != nil {
		return nil, err
	}

	leavesRaw, ok := hashData["leaves"]
	if !ok {
		return nil, fmt.Errorf("merkle cache for %s has no stored leaves", fileID)
	}

	leavesList, ok := leavesRaw.([]interface{})
	if !ok || len(leavesList) == 0 {
		return nil, fmt.Errorf("merkle cache for %s has invalid or empty leaves", fileID)
	}

	leaves := make([][]byte, len(leavesList))
	for i, l := range leavesList {
		leafStr, ok := l.(string)
		if !ok {
			return nil, fmt.Errorf("merkle cache for %s: leaf %d is not a string", fileID, i)
		}
		leafBytes, err := hex.DecodeString(leafStr)
		if err != nil {
			return nil, fmt.Errorf("merkle cache for %s: leaf %d decode error: %w", fileID, i, err)
		}
		leaves[i] = leafBytes
	}

	return buildMerkleTreeFromLeaves(leaves)
}

// BuildMerkleTree is kept for local verification and tests.
func (sm *StorageManager) BuildMerkleTree(ctx context.Context, data []byte) (*merkletree.MerkleTree, error) {
	return sm.buildMerkleTree(ctx, data)
}
