package storage

import (
	"context"
	"cygnus/atlas"
	"cygnus/config"
	"cygnus/types"
	"encoding/hex"
	"fmt"
	"io"
	"mime/multipart"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	merkletree "github.com/ahm23/go-merkletree-xxh"
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

	statusMu      sync.RWMutex
	lastProofAt   time.Time
	lastChainSync time.Time
	lastChallenge time.Time
}

func NewStorageManager(cfg *config.Config, logger *zap.Logger, atlas *atlas.AtlasManager) (*StorageManager, error) {
	dataDir := os.ExpandEnv(cfg.DataDirectory)
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, err
	}

	db, err := NewPebbleStore(dataDir, logger)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize pebble store: %w", err)
	}

	cfg.DataDirectory = dataDir

	return &StorageManager{
		config:    cfg,
		logger:    logger,
		atlas:     atlas,
		db:        db,
		activeOps: make(map[string]*sync.Mutex),
	}, nil
}

func validateFileID(fileID string) error {
	if strings.TrimSpace(fileID) == "" {
		return fmt.Errorf("file id is required")
	}
	if fileID != filepath.Base(fileID) || strings.Contains(fileID, "..") {
		return fmt.Errorf("invalid file id")
	}
	return nil
}

func (sm *StorageManager) filePath(fileID string) (string, error) {
	if err := validateFileID(fileID); err != nil {
		return "", err
	}
	return filepath.Join(sm.config.DataDirectory, fileID), nil
}

func (sm *StorageManager) currentUsage(ctx context.Context) (int64, int64, error) {
	var totalSize int64
	var fileCount int64

	err := filepath.Walk(sm.config.DataDirectory, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		relPath, relErr := filepath.Rel(sm.config.DataDirectory, path)
		if relErr != nil {
			return relErr
		}

		if info.IsDir() {
			if relPath == "index" {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasPrefix(relPath, "index"+string(os.PathSeparator)) {
			return nil
		}
		if strings.HasSuffix(path, ".upload") || strings.Contains(filepath.Base(path), ".upload-") {
			return nil
		}
		totalSize += info.Size()
		fileCount++
		return nil
	})
	if err != nil {
		return 0, 0, err
	}

	return totalSize, fileCount, nil
}

func (sm *StorageManager) HasCapacityFor(ctx context.Context, incomingSize int64) (bool, int64, error) {
	used, _, err := sm.currentUsage(ctx)
	if err != nil {
		return false, 0, err
	}

	return used+incomingSize <= sm.config.TotalSpace, used, nil
}

func (sm *StorageManager) RecordChainSync(at time.Time) {
	sm.statusMu.Lock()
	sm.lastChainSync = at
	sm.statusMu.Unlock()
}

func (sm *StorageManager) recordChallengeActivity(at time.Time) {
	sm.statusMu.Lock()
	sm.lastChallenge = at
	sm.statusMu.Unlock()
}

func (sm *StorageManager) recordProofActivity(at time.Time) {
	sm.statusMu.Lock()
	sm.lastProofAt = at
	sm.statusMu.Unlock()
}

func (sm *StorageManager) submitProof(ctx context.Context, fileID, challengeID string, proof *merkletree.Proof, chunkData []byte) error {
	if sm.atlas == nil || sm.atlas.Wallet == nil {
		return fmt.Errorf("wallet not connected")
	}

	msg := &storageTypes.MsgProveFile{
		Creator:     sm.atlas.Wallet.GetAddress(),
		ChallengeId: challengeID,
		Fid:         fileID,
		Data:        chunkData,
		Hashes:      proof.Siblings,
		Chunk:       proof.Index,
	}

	if _, err := sm.atlas.Wallet.BroadcastTxGrpc(0, false, msg); err != nil {
		return err
	}

	sm.recordProofActivity(time.Now().UTC())
	return nil
}

// CreateFile saves the file on-disk, indexes it locally, and submits an initial proof.
func (sm *StorageManager) CreateFile(ctx context.Context, fileID string, fileHeader *multipart.FileHeader) (*types.FileMetadata, error) {
	if err := validateFileID(fileID); err != nil {
		return nil, err
	}

	if ok, _, err := sm.HasCapacityFor(ctx, fileHeader.Size); err != nil {
		return nil, err
	} else if !ok {
		return nil, fmt.Errorf("insufficient provider capacity")
	}

	filePath, err := sm.filePath(fileID)
	if err != nil {
		return nil, err
	}

	if _, err := os.Stat(filePath); err == nil {
		return nil, fmt.Errorf("file already exists locally")
	}

	file, err := fileHeader.Open()
	if err != nil {
		return nil, fmt.Errorf("failed to open uploaded file: %w", err)
	}
	defer file.Close()

	tempPath := filePath + ".upload-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	ingest, err := streamFileToDiskAndCollectLeaves(file, tempPath)
	if err != nil {
		return nil, err
	}

	tree, err := buildMerkleTreeFromLeaves(ingest.Leaves)
	if err != nil {
		_ = os.Remove(tempPath)
		return nil, err
	}
	sm.logger.Info("Merkle tree created", zap.String("root_hash", hex.EncodeToString(tree.Root)))
	fmt.Println(tree.Leaves)

	if err := os.Rename(tempPath, filePath); err != nil {
		_ = os.Remove(tempPath)
		return nil, fmt.Errorf("failed to move uploaded file into place: %w", err)
	}

	owner := ""
	if sm.atlas != nil && sm.atlas.Wallet != nil {
		owner = sm.atlas.Wallet.GetAddress()
	}

	metadata := &types.FileMetadata{
		FID:         fileID,
		FileName:    fileHeader.Filename,
		Size:        ingest.Size,
		Chunks:      ingest.Chunks,
		MerkleRoot:  hex.EncodeToString(tree.Root),
		UploadedAt:  time.Now().UTC(),
		Owner:       owner,
		IsAvailable: true,
	}

	if err := sm.storeMetadata(ctx, metadata); err != nil {
		_ = os.Remove(filePath)
		return nil, fmt.Errorf("failed to store metadata: %w", err)
	}

	if err := sm.cacheMerkleTree(ctx, fileID, tree, ingest.Size, ingest.Chunks); err != nil {
		_ = sm.DeleteFile(ctx, fileID)
		return nil, fmt.Errorf("failed to save merkle metadata: %w", err)
	}

	proof, err := sm.generateProof(tree, 0)
	if err != nil {
		_ = sm.DeleteFile(ctx, fileID)
		return nil, fmt.Errorf("failed to generate initial proof: %w", err)
	}

	if err := sm.submitProof(ctx, fileID, "", proof, ingest.FirstChunk); err != nil {
		_ = sm.DeleteFile(ctx, fileID)
		return nil, fmt.Errorf("failed to post initial file proof: %w", err)
	}

	sm.logger.Info("File created successfully",
		zap.String("file_id", fileID),
		zap.Int64("size", metadata.Size),
		zap.Int("chunks", metadata.Chunks),
		zap.String("merkle_root", metadata.MerkleRoot))

	return metadata, nil
}

// GetFile gets the metadata and a readonly file handle for the specified file.
func (sm *StorageManager) GetFile(ctx context.Context, fileID string) (*types.FileMetadata, io.ReadCloser, error) {
	metadata, err := sm.getMetadata(ctx, fileID)
	if err != nil {
		return nil, nil, err
	}
	if !metadata.IsAvailable {
		return nil, nil, fmt.Errorf("file not available")
	}

	filePath, err := sm.filePath(fileID)
	if err != nil {
		return nil, nil, err
	}

	file, err := os.Open(filePath)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to open file: %w", err)
	}

	return metadata, file, nil
}

