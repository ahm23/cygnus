package cmd

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	merkletree "github.com/ahm23/go-merkletree-xxh"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/spf13/cobra"
	"github.com/zeebo/blake3"
	"go.uber.org/zap"

	storagetypes "atlas/x/storage/types"

	"cygnus/atlas"
	"cygnus/config"
	cygnustypes "cygnus/types"
)

const (
	defaultStressPostBatchSize   = 250
	defaultStressUploadBatchSize = 500
)

type stressFile struct {
	id         string
	path       string
	name       string
	size       int64
	merkleRoot []byte
}

func StressTestCmd() *cobra.Command {
	var fileCount int
	var fileSizeRaw string
	var apiURL string
	var postBatchSize int
	var uploadBatchSize int
	var replicas int32
	var subscription string
	var tempDir string
	var keepFiles bool
	var keyName string
	var keySource string

	cmd := &cobra.Command{
		Use:   "stress-test",
		Short: "Stage and upload many generated files against this provider",
		RunE: func(cmd *cobra.Command, args []string) error {
			if fileCount < 1 {
				return fmt.Errorf("--files must be at least 1")
			}
			fileSize, err := parseStressSize(fileSizeRaw)
			if err != nil {
				return err
			}
			if fileSize < 1 {
				return fmt.Errorf("--size must be at least 1 byte")
			}
			if postBatchSize < 1 {
				return fmt.Errorf("--post-batch-size must be between 1 and %d", defaultStressPostBatchSize)
			}
			if uploadBatchSize < 1 {
				return fmt.Errorf("--upload-batch-size must be at least 1")
			}
			normalizedKeySource := normalizeStressKeySource(keySource)

			home, err := cmd.Flags().GetString(FlagHome)
			if err != nil {
				return err
			}

			cfg, err := config.Init(home)
			if err != nil {
				return fmt.Errorf("failed to load config: %w", err)
			}

			logger, err := zap.NewDevelopment()
			if err != nil {
				return err
			}
			defer func() { _ = logger.Sync() }()

			if apiURL == "" {
				apiURL = defaultStressAPIURL(cfg)
			}
			uploadURL, err := normalizeStressUploadURL(apiURL)
			if err != nil {
				return err
			}

			workDir := tempDir
			if workDir == "" {
				workDir, err = os.MkdirTemp("", "cygnus-stress-*")
				if err != nil {
					return fmt.Errorf("failed to create temp directory: %w", err)
				}
			} else if err := os.MkdirAll(workDir, 0o755); err != nil {
				return fmt.Errorf("failed to create temp directory %s: %w", workDir, err)
			}
			if !keepFiles {
				defer os.RemoveAll(workDir)
			}

			fmt.Printf("Stress test starting\n")
			fmt.Printf("  files:          %d\n", fileCount)
			fmt.Printf("  size per file:  %d bytes\n", fileSize)
			fmt.Printf("  temp dir:       %s\n", workDir)
			fmt.Printf("  upload URL:     %s\n", uploadURL)
			fmt.Printf("  post batch:     %d\n", postBatchSize)
			fmt.Printf("  upload batch:   %d\n", uploadBatchSize)
			fmt.Printf("  key name:       %s\n", keyName)
			if normalizedKeySource != "" {
				fmt.Printf("  key source:     %s\n", normalizedKeySource)
			}

			files, err := createStressFiles(workDir, cfg.ProviderName, fileCount, fileSize)
			if err != nil {
				return err
			}

			am, err := atlas.NewAtlasManager(cfg, logger)
			if err != nil {
				return err
			}
			defer func() { _ = am.Close() }()

			if err := am.ConnectGRPC(); err != nil {
				return err
			}

			if err := am.ConnectWalletWithKeyNameAndSource(keyName, normalizedKeySource); err != nil {
				return err
			}

			if err := postStressFiles(context.Background(), am, files, postBatchSize, replicas, subscription); err != nil {
				return err
			}

			return uploadStressFiles(context.Background(), uploadURL, files, uploadBatchSize)
		},
	}

	cmd.Flags().IntVar(&fileCount, "files", 0, "number of files to generate and upload")
	cmd.Flags().StringVar(&fileSizeRaw, "size", "", "size per file in bytes, or with suffix KB/MB/GB")
	cmd.Flags().StringVar(&apiURL, "api-url", "", "provider API base URL or /api/v1/upload URL (default from config)")
	cmd.Flags().IntVar(&postBatchSize, "post-batch-size", defaultStressPostBatchSize, "maximum MsgPostFile messages per transaction")
	cmd.Flags().IntVar(&uploadBatchSize, "upload-batch-size", defaultStressUploadBatchSize, "maximum concurrent uploads per batch")
	cmd.Flags().Int32Var(&replicas, "replicas", 3, "replica count for MsgPostFile")
	cmd.Flags().StringVar(&subscription, "subscription", "", "subscription ID for MsgPostFile; empty uses chain default")
	cmd.Flags().StringVar(&tempDir, "temp-dir", "", "directory for generated files; default creates a temporary directory")
	cmd.Flags().BoolVar(&keepFiles, "keep-files", false, "keep generated files after the command exits")
	cmd.Flags().StringVar(&keyName, "key-name", "cygnus", "keyring key name to use for MsgPostFile transactions")
	cmd.Flags().StringVar(&keySource, "key-source", "", "keyring home/root directory, or a keyring-test/keyring-file directory; default uses --home")
	_ = cmd.MarkFlagRequired("files")
	_ = cmd.MarkFlagRequired("size")

	return cmd
}

