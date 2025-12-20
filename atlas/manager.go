package atlas

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/cosmos/cosmos-sdk/client"
	"go.uber.org/zap"

	storagetypes "nebulix/x/storage/types"

	authtypes "github.com/cosmos/cosmos-sdk/x/auth/types"
	banktypes "github.com/cosmos/cosmos-sdk/x/bank/types"

	"google.golang.org/grpc"

	// Import from your local blockchain
	"nebulix/app"

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

// Close closes the GRPC connection
func (am *AtlasManager) Close() error {
	if am.grpcConn != nil {
		return am.grpcConn.Close()
	}
	return nil
}
