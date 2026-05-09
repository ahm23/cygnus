package atlas

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	rpchttp "github.com/cometbft/cometbft/rpc/client/http"
	"github.com/cosmos/cosmos-sdk/client"
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

	Wallet       *AtlasWallet
	QueryClients types.QueryClients
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
	wallet, err := NewAtlasWallet(am.cfg, am.logger, &am.clientCtx, &am.QueryClients)
	am.Wallet = wallet
	return err
}

// WaitForNextBlock waits until the chain has produced a block after the
// currently observed height.
func (am *AtlasManager) WaitForNextBlock(ctx context.Context) error {
	rpcAddr := strings.TrimSuffix(am.cfg.ChainCfg.RPCAddr, "/")
	if !strings.HasPrefix(rpcAddr, "http://") && !strings.HasPrefix(rpcAddr, "https://") {
		rpcAddr = "http://" + rpcAddr
	}

	client, err := rpchttp.New(rpcAddr, "/websocket")
	if err != nil {
		return fmt.Errorf("failed to create RPC client: %w", err)
	}

	status, err := client.Status(ctx)
	if err != nil {
		return fmt.Errorf("failed to get current block height: %w", err)
	}

	targetHeight := status.SyncInfo.LatestBlockHeight + 1
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			status, err := client.Status(ctx)
			if err != nil {
				return fmt.Errorf("failed to get latest block height: %w", err)
			}
			if status.SyncInfo.LatestBlockHeight >= targetHeight {
				am.logger.Debug("Observed next block",
					zap.Int64("target_height", targetHeight),
					zap.Int64("latest_height", status.SyncInfo.LatestBlockHeight))
				return nil
			}
		}
	}
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
