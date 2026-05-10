package storage

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"cygnus/types"
)

func TestStreamFileToDiskAndCollectLeaves(t *testing.T) {
	t.Helper()

	tempDir := t.TempDir()
	destination := filepath.Join(tempDir, "file.bin")
	payload := bytes.Repeat([]byte("a"), int(types.ChunkSize)+17)

	result, err := streamFileToDiskAndCollectLeaves(bytes.NewReader(payload), destination, false)
	if err != nil {
		t.Fatalf("streamFileToDiskAndCollectLeaves returned error: %v", err)
	}

	if result.Size != int64(len(payload)) {
		t.Fatalf("unexpected size: got %d want %d", result.Size, len(payload))
	}
	if result.Chunks != 2 {
		t.Fatalf("unexpected chunk count: got %d want 2", result.Chunks)
	}
	if len(result.FirstChunk) != int(types.ChunkSize) {
		t.Fatalf("unexpected first chunk size: got %d want %d", len(result.FirstChunk), types.ChunkSize)
	}
	if len(result.Leaves) != 2 {
		t.Fatalf("unexpected leaf count: got %d want 2", len(result.Leaves))
	}

	onDisk, err := os.ReadFile(destination)
	if err != nil {
		t.Fatalf("failed to read streamed file: %v", err)
	}
	if !bytes.Equal(onDisk, payload) {
		t.Fatalf("streamed file contents do not match source")
	}
}
