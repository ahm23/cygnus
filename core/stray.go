package core

import (
	"context"
	"encoding/binary"
	"fmt"
	"net/http"
	"sort"
	"time"

	"cygnus/atlas"
	"cygnus/config"
	"cygnus/storage"

	"github.com/rs/zerolog/log"
	"github.com/zeebo/blake3"
)

// TODO: no need for StrayFileEntry, just the regular File obj is enough.

// StrayFileEntry describes a file missing replicas that this provider may claim.
type StrayFileEntry struct {
	FileID   string
	FileName string
	Size     int64
	Owner    string
	Holders  []ProviderRef
}

// ProviderRef identifies a provider that holds a stray file and can serve it.
type ProviderRef struct {
	Address  string
	Hostname string // ip:port where the provider's HTTP API is reachable
}

// StrayFileLister is the interface the chain query must satisfy.
// Implement it once the chain-side QueryStrayFiles RPC is available.
type StrayFileLister interface {
	ListStrayFiles(ctx context.Context) ([]StrayFileEntry, error)
}

// StraySweeper periodically discovers STRAY files on the chain and claims them
// with an initial proof (first chunk, no challenge), replicating the same
// flow used in CreateFile / ClaimFile.
type StraySweeper struct {
	cfg        *config.StraySweepConfig
	sm         *storage.StorageManager
	am         *atlas.AtlasManager
	lister     StrayFileLister
	httpClient *http.Client
	warnedNoOp bool
}

// NewStraySweeper creates a stray file "sweeper" (querier & claimer).
func NewStraySweeper(cfg *config.StraySweepConfig, sm *storage.StorageManager, am *atlas.AtlasManager, lister StrayFileLister) *StraySweeper {
	return &StraySweeper{
		cfg:    cfg,
		sm:     sm,
		am:     am,
		lister: lister,
		httpClient: &http.Client{
			Timeout: 60 * time.Second,
		},
	}
}

// Run starts the sweeper loop. Blocks until ctx is cancelled.
func (s *StraySweeper) Run(ctx context.Context) {
	if s.cfg.IntervalSeconds <= 0 {
		log.Warn().Msg("Stray sweeper interval is zero or negative; disabled")
		return
	}
	if !s.cfg.Enabled {
		log.Info().Msg("Stray sweeper disabled by config")
		return
	}

	ticker := time.NewTicker(time.Duration(s.cfg.IntervalSeconds) * time.Second)
	defer ticker.Stop()

	log.Info().
		Int("interval_seconds", s.cfg.IntervalSeconds).
		Int("max_claims", s.cfg.MaxClaimsPerSweep).
		Int("max_concurrent", s.cfg.MaxConcurrentClaims).
		Msg("Stray file sweeper started")

	for {
		select {
		case <-ctx.Done():
			log.Info().Msg("Stray sweeper stopped")
			return
		case <-ticker.C:
			s.sweep(ctx)
		}
	}
}

// sweep runs one discovery-and-claim cycle.
func (s *StraySweeper) sweep(ctx context.Context) {
	// TODO: create query endpoint in atlas protocol
	files, err := s.lister.ListStrayFiles(ctx)
	if err != nil {
		log.Warn().Err(err).Msg("Failed to list stray files")
		return
	}
	if len(files) == 0 {
		return
	}

	providerAddr := ""
	if s.am != nil && s.am.Wallet != nil {
		providerAddr = s.am.Wallet.GetAddress()
	}

	// score each file deterministically so different providers naturally claim different subsets.
	type scoredFile struct {
		entry StrayFileEntry
		score uint64
	}
	scored := make([]scoredFile, len(files))
	for i, f := range files {
		scored[i] = scoredFile{entry: f, score: claimScore(f.FileID, providerAddr)}
	}
	sort.Slice(scored, func(i, j int) bool {
		return scored[i].score < scored[j].score
	})

	// take the top K that fit within remaining capacity.
	max := s.cfg.MaxClaimsPerSweep
	if len(scored) > max {
		scored = scored[:max]
	}

	log.Debug().
		Int("stray_files", len(files)).
		Int("candidates", len(scored)).
		Msg("Stray sweep scoring complete")

	// claim with bounded concurrency.
	sem := make(chan struct{}, s.cfg.MaxConcurrentClaims)
	for _, cf := range scored {
		select {
		case <-ctx.Done():
			return
		case sem <- struct{}{}:
		}
		go func(entry StrayFileEntry) {
			defer func() { <-sem }()
			s.claimOne(ctx, entry)
		}(cf.entry)
	}

	// drain the semaphore to wait for all in-flight claims to finish.
	for i := 0; i < cap(sem); i++ {
		sem <- struct{}{}
	}
}

// claimOne downloads a stray file from one of its holders and claims it locally.
func (s *StraySweeper) claimOne(ctx context.Context, entry StrayFileEntry) {
	log := log.With().Str("file_id", entry.FileID).Logger()

	// Pick the first holder that has a hostname.
	var target *ProviderRef
	for i := range entry.Holders {
		if entry.Holders[i].Hostname != "" {
			target = &entry.Holders[i]
			break
		}
	}
	if target == nil {
		log.Warn().Int("holders", len(entry.Holders)).Msg("No reachable holder for stray file")
		return
	}

	// Download the file from the holder provider's HTTP API.
	downloadURL := fmt.Sprintf("http://%s/api/v1/download/%s", target.Hostname, entry.FileID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, downloadURL, nil)
	if err != nil {
		log.Err(err).Str("url", downloadURL).Msg("Failed to create download request")
		return
	}

	resp, err := s.httpClient.Do(req)
	if err != nil {
		log.Err(err).Str("holder", target.Hostname).Msg("Failed to download stray file")
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Warn().Str("holder", target.Hostname).Int("status", resp.StatusCode).Msg("Holder returned non-OK status")
		return
	}

	// Claim the file — this stores it locally and submits an initial proof.
	metadata, err := s.sm.ClaimFile(ctx, entry.FileID, entry.FileName, resp.Body, entry.Size)
	if err != nil {
		log.Err(err).Msg("Failed to claim stray file")
		return
	}

	log.Info().
		Str("file_name", metadata.FileName).
		Int64("size", metadata.Size).
		Int("chunks", metadata.Chunks).
		Str("merkle_root", metadata.MerkleRoot).
		Msg("Stray file claimed successfully")
}

// claimScore returns a deterministic uint64 from (fileID, providerAddr).
// Lower scores are claimed first. Different (fileID, provider) pairs produce
// different orderings, naturally distributing files across providers.
func claimScore(fileID, providerAddr string) uint64 {
	// TODO: can I use XXH3 instead?
	h := blake3.New()
	_, _ = h.Write([]byte(fileID))
	_, _ = h.Write([]byte(providerAddr))
	sum := h.Sum(nil)
	return binary.LittleEndian.Uint64(sum[:8])
}
