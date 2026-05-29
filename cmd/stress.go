package cmd

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	merkletree "github.com/ahm23/go-merkletree-xxh"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/spf13/cobra"
	"github.com/zeebo/blake3"

	storagetypes "atlas/x/storage/types"

	"cygnus/atlas"
	"cygnus/cmd/types"
	"cygnus/config"
	cygnustypes "cygnus/types"
)

const (
	defaultStressPostBatchSize     = 250
	defaultStressUploadBatchSize   = 500
	defaultStressPostCommitTimeout = 5 * time.Minute
)

type stressFile struct {
	id         string
	path       string
	name       string
	size       int64
	merkleRoot []byte
}

type stressRunMetrics struct {
	StartedAt       string               `json:"started_at"`
	FinishedAt      string               `json:"finished_at"`
	DurationMS      int64                `json:"duration_ms"`
	Success         bool                 `json:"success"`
	Error           string               `json:"error,omitempty"`
	FileCount       int                  `json:"file_count"`
	FileSizeBytes   int64                `json:"file_size_bytes"`
	UploadURL       string               `json:"upload_url"`
	PostBatchSize   int                  `json:"post_batch_size"`
	UploadBatchSize int                  `json:"upload_batch_size"`
	Replicas        int32                `json:"replicas"`
	Subscription    string               `json:"subscription,omitempty"`
	TempDir         string               `json:"temp_dir"`
	KeyName         string               `json:"key_name"`
	KeySource       string               `json:"key_source,omitempty"`
	Phases          stressPhaseMetrics   `json:"phases"`
	Upload          *stressUploadMetrics `json:"upload,omitempty"`
}

type stressPhaseMetrics struct {
	CreateFiles stressTimedPhase `json:"create_files"`
	PostFiles   stressTimedPhase `json:"post_files"`
	UploadFiles stressTimedPhase `json:"upload_files"`
}

type stressTimedPhase struct {
	StartedAt  string `json:"started_at,omitempty"`
	FinishedAt string `json:"finished_at,omitempty"`
	DurationMS int64  `json:"duration_ms,omitempty"`
}

type stressUploadMetrics struct {
	StartedAt         string                     `json:"started_at"`
	FinishedAt        string                     `json:"finished_at"`
	DurationMS        int64                      `json:"duration_ms"`
	Total             int                        `json:"total"`
	Uploaded          int                        `json:"uploaded"`
	Failed            int                        `json:"failed"`
	BatchSize         int                        `json:"batch_size"`
	UploadsPerSecond  float64                    `json:"uploads_per_second"`
	FirstCompletionMS int64                      `json:"first_completion_ms,omitempty"`
	LatencyMS         stressLatencyMetrics       `json:"latency_ms"`
	Batches           []stressUploadBatchMetrics `json:"batches"`
	ErrorSample       []string                   `json:"error_sample,omitempty"`
}

type stressUploadBatchMetrics struct {
	StartIndex       int     `json:"start_index"`
	EndIndex         int     `json:"end_index"`
	Count            int     `json:"count"`
	Uploaded         int     `json:"uploaded"`
	Failed           int     `json:"failed"`
	StartedAt        string  `json:"started_at"`
	FinishedAt       string  `json:"finished_at"`
	DurationMS       int64   `json:"duration_ms"`
	UploadsPerSecond float64 `json:"uploads_per_second"`
}

