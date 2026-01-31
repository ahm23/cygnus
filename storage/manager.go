package storage

import (
	"context"
	"cygnus/atlas"
	"cygnus/config"
	"cygnus/types"
	"encoding/hex"
	"fmt"
	"io"
	"math"
	"mime/multipart"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/wealdtech/go-merkletree/v2"
	"go.uber.org/zap"

	storageTypes "nebulix/x/storage/types"
)

type StorageManager struct {
	config    *config.Config
	logger    *zap.Logger
	atlas     *atlas.AtlasManager
	db        *PebbleStore
	mu        sync.RWMutex
	activeOps map[string]*sync.Mutex
	fileLocks sync.Map
}

func NewStorageManager(cfg *config.Config, logger *zap.Logger, atlas *atlas.AtlasManager) (*StorageManager, error) {
	// Create storage directory if it doesn't exist
	err := os.MkdirAll(cfg.DataDirectory, 0755)
	if err != nil {
		return nil, err
	}

	// Initialize PebbleDB store
	db, err := NewPebbleStore(cfg.DataDirectory, logger)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize pebble store: %w", err)
	}

	return &StorageManager{
		config:    cfg,
		logger:    logger,
		atlas:     atlas,
		db:        db,
		activeOps: make(map[string]*sync.Mutex),
	}, nil
}

/*
CreateFile saves the file on-disk, indexes it in the local file database, and submits a proof to Atlas Protocol.
*/
func (sm *StorageManager) CreateFile(ctx context.Context, fileId string, fileHeader *multipart.FileHeader) (bool, error) {
	// read entire file into memory
	// [TBD]: is there a better way of doing this? imagine loading a 32GB file into memory :p
	file, err := fileHeader.Open()
	if err != nil {
		return false, fmt.Errorf("failed to open uploaded file: %w", err)
	}
	defer file.Close()
	fileData, err := io.ReadAll(file)
	if err != nil {
		return false, fmt.Errorf("failed to read file: %w", err)
	}
	totalChunks := int(math.Ceil(float64(len(fileData)) / float64(types.ChunkSize)))
	defer sm.VerifyFileIntegrity(ctx, &fileId)

	// build merkletree
	fmt.Println("Building MerkleTree....")
	tree, err := sm.buildMerkleTree(ctx, fileData)
	if err != nil {
		return false, fmt.Errorf("failed to build merkle tree: %w", err)
	}
	merkleRoot := tree.Root()
	fmt.Println("Merkle Root:", hex.EncodeToString(merkleRoot))

	// generate proof of first chunk
	// [TBD]: make this a deterministic random chunk
	fmt.Println("Generating Proof....")
	merkleProof, err := sm.generateProof(tree, 0)
	if err != nil {
		return false, fmt.Errorf("failed to generate file proof")
	}

	// save the file
	fmt.Println("Saving File....")
	filePath := filepath.Join(sm.config.DataDirectory, fileId)
	if err := os.WriteFile(filePath, fileData, 0644); err != nil {
		return false, fmt.Errorf("failed to save file: %w", err)
	}

	// create file metadata & store in file database
	metadata := &types.FileMetadata{
		FID:         fileId,
		FileName:    fileHeader.Filename,
		Size:        fileHeader.Size,
		Chunks:      totalChunks,
		MerkleRoot:  hex.EncodeToString(merkleRoot),
		UploadedAt:  time.Now(),
		IsAvailable: true,
		// [TBD]: include MimeType? or should always be octet-stream on delivery?
	}
	fmt.Println("Storing Metadata....")
	if err := sm.storeMetadata(ctx, metadata); err != nil {
		return false, fmt.Errorf("failed to store metadata: %w", err)
	}

	// cache Merkle tree data for less intensive proof creation
	fmt.Println("Caching MerkleTree....")
	if err := sm.cacheMerkleTree(ctx, fileId, tree, len(fileData)); err != nil {
		return true, fmt.Errorf("failed to save merkle tree: %w", err)
	}

	// attest to having a file using an initial proof without challenge
	// TODO: move this logic outside this function
	fmt.Println("Proving File....")
	msg := &storageTypes.MsgProveFile{
		Creator:     sm.atlas.Wallet.GetAddress(),
		ChallengeId: "",
		Fid:         fileId,
		Data:        fileData[:1024],
		Hashes:      merkleProof.Hashes,
		Chunk:       merkleProof.Index,
	}
	_, err = sm.atlas.Wallet.BroadcastTxGrpc(0, false, msg)
	if err != nil {
		fmt.Println(err.Error())
		return false, fmt.Errorf("failed to post initial file proof")
	}

	fmt.Println("BINGO!")
	// success
	sm.logger.Info("File created successfully",
		zap.String("file_id", fileId),
		zap.Int64("size", fileHeader.Size),
		zap.Int("chunks", totalChunks),
		zap.String("merkle_root", hex.EncodeToString(merkleRoot)))

	return true, nil
}

/*
GetFile gets the metadata and a readonly file handle for the specified file.
*/
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

