package atlas

import (
	"context"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cosmos/cosmos-sdk/client"
	cmtservice "github.com/cosmos/cosmos-sdk/client/grpc/cmtservice"
	"go.uber.org/zap"

	storagetypes "atlas/x/storage/types"

	authtypes "github.com/cosmos/cosmos-sdk/x/auth/types"
	banktypes "github.com/cosmos/cosmos-sdk/x/bank/types"

	"google.golang.org/grpc"

	// Import from your local blockchain
	"atlas/app"

	"cygnus/config"
	"cygnus/types"
)

type AtlasManager struct {
	cfg       *config.Config
	logger    *zap.Logger
	clientCtx client.Context
	grpcConn  *grpc.ClientConn
	cmtClient cmtservice.ServiceClient

	Wallet       *AtlasWallet
	QueryClients types.QueryClients
	State        AtlasState
}

type AtlasState struct {
	mu     sync.Mutex
	Height int64
}

// MsgClients groups all message clients
type MsgClients struct {
	Bank    banktypes.MsgClient
	Storage storagetypes.MsgClient
}

func NewAtlasManager(cfg *config.Config, logger *zap.Logger) (*AtlasManager, error) {
	// Get encoding config from your blockchain
	encodingConfig := app.MakeEncodingConfig()

	// Create client context
	clientCtx := client.Context{}.
		WithHomeDir(cfg.HomeDir).
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

	// Initialize AtlasManager without GRPC connection first
	am := &AtlasManager{
		cfg:       cfg,
		logger:    logger,
		clientCtx: clientCtx,
	}

	return am, nil
}

// ConnectGRPC establishes GRPC connection and initializes clients
func (am *AtlasManager) ConnectGRPC() error {
	// Create GRPC connection
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn, err := grpc.DialContext(
		ctx,
		am.cfg.ChainCfg.GRPCAddr,
		grpc.WithInsecure(), // Use grpc.WithTransportCredentials(insecure.NewCredentials()) for newer grpc
		grpc.WithBlock(),
	)
	if err != nil {
		return fmt.Errorf("failed to connect to GRPC endpoint: %w", err)
	}

	am.grpcConn = conn
	am.cmtClient = cmtservice.NewServiceClient(conn)

	am.clientCtx = am.clientCtx.WithGRPCClient(conn)

	// Initialize query clients
	am.QueryClients = types.QueryClients{
		Auth:    authtypes.NewQueryClient(conn),
		Bank:    banktypes.NewQueryClient(conn),
		Storage: storagetypes.NewQueryClient(conn),
	}

	return nil
}

func (am *AtlasManager) ConnectWallet() error {
	return am.ConnectWalletWithKeyName("cygnus")
}

func (am *AtlasManager) ConnectWalletWithKeyName(keyName string) error {
	return am.ConnectWalletWithKeyNameAndSource(keyName, am.cfg.HomeDir)
}

func (am *AtlasManager) ConnectWalletWithKeyNameAndSource(keyName, keySource string) error {
	wallet, err := NewAtlasWalletWithKeyNameAndSource(am.cfg, am.logger, &am.clientCtx, &am.QueryClients, keyName, keySource)
	am.Wallet = wallet
	return err
}

// === PollBlockHeight continously polls the block height at 2 second intervals
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
			am.logger.Warn("Challenge round poll failed", zap.Error(err))
			continue
		}
		if latestHeight <= am.State.Height {
			continue
		}

		atomic.StoreInt64(&am.State.Height, latestHeight)
		am.logger.Debug("Block Height:", zap.Int64("height", latestHeight))
		callback(ctx, latestHeight)
	}
}

// === WaitForBlockHeight waits for a given block height to be reached
func (am *AtlasManager) WaitForBlockHeight(ctx context.Context, targetHeight int64) error {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()

	for {
		if am.State.Height >= targetHeight {
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// === GetLatestBlockHeight gets the latest block height via gRPC
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

// Close closes the GRPC connection
func (am *AtlasManager) Close() error {
	if am.Wallet != nil {
		am.Wallet.Stop()
	}

	if am.grpcConn != nil {
		return am.grpcConn.Close()
	}
	return nil
}