type stressLatencyMetrics struct {
	Min int64 `json:"min"`
	P50 int64 `json:"p50"`
	P95 int64 `json:"p95"`
	P99 int64 `json:"p99"`
	Max int64 `json:"max"`
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
	var metricsFile string

	cmd := &cobra.Command{
		Use:   "stress-test",
		Short: "Stage and upload many generated files against this provider",
		RunE: func(cmd *cobra.Command, args []string) (runErr error) {
			runStartedAt := time.Now()
			metrics := &stressRunMetrics{
				StartedAt: runStartedAt.UTC().Format(time.RFC3339Nano),
			}
			defer func() {
				runFinishedAt := time.Now()
				metrics.FinishedAt = runFinishedAt.UTC().Format(time.RFC3339Nano)
				metrics.DurationMS = durationMillis(runFinishedAt.Sub(runStartedAt))
				metrics.Success = runErr == nil
				if runErr != nil {
					metrics.Error = runErr.Error()
				}
				if metricsFile == "" {
					return
				}
				if err := writeStressMetrics(metricsFile, metrics); err != nil {
					if runErr == nil {
						runErr = err
					} else {
						fmt.Printf("\nfailed to write stress metrics: %v\n", err)
					}
					return
				}
				fmt.Printf("\nStress metrics written to %s\n", metricsFile)
			}()

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
			metrics.FileCount = fileCount
			metrics.FileSizeBytes = fileSize
			metrics.PostBatchSize = postBatchSize
			metrics.UploadBatchSize = uploadBatchSize
			metrics.Replicas = replicas
			metrics.Subscription = subscription
			metrics.KeyName = keyName
			metrics.KeySource = normalizedKeySource

			home, err := cmd.Flags().GetString(types.FlagHome)
			if err != nil {
				return err
			}

			cfg, err := config.Init(home)
			if err != nil {
				return fmt.Errorf("failed to load config: %w", err)
			}

			cw := zerolog.ConsoleWriter{Out: os.Stderr}
			log.Logger = zerolog.New(cw).Level(zerolog.DebugLevel).With().Timestamp().Caller().Logger()

			if apiURL == "" {
				apiURL = defaultStressAPIURL(cfg)
			}
			uploadURL, err := normalizeStressUploadURL(apiURL)
			if err != nil {
				return err
			}
			metrics.UploadURL = uploadURL

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
			metrics.TempDir = workDir

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

			createStartedAt := time.Now()
			files, err := createStressFiles(workDir, cfg.ProviderName, fileCount, fileSize)
			metrics.Phases.CreateFiles = newStressTimedPhase(createStartedAt, time.Now())
			if err != nil {
				return err
			}

			am, err := atlas.NewAtlasManager(cfg)
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

			postStartedAt := time.Now()
			if err := postStressFiles(context.Background(), am, files, postBatchSize, replicas, subscription); err != nil {
				metrics.Phases.PostFiles = newStressTimedPhase(postStartedAt, time.Now())
				return err
			}
			metrics.Phases.PostFiles = newStressTimedPhase(postStartedAt, time.Now())

			uploadStartedAt := time.Now()
			uploadMetrics, err := uploadStressFiles(context.Background(), uploadURL, files, uploadBatchSize)
			metrics.Phases.UploadFiles = newStressTimedPhase(uploadStartedAt, time.Now())
			metrics.Upload = uploadMetrics
			return err
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
	cmd.Flags().StringVar(&metricsFile, "metrics-file", "", "write stress test metrics to a JSON file")
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
	sum := blake3.Sum256(chunk)
	return sum[:]
}

func stressFileID(providerName, runID string, index int, merkleRoot []byte) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s:%s:%d:%s", providerName, runID, index, hex.EncodeToString(merkleRoot))))
	return hex.EncodeToString(sum[:])[:32]
}

func postStressFiles(ctx context.Context, am *atlas.AtlasManager, files []stressFile, batchSize int, replicas int32, subscription string) error {
	fmt.Printf("\nPosting files to chain...\n")

	creator := am.Wallet.GetAddress()
	lastTxHash := ""
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

		resp, err := am.Wallet.BroadcastTxGrpc(3, false, msgs...)
		if err != nil {
			return fmt.Errorf("failed to post chain batch %d-%d: %w", start+1, end, err)
		}
		lastTxHash = resp.TxHash
		printStressProgress("posted", end, len(files), resp.TxHash)
	}
	fmt.Println()

	if lastTxHash != "" {
		fmt.Printf("Waiting for final chain post tx to commit: %s\n", lastTxHash)
		resp, err := am.Wallet.WaitForTxWithTimeout(lastTxHash, defaultStressPostCommitTimeout)
		if err != nil {
			return fmt.Errorf("failed waiting for final chain post tx %s: %w", lastTxHash, err)
		}
		fmt.Printf("Final chain post tx committed at height %d\n", resp.Height)
	}

	return nil
}

