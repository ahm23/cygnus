package atlas

import (
	"context"
	"fmt"
	"os"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"

	"github.com/cosmos/cosmos-sdk/client"
	cmtservice "github.com/cosmos/cosmos-sdk/client/grpc/cmtservice"
	banktypes "github.com/cosmos/cosmos-sdk/x/bank/types"
	"github.com/rs/zerolog"

	"atlas/app"
	storagetypes "atlas/x/storage/types"

	"cygnus/config"
	"cygnus/types"
)

type AtlasManager struct {
	cfg       *config.Config
	log       zerolog.Logger
	clientCtx client.Context
	cmtClient cmtservice.ServiceClient

	Height       int64
	Wallet       *AtlasWallet
	QueryClients types.QueryClients
}

type MsgClients struct {
	Bank    banktypes.MsgClient
	Storage storagetypes.MsgClient
}

func NewAtlasManager(cfg *config.Config, logger zerolog.Logger) (*AtlasManager, error) {
	// use Atlas Protocol encoding config
	encodingConfig := app.MakeEncodingConfig()

	// create client context
	clientCtx := client.Context{}.
		WithHomeDirectory(cfg.HomeDirectory).
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
		cfg:       cfg,
		log:       logger,
		clientCtx: clientCtx,
	}

	return am, nil
}

// ConnectGRPC establishes gRPC connection and initializes query clients.
func (am *AtlasManager) ConnectGRPC() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// establish gRPC connection
	conn, err := grpc.DialContext(
		ctx,
		am.cfg.ChainCfg.GRPCAddr,
		grpc.WithInsecure(), // TODO: use grpc.WithTransportCredentials(insecure.NewCredentials()) for newer grpc
		grpc.WithBlock(),
	)
	if err != nil {
		return fmt.Errorf("failed to connect to GRPC endpoint: %w", err)
	}

	am.cmtClient = cmtservice.NewServiceClient(conn)
	am.clientCtx = am.clientCtx.WithGRPCClient(conn)

	// initialize query clients
	am.QueryClients = types.QueryClients{
		// Auth:    authtypes.NewQueryClient(conn),		// TODO: to be used for authz extensions?
		Bank:    banktypes.NewQueryClient(conn),
		Storage: storagetypes.NewQueryClient(conn),
	}

	return nil
}

// ConnectWallet creates and initializes the wallet handler.
func (am *AtlasManager) ConnectWallet() error {
	wallet, err := NewAtlasWallet(am.cfg, am.log, &am.clientCtx, &am.QueryClients, "cygnus", am.cfg.HomeDirectory)
	am.Wallet = wallet
	return err
}

// TEMP: for stress test
func (am *AtlasManager) ConnectWalletWithKeyNameAndSource(keyName, keySource string) error {
	wallet, err := NewAtlasWallet(am.cfg, am.log, &am.clientCtx, &am.QueryClients, keyName, keySource)
	am.Wallet = wallet
	return err
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
			am.log.Warn().Err(err).Msg("Challenge round poll failed")
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

// Close closes the GRPC connection
func (am *AtlasManager) Close() error {
	if am.Wallet != nil {
		am.Wallet.Stop()
	}

	am.clientCtx.GRPCClient.Close()

	return nil
}
