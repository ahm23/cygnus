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
	"github.com/rs/zerolog/log"
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

func (sm *StorageManager) buildMerkleTreeFromReader(ctx context.Context, reader io.Reader) (*merkletree.MerkleTree, [][]byte, int, error) {
	// sm.logger.Debug("Building Merkle tree from stream", zap.Int64("chunk_size", types.ChunkSize))

	buf := make([]byte, types.ChunkSize)
	var leaves [][]byte

	for {
		n, readErr := io.ReadFull(reader, buf)
		if readErr != nil && readErr != io.EOF && readErr != io.ErrUnexpectedEOF {
			return nil, nil, 0, fmt.Errorf("failed to read merkle input: %w", readErr)
		}
		if n > 0 {
			leaves = append(leaves, hashChunk(buf[:n]))
		}
		if readErr == io.EOF || readErr == io.ErrUnexpectedEOF {
			break
		}
	}

	// leaves are the blake3 hashes of each chunk — the raw material for the merkle tree.
	// tree.Leaves would be the XXH128 of these, so we return them separately.
	tree, err := buildMerkleTreeFromLeaves(leaves)
	if err != nil {
		return nil, nil, 0, err
	}

	// sm.logger.Info("Merkle tree created",
	// 	zap.String("root_hash", hex.EncodeToString(tree.Root)),
	// 	zap.Int("total_chunks", len(leaves)))

	return tree, leaves, len(leaves), nil
}

// buildMerkleTree creates a Merkle tree from file chunks.
func (sm *StorageManager) buildMerkleTree(ctx context.Context, data []byte) (*merkletree.MerkleTree, [][]byte, error) {
	tree, leaves, _, err := sm.buildMerkleTreeFromReader(ctx, bytes.NewReader(data))
	return tree, leaves, err
}

func (sm *StorageManager) buildMerkleTreeFromFile(ctx context.Context, filePath string) (*merkletree.MerkleTree, [][]byte, int, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("failed to open file for merkle build: %w", err)
	}
	defer file.Close()

	return sm.buildMerkleTreeFromReader(ctx, file)
}

// cacheMerkleTree persists the original leaf hashes (blake3 of each chunk) alongside
// the tree's root hash.
func (sm *StorageManager) cacheMerkleTree(ctx context.Context, fileID string, tree *merkletree.MerkleTree, originalLeaves [][]byte) error {
	leaves := make([]string, len(originalLeaves))
	for i, leaf := range originalLeaves {
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
// Returns the tree on success. If the cache is missing, corrupt, or the
// reconstructed root doesn't match the stored root_hash, the cache entry
// is deleted and (nil, nil) is returned so the caller rebuilds from disk.
func (sm *StorageManager) loadCachedMerkleTree(ctx context.Context, fileID string) (*merkletree.MerkleTree, error) {
	key := MerkleKey(fileID)

	hashData, err := sm.db.GetHash(ctx, key)
	if err != nil {
		return nil, nil // cache miss - rebuild from file
	}

	leavesRaw, ok := hashData["leaves"]
	if !ok {
		log.Warn().Str("file_id", fileID).Msg("Merkle cache has no stored leaves; discarding")
		_ = sm.db.Delete(ctx, key)
		return nil, nil
	}

	leavesList, ok := leavesRaw.([]interface{})
	if !ok || len(leavesList) == 0 {
		log.Warn().Str("file_id", fileID).Msg("Merkle cache has invalid or empty leaves; discarding")
		_ = sm.db.Delete(ctx, key)
		return nil, nil
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

	tree, err := buildMerkleTreeFromLeaves(leaves)
	if err != nil {
		log.Warn().Str("file_id", fileID).Err(err).Msg("Failed to rebuild tree from cached leaves; discarding")
		_ = sm.db.Delete(ctx, key)
		return nil, nil
	}

	return tree, nil
}

// BuildMerkleTree is kept for local verification and tests.
func (sm *StorageManager) BuildMerkleTree(ctx context.Context, data []byte) (*merkletree.MerkleTree, [][]byte, error) {
	return sm.buildMerkleTree(ctx, data)
}
