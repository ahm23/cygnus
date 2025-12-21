package storage

import (
	"context"
	"cygnus/atlas"
	"cygnus/config"
	"cygnus/types"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"mime/multipart"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/wealdtech/go-merkletree/v2"
	"go.uber.org/zap"

	storageTypes "nebulix/x/storage/types"
)

type StorageManager struct {
	config    *config.Config
	logger    *zap.Logger
	atlas     *atlas.AtlasManager
	redis     *redis.Client
	mu        sync.RWMutex
	activeOps map[string]*sync.Mutex
	fileLocks sync.Map
}

func NewStorageManager(cfg *config.Config, logger *zap.Logger, atlas *atlas.AtlasManager) (*StorageManager, error) {
	// Create storage directory if it doesn't exist
	err := os.MkdirAll(cfg.DataDirectory, 0755)

	return &StorageManager{
		config:    cfg,
		logger:    logger,
		atlas:     atlas,
		activeOps: make(map[string]*sync.Mutex),
	}, err
}

func (sm *StorageManager) UploadFile(ctx context.Context, fileId string, fileHeader *multipart.FileHeader) (*types.FileMetadata, error) {
	// read entire file into memory
	// [TBD]: is there a better way of doing this? imagine loading a 128GB file into memory :p
	file, err := fileHeader.Open()
	if err != nil {
		return nil, fmt.Errorf("failed to open uploaded file: %w", err)
	}
	defer file.Close()

	fileData, err := io.ReadAll(file)
	if err != nil {
		return nil, fmt.Errorf("failed to read file: %w", err)
	}
	totalChunks := int(math.Ceil(float64(len(fileData)) / float64(types.ChunkSize)))

	// build merkletree
	tree, err := sm.buildMerkleTree(ctx, fileData)
	if err != nil {
		return nil, fmt.Errorf("failed to build merkle tree: %w", err)
	}
	merkleRoot := tree.Root()

	// generate proof of first chunk
	// [TODO]: make this a deterministic random chunk
	merkleProof, err := sm.generateProof(tree, 0)
	if err != nil {
		return nil, fmt.Errorf("failed to generate file proof")
	}

	// save the file
	filePath := filepath.Join(sm.config.DataDirectory, fileId)
	if err := os.WriteFile(filePath, fileData, 0644); err != nil {
		return nil, fmt.Errorf("failed to save file: %w", err)
	}

	// attest to having a file using an initial proof without challenge
	msg := &storageTypes.MsgProveFile{
		Creator:     sm.atlas.Wallet.GetAddress(),
		ChallengeId: "",
		FileId:      fileId,
		Data:        fileData[:1024],
		Hashes:      merkleProof.Hashes,
		Chunk:       merkleProof.Index,
	}
	_, err = sm.atlas.Wallet.BroadcastTxGrpc(0, false, msg)
	if err != nil {
		// [TODO]: cleanup, delete saved file
		fmt.Println(err.Error())
		return nil, fmt.Errorf("failed to post initial file proof")
	}

	// cache Merkle tree data for less intensive proof creation
	// if err := sm.cacheMerkleTree(ctx, fileId, tree, len(fileData)); err != nil {
	// 	return nil, fmt.Errorf("failed to save merkle tree: %w", err)
	// }

	// create file metadata & store in Redis
	metadata := &types.FileMetadata{
		FID:         fileId,
		FileName:    fileHeader.Filename,
		Size:        fileHeader.Size,
		Chunks:      totalChunks,
		MerkleRoot:  hex.EncodeToString(merkleRoot),
		UploadedAt:  time.Now(), // [TBD]: should this be representative of the inital request time? merkle tree can take a few seconds for huge files
		IsAvailable: true,
		//MimeType:    fileHeader.Header.Get("Content-Type"), // [TBD]: keep this? or should always be octet-stream on delivery
	}

	// if err := sm.storeMetadata(ctx, metadata); err != nil {
	// 	return nil, fmt.Errorf("failed to store metadata: %w", err)
	// }

	sm.logger.Info("File uploaded successfully with Merkle tree",
		zap.String("file_id", fileId),
		zap.Int64("size", fileHeader.Size),
		zap.Int("chunks", totalChunks),
		zap.String("merkle_root", hex.EncodeToString(merkleRoot)))

	return metadata, nil
}

func (sm *StorageManager) generateProof(tree *merkletree.MerkleTree, index int64) (*merkletree.Proof, error) {
	if tree == nil {
		// [TODO]: load tree from cache or create new tree from file
	}

	proof, err := tree.GenerateProof(tree.Data[index], 0)
	if err != nil {
		return nil, err
	}

	return proof, nil
}