// DeleteFile removes the local file, metadata, and cached merkle state.
func (sm *StorageManager) DeleteFile(ctx context.Context, fileID string) error {
	filePath, err := sm.filePath(fileID)
	if err != nil {
		return err
	}

	if err := os.Remove(filePath); err != nil && !os.IsNotExist(err) {
		sm.logger.Error("Failed to delete file", zap.String("file_id", fileID), zap.Error(err))
	}

	fileKey := FileKey(fileID)
	if err := sm.db.Delete(ctx, fileKey); err != nil {
		return fmt.Errorf("failed to delete metadata: %w", err)
	}

	merkleKey := MerkleKey(fileID)
	if err := sm.db.Delete(ctx, merkleKey); err != nil {
		sm.logger.Warn("Failed to delete merkle tree data", zap.String("file_id", fileID), zap.Error(err))
	}

	sm.logger.Info("File deleted", zap.String("file_id", fileID))
	return nil
}

func (sm *StorageManager) ProveFile(ctx context.Context, fileID string, challengeID string, chunk int64) error {
	filePath, err := sm.filePath(fileID)
	if err != nil {
		return err
	}
	if chunk < 0 {
		return fmt.Errorf("chunk index must be non-negative")
	}

	tree, _, err := sm.buildMerkleTreeFromFile(ctx, filePath)
	if err != nil {
		return fmt.Errorf("failed to rebuild merkle tree for %s: %w", fileID, err)
	}

	proof, err := sm.generateProof(tree, chunk)
	if err != nil {
		return fmt.Errorf("failed to generate proof for %s: %w", fileID, err)
	}

	chunkData, err := getFileSegment(filePath, chunk*types.ChunkSize, (chunk+1)*types.ChunkSize)
	if err != nil {
		return fmt.Errorf("failed to read chunk data for %s: %w", fileID, err)
	}

	if err := sm.submitProof(ctx, fileID, challengeID, proof, chunkData); err != nil {
		return err
	}

	sm.logger.Info("Challenge proof submitted",
		zap.String("file_id", fileID),
		zap.String("challenge_id", challengeID),
		zap.Int64("chunk", chunk),
		zap.String("merkle_root", hex.EncodeToString(tree.Root)))

	return nil
}

