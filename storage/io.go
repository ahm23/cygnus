package storage

import (
	"bufio"
	"fmt"
	"io"
	"os"

	"cygnus/types"
)

const (
	writeBufSize = 256 * 1024  // buffered disk write buffer
	readBufSize  = 1024 * 1024 // internal read buffer — read big, split into ChunkSize pieces
)

type ingestResult struct {
	FirstChunk []byte
	Leaves     [][]byte
	Size       int64
	Chunks     int
}

func streamFileToDiskAndCollectLeaves(src io.Reader, destinationPath string, syncToDisk bool, fileSizeHint int64) (*ingestResult, error) {
	dest, err := os.Create(destinationPath)
	if err != nil {
		return nil, fmt.Errorf("failed to create temp file: %w", err)
	}

	cleanup := func(closeErr error) error {
		_ = dest.Close()
		_ = os.Remove(destinationPath)
		return closeErr
	}

	bufWriter := bufio.NewWriterSize(dest, writeBufSize)

	result := &ingestResult{}
	readBuf := make([]byte, readBufSize)

	// pre-allocate leaves to avoid reallocation churn
	estimatedChunks := int(fileSizeHint / types.ChunkSize)
	if estimatedChunks < 1024 {
		estimatedChunks = 1024
	}
	result.Leaves = make([][]byte, 0, estimatedChunks)

	for {
		n, readErr := src.Read(readBuf)
		if n == 0 && readErr == io.EOF {
			break
		}
		if readErr != nil && readErr != io.EOF && readErr != io.ErrUnexpectedEOF {
			return nil, cleanup(fmt.Errorf("failed to read upload stream: %w", readErr))
		}

		// split the large read into protocol ChunkSize pieces
		for off := 0; off < n; off += types.ChunkSize {
			end := off + types.ChunkSize
			if end > n {
				end = n
			}
			piece := readBuf[off:end]

			if _, err := bufWriter.Write(piece); err != nil {
				return nil, cleanup(fmt.Errorf("failed to write upload stream: %w", err))
			}

			// hash the piece — only the 32-byte hash is stored in leaves
			result.Leaves = append(result.Leaves, hashChunk(piece))
			result.Size += int64(len(piece))
			result.Chunks++

			if result.FirstChunk == nil {
				result.FirstChunk = append([]byte(nil), piece...)
			}
		}

		if readErr == io.EOF || readErr == io.ErrUnexpectedEOF {
			break
		}
	}

	if result.Chunks == 0 {
		return nil, cleanup(fmt.Errorf("empty files are not supported"))
	}

	if err := bufWriter.Flush(); err != nil {
		return nil, cleanup(fmt.Errorf("failed to flush upload stream: %w", err))
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
