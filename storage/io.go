package storage

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"time"

	"cygnus/types"

	"github.com/rs/zerolog/log"
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
	// 1GB @ 1KB/chunk → 1M syscalls without buffering → ~4K with 256KB buffer
	bufWriter := bufio.NewWriterSize(dest, writeBufSize)

	result := &ingestResult{}
	buf := make([]byte, types.ChunkSize)
	result.Leaves = make([][]byte, 0, 1024)

	loopStart := time.Now()
	lastReport := loopStart
	reportEvery := 100_000 // log throughput every N chunks

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

		if result.Chunks%reportEvery == 0 {
			now := time.Now()
			elapsedTotal := now.Sub(loopStart)
			elapsedSince := now.Sub(lastReport)
			bytesSince := int64(reportEvery) * types.ChunkSize
			mbpsSince := float64(bytesSince) / elapsedSince.Seconds() / (1024 * 1024)
			mbpsTotal := float64(result.Size) / elapsedTotal.Seconds() / (1024 * 1024)
			log.Info().
				Int("chunks", result.Chunks).
				Int64("bytes", result.Size).
				Float64("mbps_segment", mbpsSince).
				Float64("mbps_cumulative", mbpsTotal).
				Msg("streamFile: progress")
			lastReport = now
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
