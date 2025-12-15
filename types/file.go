package types

import (
	"crypto/sha256"
	"encoding/hex"
	"time"
)

const (
	ChunkSize = 1024
)

func GenerateFileID(owner string, merkleRoot []byte, index int64) string {
	fidRaw := append([]byte(owner), merkleRoot...)
	fidRaw = append(fidRaw, byte(index))
	fidBytes := sha256.Sum256(fidRaw)

	return hex.EncodeToString(fidBytes[:])[:32]
}

type FileMetadata struct {
	FID      string `json:"fid"`
	FileName string `json:"filename"`
	Size     int64  `json:"size"`
	// MimeType    string    `json:"mime_type"`
	Chunks      int       `json:"chunks"`
	MerkleRoot  string    `json:"merkle_root"`
	UploadedAt  time.Time `json:"uploaded_at"`
	Owner       string    `json:"owner"` // Wallet address
	IsAvailable bool      `json:"is_available"`
}

type FileListResponse struct {
	Files       []FileMetadata `json:"files"`
	Total       int64          `json:"total"`
	Page        int            `json:"page"`
	PageSize    int            `json:"page_size"`
	HasNext     bool           `json:"has_next"`
	HasPrevious bool           `json:"has_previous"`
}

type ProviderStatus struct {
	ProviderID   string    `json:"provider_id"`
	Wallet       string    `json:"wallet_address"`
	Uptime       float64   `json:"uptime"`
	TotalStorage int64     `json:"total_storage"`
	UsedStorage  int64     `json:"used_storage"`
	FilesCount   int64     `json:"files_count"`
	IsOnline     bool      `json:"is_online"`
	LastSync     time.Time `json:"last_sync"`
	Peers        int       `json:"peers"`
	Version      string    `json:"version"`
}

type DownloadRequest struct {
	ID     string `json:"id"`
	Stream bool   `json:"stream,omitempty"`
	Chunk  int    `json:"chunk,omitempty"`
}

type ErrorResponse struct {
	Error   string `json:"error"`
	Code    string `json:"code,omitempty"`
	Details string `json:"details,omitempty"`
}

type APIResponse struct {
	Success bool        `json:"success"`
	Data    interface{} `json:"data,omitempty"`
	Error   string      `json:"error,omitempty"`
	Message string      `json:"message,omitempty"`
}