func normalizeStressKeySource(keySource string) string {
	keySource = strings.TrimSpace(os.ExpandEnv(keySource))
	if keySource == "" {
		return ""
	}

	base := filepath.Base(filepath.Clean(keySource))
	if base == "keyring-test" || base == "keyring-file" {
		return filepath.Dir(keySource)
	}

	return keySource
}

func createStressFiles(dir, providerName string, count int, size int64) ([]stressFile, error) {
	fmt.Printf("\nCreating files...\n")

	runID := time.Now().UTC().Format("20060102T150405.000000000")
	files := make([]stressFile, 0, count)
	for i := 0; i < count; i++ {
		name := fmt.Sprintf("stress-%s-%06d.bin", runID, i)
		path := filepath.Join(dir, name)

		root, err := writeStressFile(path, int64(i), size)
		if err != nil {
			return nil, err
		}

		id := stressFileID(providerName, runID, i, root)
		files = append(files, stressFile{
			id:         id,
			path:       path,
			name:       name,
			size:       size,
			merkleRoot: root,
		})

		printStressProgress("created", i+1, count, "")
	}
	fmt.Println()

	return files, nil
}

func writeStressFile(path string, index, size int64) ([]byte, error) {
	file, err := os.Create(path)
	if err != nil {
		return nil, fmt.Errorf("failed to create %s: %w", path, err)
	}
	defer file.Close()

	chunk := make([]byte, cygnustypes.ChunkSize)
	var leaves [][]byte
	var written int64

	for written < size {
		n := int64(len(chunk))
		if remaining := size - written; remaining < n {
			n = remaining
		}

		fillStressBytes(chunk[:n], index, written)
		if _, err := file.Write(chunk[:n]); err != nil {
			return nil, fmt.Errorf("failed to write %s: %w", path, err)
		}
		leaves = append(leaves, hashStressChunk(chunk[:n]))
		written += n
	}

	if err := file.Sync(); err != nil {
		return nil, fmt.Errorf("failed to sync %s: %w", path, err)
	}

	tree, err := merkletree.New(&merkletree.Config{XXH128: true}, leaves)
	if err != nil {
		return nil, fmt.Errorf("failed to build merkle tree for %s: %w", path, err)
	}

	return append([]byte(nil), tree.Root...), nil
}

func fillStressBytes(dst []byte, fileIndex, offset int64) {
	seed := sha256.Sum256([]byte(fmt.Sprintf("cygnus-stress:%d:%d", fileIndex, offset)))
	for i := range dst {
		dst[i] = seed[i%len(seed)] ^ byte((offset+int64(i))&0xff)
	}
}

func hashStressChunk(chunk []byte) []byte {
	hasher := blake3.New()
	_, _ = hasher.Write(chunk)
	return hasher.Sum(nil)
}

func stressFileID(providerName, runID string, index int, merkleRoot []byte) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s:%s:%d:%s", providerName, runID, index, hex.EncodeToString(merkleRoot))))
	return hex.EncodeToString(sum[:])[:32]
}

func postStressFiles(ctx context.Context, am *atlas.AtlasManager, files []stressFile, batchSize int, replicas int32, subscription string) error {
	fmt.Printf("\nPosting files to chain...\n")

	creator := am.Wallet.GetAddress()
	for start := 0; start < len(files); start += batchSize {
		end := start + batchSize
		if end > len(files) {
			end = len(files)
		}

		msgs := make([]sdk.Msg, 0, end-start)
		for _, file := range files[start:end] {
			msgs = append(msgs, &storagetypes.MsgPostFile{
				Creator:      creator,
				Fid:          file.id,
				Merkle:       file.merkleRoot,
				FileSize:     file.size,
				Replicas:     replicas,
				Subscription: subscription,
			})
		}

		resp, err := am.Wallet.BroadcastTxGrpc(3, true, msgs...)
		if err != nil {
			return fmt.Errorf("failed to post chain batch %d-%d: %w", start+1, end, err)
		}
		printStressProgress("posted", end, len(files), resp.TxHash)
	}
	fmt.Println()

	return nil
}

