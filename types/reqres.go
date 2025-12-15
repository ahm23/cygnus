package types

type UploadRequest struct {
	Owner string `json:"owner"`
}

type UploadResponse struct {
	ID        string `json:"id"`
	UploadURL string `json:"upload_url,omitempty"`
	ChunkSize int64  `json:"chunk_size,omitempty"`
	MaxSize   int64  `json:"max_size"`
	ExpiresAt string `json:"expires_at,omitempty"`
}
