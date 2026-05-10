package storage

import (
	"fmt"
	"io"
	"os"

	"cygnus/types"
)

type ingestResult struct {
	FirstChunk []byte
	Leaves     [][]byte
	Size       int64
	Chunks     int
}

func streamFileToDiskAndCollectLeaves(src io.Reader, destinationPath string, syncToDisk bool) (*ingestResult, error) {
	dest, err := os.Create(destinationPath)
	if err != nil {
		return nil, fmt.Errorf("failed to create temp file: %w", err)
	}

	cleanup := func(closeErr error) error {
		_ = dest.Close()
		_ = os.Remove(destinationPath)
		return closeErr
	}

	result := &ingestResult{}
	buf := make([]byte, types.ChunkSize)

	for {
		n, readErr := io.ReadFull(src, buf)
		if readErr != nil && readErr != io.EOF && readErr != io.ErrUnexpectedEOF {
			return nil, cleanup(fmt.Errorf("failed to read upload stream: %w", readErr))
		}

		if n > 0 {
			chunk := append([]byte(nil), buf[:n]...)
			if _, err := dest.Write(chunk); err != nil {
				return nil, cleanup(fmt.Errorf("failed to write upload stream: %w", err))
			}

			result.Leaves = append(result.Leaves, hashChunk(chunk))
			result.Size += int64(n)
			result.Chunks++
			if result.FirstChunk == nil {
				result.FirstChunk = chunk
			}
		}

		if readErr == io.EOF || readErr == io.ErrUnexpectedEOF {
			break
		}
	}

	if result.Chunks == 0 {
		return nil, cleanup(fmt.Errorf("empty files are not supported"))
	}

	if syncToDisk {
		if err := dest.Sync(); err != nil {
			return nil, cleanup(fmt.Errorf("failed to sync upload stream: %w", err))
		}
	}
	if err := dest.Close(); err != nil {
		_ = os.Remove(destinationPath)
		return nil, fmt.Errorf("failed to close temp file: %w", err)
	}

	return result, nil
}