/*
DeleteFile deletes the file all over
*/
func (sm *StorageManager) DeleteFile(ctx context.Context, fileID string) error {
	// deleteMsg := &storageTypes.MsgRemoveProver{}
	// _, err := sm.atlas.Wallet.BroadcastTxGrpc(0, false, deleteMsg)
	// TODO: handle file not exist error (OK)
	// if err != nil {
	// 	fmt.Println(err.Error())
	// 	return false, fmt.Errorf("failed to post initial file proof")
	// }

	// Delete file on disk
	filePath := filepath.Join(sm.config.DataDirectory, fileID)
	if err := os.Remove(filePath); err != nil {
		sm.logger.Error("Failed to delete file", zap.String("file_id", fileID), zap.Error(err))
	}

	// Delete metadata from PebbleDB
	fileKey := FileKey(fileID)
	if err := sm.db.Delete(ctx, fileKey); err != nil {
		return fmt.Errorf("failed to delete metadata: %w", err)
	}

	// Delete cached merkle tree data
	merkleKey := MerkleKey(fileID)
	if err := sm.db.Delete(ctx, merkleKey); err != nil {
		sm.logger.Warn("Failed to delete merkle tree data", zap.String("file_id", fileID), zap.Error(err))
	}

	sm.logger.Info("File deleted", zap.String("file_id", fileID))
	return nil
}

/*
ListFiles returns a list of files. This is paginated.
*/
func (sm *StorageManager) ListFiles(ctx context.Context, page, pageSize int) (*types.FileListResponse, error) {
	// get all file keys
	fileKeys, err := sm.db.Keys(ctx, filePrefix)
	if err != nil {
		return nil, fmt.Errorf("failed to list files: %w", err)
	}

	// collect metadata
	var allFiles []types.FileMetadata
	for _, key := range fileKeys {
		fileID := strings.TrimPrefix(key, filePrefix)
		metadata, err := sm.getMetadata(ctx, fileID)
		if err == nil && metadata != nil {
			allFiles = append(allFiles, *metadata)
		}
	}
	total := len(allFiles)

	// calculate pagination
	start := (page - 1) * pageSize
	end := start + pageSize

	if start >= total {
		return &types.FileListResponse{
			Files:       []types.FileMetadata{},
			Total:       int64(total),
			Page:        page,
			PageSize:    pageSize,
			HasNext:     false,
			HasPrevious: page > 1,
		}, nil
	}
	if end > total {
		end = total
	}

	files := allFiles[start:end]

	return &types.FileListResponse{
		Files:       files,
		Total:       int64(total),
		Page:        page,
		PageSize:    pageSize,
		HasNext:     end < total,
		HasPrevious: page > 1,
	}, nil
}

/*
VerifyFileIntegrity validates a file's existence on-disk, on-chain, and in the local file database.
*/
func (sm *StorageManager) VerifyFileIntegrity(ctx context.Context, fileID *string) (bool, error) {
	filePath := filepath.Join(sm.config.DataDirectory, *fileID)

	if _, err := sm.atlas.QueryClients.Storage.File(ctx, &storageTypes.QueryFileRequest{Fid: *fileID}); err != nil {
		sm.logger.Warn("File integrity check failed for "+*fileID+". File does not exist on-chain.", zap.Error(err))
	} else if err := sm.db.GetJSON(ctx, FileKey(*fileID), nil); err != nil {
		sm.logger.Warn("File integrity check failed for "+*fileID+". File does not exist in database.", zap.Error(err))
	} else if _, err := os.Stat(filePath); os.IsNotExist(err) {
		sm.logger.Warn("File integrity check failed for "+*fileID+". File does not exist on disk.", zap.Error(err))
	} else {
		return true, nil
	}

	return false, sm.DeleteFile(ctx, *fileID)
}

/*
Some random AI generated bullshit
*/
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

	// Get provider info from PebbleDB
	ctx := context.Background()
	uptime := 0.0
	peers := 0

	uptimeKey := ProviderKey("uptime")
	if uptimeData, err := sm.db.Get(ctx, uptimeKey); err == nil {
		if val, err := strconv.ParseFloat(string(uptimeData), 64); err == nil {
			uptime = val
		}
	}

	peersKey := ProviderKey("peers")
	if peersData, err := sm.db.Get(ctx, peersKey); err == nil {
		if val, err := strconv.Atoi(string(peersData)); err == nil {
			peers = val
		}
	}

	walletAddr := "0x..."
	if sm.atlas != nil && sm.atlas.Wallet != nil {
		walletAddr = sm.atlas.Wallet.GetAddress()
	}

	return &types.ProviderStatus{
		ProviderID:   "provider_" + uuid.New().String()[:8],
		Wallet:       walletAddr,
		Uptime:       uptime,
		TotalStorage: sm.config.TotalSpace,
		UsedStorage:  totalSize,
		FilesCount:   fileCount,
		IsOnline:     true,
		LastSync:     time.Now(),
		Peers:        peers,
		Version:      "1.0.0",
	}, nil
}

// Helper methods for PebbleDB
func (sm *StorageManager) storeMetadata(ctx context.Context, metadata *types.FileMetadata) error {
	key := FileKey(metadata.FID)

	// Store file metadata
	if err := sm.db.SetJSON(ctx, key, metadata); err != nil {
		return err
	}

	return nil
}

func (sm *StorageManager) getMetadata(ctx context.Context, fileID string) (*types.FileMetadata, error) {
	key := FileKey(fileID)

	var metadata types.FileMetadata
	if err := sm.db.GetJSON(ctx, key, &metadata); err != nil {
		return nil, err
	}

	return &metadata, nil
}

// Close closes the PebbleDB connection
func (sm *StorageManager) Close() error {
	if sm.db != nil {
		return sm.db.Close()
	}
	return nil
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
