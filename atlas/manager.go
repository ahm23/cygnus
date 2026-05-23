package atlas

import (
	"context"
	"crypto/tls"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/cosmos/cosmos-sdk/client"
	cmtservice "github.com/cosmos/cosmos-sdk/client/grpc/cmtservice"
	authtypes "github.com/cosmos/cosmos-sdk/x/auth/types"
	banktypes "github.com/cosmos/cosmos-sdk/x/bank/types"
	"github.com/rs/zerolog/log"

	"atlas/app"
	storagetypes "atlas/x/storage/types"

	"cygnus/config"
	"cygnus/types"
)

type AtlasManager struct {
	cfg       *config.Config
	clientCtx client.Context
	cmtClient cmtservice.ServiceClient

	Height       int64
	Wallet       *AtlasWallet
	QueryClients types.QueryClients

	storageParams   *storagetypes.Params
	providerCache   map[string]string // address → hostname
	providerCacheMu sync.RWMutex
}

// GetProofRoundBlocks returns the on-chain proof_round_blocks param,
// or 0 if params have not been fetched yet.
func (am *AtlasManager) GetProofRoundBlocks() uint64 {
	if am.storageParams != nil {
		return am.storageParams.ProofRoundBlocks
	}
	return 0
}

// GetProofWindowBlocks returns the on-chain proof_window_blocks param,
// or 0 if params have not been fetched yet.
func (am *AtlasManager) GetProofWindowBlocks() uint64 {
	if am.storageParams != nil {
		return am.storageParams.ProofWindowBlocks
	}
	return 0
}

type MsgClients struct {
	Bank    banktypes.MsgClient
	Storage storagetypes.MsgClient
}

func NewAtlasManager(cfg *config.Config) (*AtlasManager, error) {
	// use Atlas Protocol encoding config
	encodingConfig := app.MakeEncodingConfig()

	// create client context
	clientCtx := client.Context{}.
		WithHomeDir(cfg.HomeDirectory).
		WithChainID(cfg.ChainCfg.ChainId).
		WithInput(os.Stdin).
		WithOutput(os.Stdout).
		WithCodec(encodingConfig.Codec).
		WithInterfaceRegistry(encodingConfig.InterfaceRegistry).
		WithTxConfig(encodingConfig.TxConfig).
		WithLegacyAmino(encodingConfig.Amino).
		WithBroadcastMode("sync").
		WithSkipConfirmation(true).
		WithSignModeStr("direct")

	registerAccountInterfaces(clientCtx.InterfaceRegistry)

	// create new AtlasManager instance
	am := &AtlasManager{
		cfg:           cfg,
		clientCtx:     clientCtx,
		providerCache: make(map[string]string),
	}

	return am, nil
}

// ConnectGRPC establishes gRPC connection and initializes query clients.
func (am *AtlasManager) ConnectGRPC() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// create TLS credentials
	creds := credentials.NewTLS(&tls.Config{
		MinVersion: tls.VersionTLS12,
	})

	// establish gRPC connection
	conn, err := grpc.DialContext(
		ctx,
		am.cfg.ChainCfg.GRPCAddr,
		grpc.WithTransportCredentials(creds),
		grpc.WithBlock(),
	)
	if err != nil {
		return fmt.Errorf("failed to connect to GRPC endpoint: %w", err)
	}

	am.cmtClient = cmtservice.NewServiceClient(conn)
	am.clientCtx = am.clientCtx.WithGRPCClient(conn)

	// initialize query clients
	am.QueryClients = types.QueryClients{
		Auth:    authtypes.NewQueryClient(conn),
		Bank:    banktypes.NewQueryClient(conn),
		Storage: storagetypes.NewQueryClient(conn),
	}

	return nil
}

// ConnectWallet creates and initializes the wallet handler.
func (am *AtlasManager) ConnectWallet() error {
	wallet, err := NewAtlasWallet(am.cfg, &am.clientCtx, &am.QueryClients, "cygnus", am.cfg.HomeDirectory)
	am.Wallet = wallet
	return err
}