func uploadStressFiles(ctx context.Context, uploadURL string, files []stressFile, batchSize int) error {
	fmt.Printf("\nUploading files to provider API...\n")
	client := &http.Client{Timeout: 30 * time.Minute}

	var uploaded atomic.Int64
	var failed atomic.Int64
	var allErrs []error
	var errMu sync.Mutex
	var printMu sync.Mutex

	for start := 0; start < len(files); start += batchSize {
		end := start + batchSize
		if end > len(files) {
			end = len(files)
		}

		fmt.Printf("Starting upload batch %d-%d of %d\n", start+1, end, len(files))

		var wg sync.WaitGroup
		for _, file := range files[start:end] {
			file := file
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := uploadStressFile(ctx, client, uploadURL, file); err != nil {
					nextFailed := failed.Add(1)
					errMu.Lock()
					allErrs = append(allErrs, fmt.Errorf("%s: %w", file.id, err))
					errMu.Unlock()
					printMu.Lock()
					printStressProgress("upload failed", int(uploaded.Load()+nextFailed), len(files), file.id)
					printMu.Unlock()
					return
				}
				nextUploaded := uploaded.Add(1)
				printMu.Lock()
				printStressProgress("uploaded", int(nextUploaded+failed.Load()), len(files), file.id)
				printMu.Unlock()
			}()
		}
		wg.Wait()
		fmt.Println()
	}

	if len(allErrs) > 0 {
		fmt.Printf("\nUpload completed with %d errors:\n", len(allErrs))
		for _, err := range allErrs {
			fmt.Printf("  - %v\n", err)
		}
		return fmt.Errorf("%d uploads failed", len(allErrs))
	}

	fmt.Printf("\nStress test complete: %d files uploaded successfully\n", uploaded.Load())
	return nil
}

func uploadStressFile(ctx context.Context, client *http.Client, uploadURL string, file stressFile) error {
	bodyReader, bodyWriter := io.Pipe()
	multipartWriter := multipart.NewWriter(bodyWriter)

	go func() {
		defer bodyWriter.Close()
		defer multipartWriter.Close()

		if err := multipartWriter.WriteField("fid", file.id); err != nil {
			_ = bodyWriter.CloseWithError(err)
			return
		}

		part, err := multipartWriter.CreateFormFile("file", file.name)
		if err != nil {
			_ = bodyWriter.CloseWithError(err)
			return
		}

		src, err := os.Open(file.path)
		if err != nil {
			_ = bodyWriter.CloseWithError(err)
			return
		}
		defer src.Close()

		if _, err := io.Copy(part, src); err != nil {
			_ = bodyWriter.CloseWithError(err)
			return
		}
	}()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, uploadURL, bodyReader)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", multipartWriter.FormDataContentType())

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var buf bytes.Buffer
		_, _ = io.CopyN(&buf, resp.Body, 4096)
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(buf.String()))
	}

	return nil
}

func defaultStressAPIURL(cfg *config.Config) string {
	host := strings.TrimSpace(cfg.Ip)
	if host == "" {
		host = "localhost"
	}
	if strings.Contains(host, "://") {
		parsed, err := url.Parse(host)
		if err == nil && parsed.Port() == "" {
			parsed.Host = fmt.Sprintf("%s:%d", parsed.Hostname(), cfg.APICfg.Port)
			return strings.TrimRight(parsed.String(), "/")
		}
		return strings.TrimRight(host, "/")
	}
	if strings.Contains(host, ":") {
		return "http://" + strings.TrimRight(host, "/")
	}
	return fmt.Sprintf("http://%s:%d", host, cfg.APICfg.Port)
}

func normalizeStressUploadURL(raw string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		return "", fmt.Errorf("api URL cannot be empty")
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("unsupported API URL scheme %q", parsed.Scheme)
	}
	if strings.HasSuffix(parsed.Path, "/api/v1/upload") {
		return strings.TrimRight(parsed.String(), "/"), nil
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/") + "/api/v1/upload"
	return parsed.String(), nil
}

func parseStressSize(raw string) (int64, error) {
	normalized := strings.TrimSpace(strings.ToUpper(raw))
	if normalized == "" {
		return 0, fmt.Errorf("--size is required")
	}

	multiplier := int64(1)
	suffixes := []struct {
		suffix     string
		multiplier int64
	}{
		{suffix: "GB", multiplier: 1024 * 1024 * 1024},
		{suffix: "G", multiplier: 1024 * 1024 * 1024},
		{suffix: "MB", multiplier: 1024 * 1024},
		{suffix: "M", multiplier: 1024 * 1024},
		{suffix: "KB", multiplier: 1024},
		{suffix: "K", multiplier: 1024},
		{suffix: "B", multiplier: 1},
	}
	for _, candidate := range suffixes {
		if strings.HasSuffix(normalized, candidate.suffix) {
			multiplier = candidate.multiplier
			normalized = strings.TrimSpace(strings.TrimSuffix(normalized, candidate.suffix))
			break
		}
	}

	value, err := strconv.ParseInt(normalized, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid --size %q: %w", raw, err)
	}
	return value * multiplier, nil
}

func printStressProgress(label string, current, total int, detail string) {
	if detail == "" {
		fmt.Printf("\r%s: %d/%d", label, current, total)
		return
	}
	fmt.Printf("\r%s: %d/%d (%s)", label, current, total, detail)
}