func uploadStressFiles(ctx context.Context, uploadURL string, files []stressFile, batchSize int) (*stressUploadMetrics, error) {
	fmt.Printf("\nUploading files to provider API...\n")
	client := &http.Client{Timeout: 30 * time.Minute}
	startedAt := time.Now()
	metrics := &stressUploadMetrics{
		StartedAt: startedAt.UTC().Format(time.RFC3339Nano),
		Total:     len(files),
		BatchSize: batchSize,
	}

	var uploaded atomic.Int64
	var failed atomic.Int64
	var firstCompletion atomic.Int64
	var firstCompletionOnce sync.Once
	var allErrs []error
	var errMu sync.Mutex
	var latencies []time.Duration
	var latencyMu sync.Mutex
	var printMu sync.Mutex

	for start := 0; start < len(files); start += batchSize {
		end := start + batchSize
		if end > len(files) {
			end = len(files)
		}

		fmt.Printf("Starting upload batch %d-%d of %d\n", start+1, end, len(files))
		batchStartedAt := time.Now()
		batchUploadedBefore := uploaded.Load()
		batchFailedBefore := failed.Load()

		var wg sync.WaitGroup
		for _, file := range files[start:end] {
			file := file
			wg.Add(1)
			go func() {
				defer wg.Done()
				uploadStartedAt := time.Now()
				if err := uploadStressFile(ctx, client, uploadURL, file); err != nil {
					latencyMu.Lock()
					latencies = append(latencies, time.Since(uploadStartedAt))
					latencyMu.Unlock()
					firstCompletionOnce.Do(func() {
						firstCompletion.Store(time.Now().UnixNano())
					})
					nextFailed := failed.Add(1)
					errMu.Lock()
					allErrs = append(allErrs, fmt.Errorf("%s: %w", file.id, err))
					errMu.Unlock()
					printMu.Lock()
					printStressProgress("upload failed", int(uploaded.Load()+nextFailed), len(files), file.id)
					printMu.Unlock()
					return
				}
				latencyMu.Lock()
				latencies = append(latencies, time.Since(uploadStartedAt))
				latencyMu.Unlock()
				firstCompletionOnce.Do(func() {
					firstCompletion.Store(time.Now().UnixNano())
				})
				nextUploaded := uploaded.Add(1)
				printMu.Lock()
				printStressProgress("uploaded", int(nextUploaded+failed.Load()), len(files), file.id)
				printMu.Unlock()
			}()
		}
		wg.Wait()
		batchFinishedAt := time.Now()
		batchUploaded := int(uploaded.Load() - batchUploadedBefore)
		batchFailed := int(failed.Load() - batchFailedBefore)
		batchDuration := batchFinishedAt.Sub(batchStartedAt)
		metrics.Batches = append(metrics.Batches, stressUploadBatchMetrics{
			StartIndex:       start + 1,
			EndIndex:         end,
			Count:            end - start,
			Uploaded:         batchUploaded,
			Failed:           batchFailed,
			StartedAt:        batchStartedAt.UTC().Format(time.RFC3339Nano),
			FinishedAt:       batchFinishedAt.UTC().Format(time.RFC3339Nano),
			DurationMS:       durationMillis(batchDuration),
			UploadsPerSecond: perSecond(batchUploaded, batchDuration),
		})
		fmt.Println()
	}

	finishedAt := time.Now()
	metrics.FinishedAt = finishedAt.UTC().Format(time.RFC3339Nano)
	metrics.DurationMS = durationMillis(finishedAt.Sub(startedAt))
	metrics.Uploaded = int(uploaded.Load())
	metrics.Failed = int(failed.Load())
	metrics.UploadsPerSecond = perSecond(metrics.Uploaded, finishedAt.Sub(startedAt))
	if firstCompletedAt := firstCompletion.Load(); firstCompletedAt > 0 {
		metrics.FirstCompletionMS = durationMillis(time.Unix(0, firstCompletedAt).Sub(startedAt))
	}
	latencyMu.Lock()
	metrics.LatencyMS = summarizeStressLatencies(latencies)
	latencyMu.Unlock()

	if len(allErrs) > 0 {
		fmt.Printf("\nUpload completed with %d errors:\n", len(allErrs))
		for _, err := range allErrs {
			fmt.Printf("  - %v\n", err)
		}
		metrics.ErrorSample = stressErrorSample(allErrs, 100)
		return metrics, fmt.Errorf("%d uploads failed", len(allErrs))
	}

	fmt.Printf("\nStress test complete: %d files uploaded successfully\n", uploaded.Load())
	return metrics, nil
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

func newStressTimedPhase(startedAt, finishedAt time.Time) stressTimedPhase {
	return stressTimedPhase{
		StartedAt:  startedAt.UTC().Format(time.RFC3339Nano),
		FinishedAt: finishedAt.UTC().Format(time.RFC3339Nano),
		DurationMS: durationMillis(finishedAt.Sub(startedAt)),
	}
}

func durationMillis(duration time.Duration) int64 {
	if duration <= 0 {
		return 0
	}
	return duration.Milliseconds()
}

func perSecond(count int, duration time.Duration) float64 {
	if count <= 0 || duration <= 0 {
		return 0
	}
	return float64(count) / duration.Seconds()
}

func summarizeStressLatencies(latencies []time.Duration) stressLatencyMetrics {
	if len(latencies) == 0 {
		return stressLatencyMetrics{}
	}

	sorted := append([]time.Duration(nil), latencies...)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i] < sorted[j]
	})

	return stressLatencyMetrics{
		Min: durationMillis(sorted[0]),
		P50: durationMillis(stressPercentile(sorted, 0.50)),
		P95: durationMillis(stressPercentile(sorted, 0.95)),
		P99: durationMillis(stressPercentile(sorted, 0.99)),
		Max: durationMillis(sorted[len(sorted)-1]),
	}
}

func stressPercentile(sorted []time.Duration, percentile float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	if percentile <= 0 {
		return sorted[0]
	}
	if percentile >= 1 {
		return sorted[len(sorted)-1]
	}

	index := int(percentile*float64(len(sorted)-1) + 0.5)
	if index < 0 {
		index = 0
	}
	if index >= len(sorted) {
		index = len(sorted) - 1
	}
	return sorted[index]
}

func stressErrorSample(errs []error, limit int) []string {
	if limit < 1 || len(errs) == 0 {
		return nil
	}
	if len(errs) < limit {
		limit = len(errs)
	}

	sample := make([]string, 0, limit)
	for _, err := range errs[:limit] {
		sample = append(sample, err.Error())
	}
	return sample
}

func writeStressMetrics(path string, metrics *stressRunMetrics) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("failed to create metrics directory: %w", err)
	}

	file, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("failed to create metrics file: %w", err)
	}
	defer file.Close()

	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(metrics); err != nil {
		return fmt.Errorf("failed to write metrics file: %w", err)
	}

	return nil
}

func printStressProgress(label string, current, total int, detail string) {
	if detail == "" {
		fmt.Printf("\r%s: %d/%d", label, current, total)
		return
	}
	fmt.Printf("\r%s: %d/%d (%s)", label, current, total, detail)
}
