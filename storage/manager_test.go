package storage

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"cygnus/config"
	"cygnus/types"

	"go.uber.org/zap"
)

func newTestStorageManager(t *testing.T, totalSpace int64) *StorageManager {
	t.Helper()

	cfg := &config.Config{
		DataDirectory: t.TempDir(),
		TotalSpace:    totalSpace,
	}

	sm, err := NewStorageManager(cfg, zap.NewNop(), nil)
	if err != nil {
		t.Fatalf("NewStorageManager returned error: %v", err)
	}

	t.Cleanup(func() {
		_ = sm.Close()
	})

	return sm
}

func TestHasCapacityForIgnoresPebbleIndexFiles(t *testing.T) {
	sm := newTestStorageManager(t, 10)

	filePath := filepath.Join(sm.config.DataDirectory, "payload.bin")
	if err := os.WriteFile(filePath, []byte("12345"), 0o644); err != nil {
		t.Fatalf("failed to write payload file: %v", err)
	}

	ok, used, err := sm.HasCapacityFor(context.Background(), 5)
	if err != nil {
		t.Fatalf("HasCapacityFor returned error: %v", err)
	}
	if !ok {
		t.Fatalf("expected provider to have capacity, used=%d", used)
	}
	if used != 5 {
		t.Fatalf("unexpected used bytes: got %d want 5", used)
	}
}

func TestBuildMerkleTreeFromFileSupportsNonZeroChunkProofs(t *testing.T) {
	sm := newTestStorageManager(t, 1024*1024)

	payload := make([]byte, int(types.ChunkSize*2+13))
	for i := range payload {
		payload[i] = byte(i % 251)
	}

	filePath := filepath.Join(sm.config.DataDirectory, "proof.bin")
	if err := os.WriteFile(filePath, payload, 0o644); err != nil {
		t.Fatalf("failed to write proof file: %v", err)
	}

	tree, chunks, err := sm.buildMerkleTreeFromFile(context.Background(), filePath)
	if err != nil {
		t.Fatalf("buildMerkleTreeFromFile returned error: %v", err)
	}
	if chunks != 3 {
		t.Fatalf("unexpected chunk count: got %d want 3", chunks)
	}

	proof, err := sm.generateProof(tree, 1)
	if err != nil {
		t.Fatalf("generateProof returned error: %v", err)
	}
	if proof.Index != 1 {
		t.Fatalf("unexpected proof index: got %d want 1", proof.Index)
	}
}

func TestListFilesReturnsDeterministicPagination(t *testing.T) {
	sm := newTestStorageManager(t, 1024*1024)
	ctx := context.Background()

	first := &types.FileMetadata{
		FID:         "a",
		FileName:    "a.bin",
		Size:        1,
		Chunks:      1,
		MerkleRoot:  "root-a",
		UploadedAt:  time.Now().Add(-time.Minute),
		IsAvailable: true,
	}
	second := &types.FileMetadata{
		FID:         "b",
		FileName:    "b.bin",
		Size:        1,
		Chunks:      1,
		MerkleRoot:  "root-b",
		UploadedAt:  time.Now(),
		IsAvailable: true,
	}

	if err := sm.storeMetadata(ctx, first); err != nil {
		t.Fatalf("storeMetadata(first) returned error: %v", err)
	}
	if err := sm.storeMetadata(ctx, second); err != nil {
		t.Fatalf("storeMetadata(second) returned error: %v", err)
	}

	page, err := sm.ListFiles(ctx, 1, 1)
	if err != nil {
		t.Fatalf("ListFiles returned error: %v", err)
	}
	if len(page.Files) != 1 {
		t.Fatalf("unexpected file count on page: got %d want 1", len(page.Files))
	}
	if page.Files[0].FID != "b" {
		t.Fatalf("unexpected first paged file: got %s want b", page.Files[0].FID)
	}
	if !page.HasNext {
		t.Fatalf("expected pagination to indicate next page")
	}
}