func (sm *StorageManager) GetFile(ctx context.Context, fileID string) (*types.FileMetadata, io.ReadCloser, error) {
	metadata, err := sm.getMetadata(ctx, fileID)
	if err != nil {
		return nil, nil, err
	}

	if !metadata.IsAvailable {
		return nil, nil, fmt.Errorf("file not available")
	}

	filePath := filepath.Join(sm.config.DataDirectory, fileID)
	file, err := os.Open(filePath)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to open file: %w", err)
	}

	return metadata, file, nil
}

func (sm *StorageManager) ListFiles(ctx context.Context, owner string, page, pageSize int) (*types.FileListResponse, error) {
	// This would typically query a database
	// For Redis implementation:
	pattern := fmt.Sprintf("file:*:owner:%s", owner)
	keys, err := sm.redis.Keys(ctx, pattern).Result()
	if err != nil {
		return nil, err
	}

	// Calculate pagination
	start := (page - 1) * pageSize
	end := start + pageSize

	if start >= len(keys) {
		return &types.FileListResponse{
			Files:       []types.FileMetadata{},
			Total:       int64(len(keys)),
			Page:        page,
			PageSize:    pageSize,
			HasNext:     false,
			HasPrevious: page > 1,
		}, nil
	}

	if end > len(keys) {
		end = len(keys)
	}

	var files []types.FileMetadata
	for i := start; i < end; i++ {
		metadata, err := sm.getMetadata(ctx, keys[i])
		if err == nil && metadata != nil {
			files = append(files, *metadata)
		}
	}

	return &types.FileListResponse{
		Files:       files,
		Total:       int64(len(keys)),
		Page:        page,
		PageSize:    pageSize,
		HasNext:     end < len(keys),
		HasPrevious: page > 1,
	}, nil
}

func (sm *StorageManager) DeleteFile(ctx context.Context, fileID, owner string) error {
	metadata, err := sm.getMetadata(ctx, fileID)
	if err != nil {
		return err
	}

	if metadata.Owner != owner {
		return fmt.Errorf("unauthorized to delete this file")
	}

	// Delete physical file
	filePath := filepath.Join(sm.config.DataDirectory, fileID)
	if err := os.Remove(filePath); err != nil {
		sm.logger.Error("Failed to delete file", zap.String("file_id", fileID), zap.Error(err))
	}

	// Delete metadata from Redis
	key := fmt.Sprintf("file:%s", fileID)
	if err := sm.redis.Del(ctx, key).Err(); err != nil {
		return fmt.Errorf("failed to delete metadata: %w", err)
	}

	sm.logger.Info("File deleted", zap.String("file_id", fileID))
	return nil
}

func (sm *StorageManager) GetStatus() (*types.ProviderStatus, error) {
	// Calculate storage usage
	var totalSize int64
	var fileCount int64

	// Walk through storage directory
	err := filepath.Walk(sm.config.DataDirectory, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			totalSize += info.Size()
			fileCount++
		}
		return nil
	})

	if err != nil {
		return nil, err
	}

	// Get provider info from Redis
	ctx := context.Background()
	uptime, _ := sm.redis.Get(ctx, "provider:uptime").Float64()
	peers, _ := sm.redis.Get(ctx, "provider:peers").Int()

	return &types.ProviderStatus{
		ProviderID:   "provider_" + uuid.New().String()[:8],
		Wallet:       "0x...", // Should come from your wallet integration
		Uptime:       uptime,
		TotalStorage: int64(100 * 1024 * 1024 * 1024), // 100GB example
		UsedStorage:  totalSize,
		FilesCount:   fileCount,
		IsOnline:     true,
		LastSync:     time.Now(),
		Peers:        peers,
		Version:      "1.0.0",
	}, nil
}

// Helper methods for Redis
func (sm *StorageManager) storeMetadata(ctx context.Context, metadata *types.FileMetadata) error {
	key := fmt.Sprintf("file:%s", metadata.FID)

	data, err := json.Marshal(metadata)
	if err != nil {
		return err
	}

	return sm.redis.Set(ctx, key, data, 0).Err()
}

func (sm *StorageManager) getMetadata(ctx context.Context, fileID string) (*types.FileMetadata, error) {
	key := fmt.Sprintf("file:%s", fileID)
	data, err := sm.redis.Get(ctx, key).Bytes()
	if err != nil {
		return nil, err
	}

	var metadata types.FileMetadata
	if err := json.Unmarshal(data, &metadata); err != nil {
		return nil, err
	}

	return &metadata, nil
}
