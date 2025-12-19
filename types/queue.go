package types

import "time"

const UploadQueueName = "uploads:processing"

type UploadJob struct {
	ID          string    `json:"id"`
	Owner       string    `json:"owner"`
	FilePath    string    `json:"file_path"`
	Size        int64     `json:"size"`
	CreatedAt   time.Time `json:"created_at"`
	CompletedAt time.Time `json:"completed_at"`
	Status      string    `json:"status"`
}