// ListFiles returns a paginated list of locally stored files.
func (sm *StorageManager) ListFiles(ctx context.Context, page, pageSize int) (*types.FileListResponse, error) {
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = 25
	}

	fileKeys, err := sm.db.Keys(ctx, filePrefix)
	if err != nil {
		return nil, fmt.Errorf("failed to list files: %w", err)
	}

	var allFiles []types.FileMetadata
	for _, key := range fileKeys {
		fileID := strings.TrimPrefix(key, filePrefix)
		metadata, err := sm.getMetadata(ctx, fileID)
		if err == nil && metadata != nil {
			allFiles = append(allFiles, *metadata)
		}
	}

	sort.Slice(allFiles, func(i, j int) bool {
		if allFiles[i].UploadedAt.Equal(allFiles[j].UploadedAt) {
			return allFiles[i].FID < allFiles[j].FID
		}
		return allFiles[i].UploadedAt.After(allFiles[j].UploadedAt)
	})

	total := len(allFiles)
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

	return &types.FileListResponse{
		Files:       allFiles[start:end],
		Total:       int64(total),
		Page:        page,
		PageSize:    pageSize,
		HasNext:     end < total,
		HasPrevious: page > 1,
	}, nil
}

// VerifyFileIntegrity validates local file availability and warns on chain lookup failures.
func (sm *StorageManager) VerifyFileIntegrity(ctx context.Context, fileID *string) (bool, error) {
	if fileID == nil {
		return false, fmt.Errorf("file id is required")
	}

	filePath, err := sm.filePath(*fileID)
	if err != nil {
		return false, err
	}

	if ok, err := sm.db.Has(ctx, FileKey(*fileID)); !ok || err != nil {
		return false, fmt.Errorf("file %s missing from metadata store: %w", *fileID, err)
	}
	if _, err := os.Stat(filePath); err != nil {
		return false, fmt.Errorf("file %s missing from disk: %w", *fileID, err)
	}

	if sm.atlas != nil && sm.atlas.QueryClients.Storage != nil {
		if _, err := sm.atlas.QueryClients.Storage.File(ctx, &storageTypes.QueryFileRequest{Fid: *fileID}); err != nil {
			sm.logger.Warn("Unable to verify file against chain state",
				zap.String("file_id", *fileID),
				zap.Error(err))
			return false, err
		}
		sm.RecordChainSync(time.Now().UTC())
	}

	return true, nil
}

func (sm *StorageManager) GetStatus() (*types.ProviderStatus, error) {
	totalSize, fileCount, err := sm.currentUsage(context.Background())
	if err != nil {
		return nil, err
	}

	uptime := 0.0
	peers := 0

	if uptimeData, err := sm.db.Get(context.Background(), ProviderKey("uptime")); err == nil {
		if val, parseErr := strconv.ParseFloat(string(uptimeData), 64); parseErr == nil {
			uptime = val
		}
	}
	if peersData, err := sm.db.Get(context.Background(), ProviderKey("peers")); err == nil {
		if val, parseErr := strconv.Atoi(string(peersData)); parseErr == nil {
			peers = val
		}
	}

	walletAddr := ""
	if sm.atlas != nil && sm.atlas.Wallet != nil {
		walletAddr = sm.atlas.Wallet.GetAddress()
	}

	providerID := sm.config.ProviderName
	if providerID == "" {
		providerID = walletAddr
	}
	if providerID == "" {
		providerID = "unknown"
	}

	sm.statusMu.RLock()
	lastSync := sm.lastChainSync
	if sm.lastProofAt.After(lastSync) {
		lastSync = sm.lastProofAt
	}
	if sm.lastChallenge.After(lastSync) {
		lastSync = sm.lastChallenge
	}
	sm.statusMu.RUnlock()

	return &types.ProviderStatus{
		ProviderID:   providerID,
		Wallet:       walletAddr,
		Uptime:       uptime,
		TotalStorage: sm.config.TotalSpace,
		UsedStorage:  totalSize,
		FilesCount:   fileCount,
		IsOnline:     sm.atlas != nil && sm.atlas.Wallet != nil,
		LastSync:     lastSync,
		Peers:        peers,
		Version:      config.Version(),
	}, nil
}

func (sm *StorageManager) storeMetadata(ctx context.Context, metadata *types.FileMetadata) error {
	return sm.db.SetJSON(ctx, FileKey(metadata.FID), metadata)
}

func (sm *StorageManager) getMetadata(ctx context.Context, fileID string) (*types.FileMetadata, error) {
	if err := validateFileID(fileID); err != nil {
		return nil, err
	}

	var metadata types.FileMetadata
	if err := sm.db.GetJSON(ctx, FileKey(fileID), &metadata); err != nil {
		return nil, err
	}
	return &metadata, nil
}

func (sm *StorageManager) Close() error {
	if sm.db != nil {
		return sm.db.Close()
	}
	return nil
}

func (sm *StorageManager) generateProof(tree *merkletree.MerkleTree, index int64) (*merkletree.Proof, error) {
	if tree == nil {
		return nil, fmt.Errorf("cannot generate proof from nil tree")
	}
	if index < 0 {
		return nil, fmt.Errorf("proof index must be non-negative")
	}

	proof, err := tree.Proof(int(index))
	if err != nil {
		return nil, err
	}
	return proof, nil
}