// TEMP: for stress test
func (am *AtlasManager) ConnectWalletWithKeyNameAndSource(keyName, keySource string) error {
	wallet, err := NewAtlasWallet(am.cfg, &am.clientCtx, &am.QueryClients, keyName, keySource)
	am.Wallet = wallet
	return err
}

// RefreshStorageParams fetches storage module on-chain params via gRPC
// and stores them on the AtlasManager. ProofRoundBlocks and ProofWindowBlocks
// are used for challenge-round scheduling everywhere else in the provider.
func (am *AtlasManager) RefreshStorageParams(ctx context.Context) error {
	res, err := am.QueryClients.Storage.Params(ctx, &storagetypes.QueryParamsRequest{})
	if err != nil {
		return fmt.Errorf("refresh storage params: %w", err)
	}

	am.storageParams = &res.Params

	log.Debug().
		Uint64("proof_window_blocks", am.storageParams.ProofWindowBlocks).
		Uint64("proof_round_blocks", am.storageParams.ProofRoundBlocks).
		Msg("Storage module params fetched from chain")

	return nil
}

// PollBlockHeight continously polls the block height at 2 second intervals.
func (am *AtlasManager) PollBlockHeight(ctx context.Context, callback func(context.Context, int64)) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		latestHeight, err := am.GetLatestBlockHeight(ctx)
		if err != nil {
			log.Warn().Err(err).Msg("Challenge round poll failed")
			continue
		}
		if latestHeight <= am.Height {
			continue
		}

		atomic.StoreInt64(&am.Height, latestHeight)
		callback(ctx, latestHeight)
	}
}

// WaitForBlockHeight waits for a given block height to be reached.
func (am *AtlasManager) WaitForBlockHeight(ctx context.Context, targetHeight int64) error {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()

	for {
		if am.Height >= targetHeight {
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// GetLatestBlockHeight gets the latest block height via gRPC
func (am *AtlasManager) GetLatestBlockHeight(ctx context.Context) (int64, error) {
	res, err := am.cmtClient.GetLatestBlock(ctx, &cmtservice.GetLatestBlockRequest{})
	if err != nil {
		return 0, fmt.Errorf("failed to get latest block height via gRPC: %w", err)
	}
	if sdkBlock := res.GetSdkBlock(); sdkBlock != nil {
		height := sdkBlock.GetHeader().Height
		if height > 0 {
			return height, nil
		}
	}
	if block := res.GetBlock(); block != nil {
		height := block.GetHeader().Height
		if height > 0 {
			return height, nil
		}
	}

	return 0, fmt.Errorf("latest block response did not include a block height")
}

// RefreshProviders queries all registered storage providers and updates the
// local cache (address → hostname). Called at startup and at every proof window
// start to keep the cache fresh.
func (am *AtlasManager) RefreshProviders(ctx context.Context) error {
	if am.QueryClients.Storage == nil {
		return fmt.Errorf("storage query client not connected")
	}

	resp, err := am.QueryClients.Storage.Providers(ctx, &storagetypes.QueryProvidersRequest{})
	if err != nil {
		return fmt.Errorf("failed to query providers: %w", err)
	}

	cache := make(map[string]string, len(resp.Providers))
	for _, p := range resp.Providers {
		if p != nil {
			cache[p.Address] = p.Hostname
		}
	}

	am.providerCacheMu.Lock()
	am.providerCache = cache
	am.providerCacheMu.Unlock()

	log.Debug().Int("providers", len(cache)).Msg("Provider cache refreshed")
	return nil
}

// GetProviderHostname returns the cached hostname for a provider address.
// Returns false if the address is not in the cache.
func (am *AtlasManager) GetProviderHostname(address string) (string, bool) {
	am.providerCacheMu.RLock()
	defer am.providerCacheMu.RUnlock()
	h, ok := am.providerCache[address]
	return h, ok
}

// Close closes the GRPC connection
func (am *AtlasManager) Close() error {
	if am.Wallet != nil {
		am.Wallet.Stop()
	}

	am.clientCtx.GRPCClient.Close()

	return nil
}
