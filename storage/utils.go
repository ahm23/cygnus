package storage

import (
	"fmt"
	"os"
)

func getFileSegment(filePath string, start, end int64) ([]byte, error) {
	if start > end {
		return nil, fmt.Errorf("file segment start greater than segment end")
	}

	file, err := os.Open(filePath)
	if err != nil {
		return nil, fmt.Errorf("failed to open file: %w", err)
	}
	defer file.Close()

	stat, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("failed to stat file: %w", err)
	}
	fileSize := stat.Size()

	// Ensure the chunk is within bounds.
	if start >= fileSize {
		return nil, fmt.Errorf("file segment out of range for file size %d", fileSize)
	}

	// Determine the actual length to read (up to EOF at most).
	readLen := end - start
	if start+readLen > fileSize {
		readLen = fileSize - start
	}

	// Read the chunk into a byte slice.
	chunkData := make([]byte, readLen)
	n, err := file.ReadAt(chunkData, start)
	if err != nil {
		return nil, fmt.Errorf("failed to read chunk: %w", err)
	}
	if n != int(readLen) {
		return nil, fmt.Errorf("short read: expected %d bytes, got %d", readLen, n)
	}

	return chunkData, nil
}
