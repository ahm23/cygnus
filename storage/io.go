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

	const bulkSize = 64 * 1024 // read in 64KB blocks from the source

	result := &ingestResult{}
	bulkBuf := make([]byte, bulkSize)
	result.Leaves = make([][]byte, 0, 1024)

	loopStart := time.Now()
	lastReport := loopStart
	reportEvery := 100_000 // log throughput every N chunks

	for {
		// read a large block from the source — one boundary check, not one per 1KB
		n, readErr := io.ReadFull(src, bulkBuf)
		if readErr != nil && readErr != io.EOF && readErr != io.ErrUnexpectedEOF {
			return nil, cleanup(fmt.Errorf("failed to read upload stream: %w", readErr))
		}

		// split the block into protocol ChunkSize pieces
		for off := int64(0); off < int64(n); off += types.ChunkSize {
			end := off + types.ChunkSize
			if end > int64(n) {
				end = int64(n)
			}
			piece := bulkBuf[off:end]

			if _, err := bufWriter.Write(piece); err != nil {
				return nil, cleanup(fmt.Errorf("failed to write upload stream: %w", err))
			}

			result.Leaves = append(result.Leaves, hashChunk(piece))
			result.Size += int64(len(piece))
			result.Chunks++

			if result.FirstChunk == nil {
				result.FirstChunk = append([]byte(nil), piece...)
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
