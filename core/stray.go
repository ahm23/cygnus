package core

import (
	"context"
	"encoding/binary"
	"fmt"
	"net/http"
	"sort"
	"time"

	storageTypes "atlas/x/storage/types"

	"cygnus/atlas"
	"cygnus/config"
	"cygnus/storage"

	"github.com/cosmos/cosmos-sdk/types/query"
	"github.com/rs/zerolog/log"
	"github.com/zeebo/blake3"
)

// StraySweeper periodically discovers STRAY files on the chain and claims them
// with an initial proof (first chunk, no challenge), replicating the same
// flow used in CreateFile / ClaimFile.
type StraySweeper struct {
	cfg        *config.StraySweepConfig
	sm         *storage.StorageManager
	am         *atlas.AtlasManager
	httpClient *http.Client
}

// NewStraySweeper creates a sweeper that queries the chain's Strays RPC directly.
func NewStraySweeper(cfg *config.StraySweepConfig, sm *storage.StorageManager, am *atlas.AtlasManager) *StraySweeper {
	return &StraySweeper{
		cfg: cfg,
		sm:  sm,
		am:  am,
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
	resp, err := s.am.QueryClients.Storage.Strays(ctx, &storageTypes.QueryStraysRequest{
		Pagination: &query.PageRequest{
			Limit: uint64(s.cfg.MaxClaimsPerSweep),
		},
	})
	if err != nil {
		log.Warn().Err(err).Msg("Failed to query stray files")
		return
	}
	if len(resp.Files) == 0 {
		return
	}

	if s.am.Wallet == nil {
		log.Warn().Msg("Stray sweeper: wallet not connected")
		return
	}
	providerAddr := s.am.Wallet.GetAddress()

	// Filter out files already held by this provider.
	files := make([]*storageTypes.File, 0, len(resp.Files))
	for _, f := range resp.Files {
		alreadyHeld := false
		for _, p := range f.Providers {
			if p == providerAddr {
				alreadyHeld = true
				break
			}
		}
		if !alreadyHeld {
			files = append(files, f)
		}
	}
	if len(files) == 0 {
		return
	}

	// score each file deterministically so different providers
	// naturally claim different subsets.
	type scoredFile struct {
		file  *storageTypes.File
		score uint64
	}
	scored := make([]scoredFile, len(files))
	for i, f := range files {
		scored[i] = scoredFile{file: f, score: claimScore(f.Fid, providerAddr)}
	}
	sort.Slice(scored, func(i, j int) bool {
		return scored[i].score < scored[j].score
	})

	// take the top K.
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
		go func(file *storageTypes.File) {
			defer func() { <-sem }()
			s.claimOne(ctx, file)
		}(cf.file)
	}

	// drain the semaphore to wait for all in-flight claims to finish.
	for i := 0; i < cap(sem); i++ {
		sem <- struct{}{}
	}
}

// claimOne downloads a stray file from one of its holders and claims it locally.
func (s *StraySweeper) claimOne(ctx context.Context, file *storageTypes.File) {
	log := log.With().Str("file_id", file.Fid).Logger()

	if len(file.Providers) == 0 {
		log.Warn().Msg("Stray file has no providers to download from")
		return
	}

	for _, addr := range file.Providers {
		holderHostname, _ := s.am.GetProviderHostname(addr)
		if holderHostname == "" {
			continue
		}

		downloadURL := fmt.Sprintf("https://%s/api/v1/download/%s", holderHostname, file.Fid)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, downloadURL, nil)
		if err != nil {
			log.Warn().Err(err).Str("provider", addr).Msg("Failed to create download request")
			continue
		}

		resp, err := s.httpClient.Do(req)
		if err != nil {
			log.Warn().Err(err).Str("provider", addr).Msg("Failed to download stray file")
			continue
		}

		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			log.Warn().Str("provider", addr).Int("status", resp.StatusCode).Msg("Holder returned non-OK status")
			continue
		}

		metadata, err := s.sm.ClaimFile(ctx, file.Fid, file.Fid, resp.Body, file.FileSize)
		resp.Body.Close()
		if err != nil {
			log.Warn().Err(err).Str("provider", addr).Msg("Failed to claim from holder, trying next")
			continue
		}

		log.Info().
			Int64("size", metadata.Size).
			Int("chunks", metadata.Chunks).
			Str("merkle_root", metadata.MerkleRoot).
			Str("from", addr).
			Msg("Stray file claimed successfully")
		return
	}

	log.Warn().Int("providers", len(file.Providers)).Msg("All holders failed for stray file")
}

// claimScore returns a deterministic uint64 from (fileID, providerAddr).
// Lower scores are claimed first. Different (fileID, provider) pairs produce
// different orderings, naturally distributing files across providers.
func claimScore(fileID, providerAddr string) uint64 {
	// avoid Hasher allocation for this two-element hash
	var buf [32]byte
	hasher := blake3.New()
	_, _ = hasher.WriteString(fileID)
	_, _ = hasher.WriteString(providerAddr)
	hasher.Sum(buf[:0])
	return binary.LittleEndian.Uint64(buf[:8])
}
