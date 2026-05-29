package storage

import (
	"bufio"
	"fmt"
	"io"
	"os"

	"cygnus/types"
)

const (
	// writeBufSize is the buffer size for buffered disk writes during upload.
	// Larger buffers reduce syscalls when writing many small chunks.
	writeBufSize = 256 * 1024
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

	// buffered writer batches small writes into large ones:
	// 1GB @ 1KB/chunk 1M syscalls without buffering → ~4K with 256KB buffer
	bufWriter := bufio.NewWriterSize(dest, writeBufSize)

	result := &ingestResult{}
	buf := make([]byte, types.ChunkSize)
	result.Leaves = make([][]byte, 0, 1024)

	for {
		n, readErr := io.ReadFull(src, buf)
		if readErr != nil && readErr != io.EOF && readErr != io.ErrUnexpectedEOF {
			return nil, cleanup(fmt.Errorf("failed to read upload stream: %w", readErr))
		}

		if n > 0 {
			// write via buffered writer — batched, not one syscall per chunk
			if _, err := bufWriter.Write(buf[:n]); err != nil {
				return nil, cleanup(fmt.Errorf("failed to write upload stream: %w", err))
			}

			// hash directly from buf — only the 32-byte hash is persisted in leaves
			result.Leaves = append(result.Leaves, hashChunk(buf[:n]))
			result.Size += int64(n)
			result.Chunks++

			// only the first chunk's data is needed for the initial proof;
			// subsequent chunks are re-read from the file on demand via getFileSegment
			if result.FirstChunk == nil {
				result.FirstChunk = append([]byte(nil), buf[:n]...)
			}
		}

		if readErr == io.EOF || readErr == io.ErrUnexpectedEOF {
			break
		}
	}

	if result.Chunks == 0 {
		return nil, cleanup(fmt.Errorf("empty files are not supported"))
	}

	// flush buffered writer before sync/close
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
