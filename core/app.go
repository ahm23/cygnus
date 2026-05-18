package core

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/rs/zerolog/log"

	"cygnus/api"
	"cygnus/atlas"
	"cygnus/config"
	"cygnus/storage"

	storageTypes "atlas/x/storage/types"
)

type App struct {
	cfg            *config.Config
	home           string
	api            *api.API
	atlas          *atlas.AtlasManager
	storageManager *storage.StorageManager
	eventListener  *atlas.EventListener
	eventCancel    context.CancelFunc
	chainReceiver  *chainEventReceiver
}

func NewApp(home string) (*App, error) {
	cfg, err := config.Init(home)
	if err != nil {
		return nil, err
	}

	dataDir := os.ExpandEnv(cfg.DataDirectory)
	if err := os.MkdirAll(dataDir, os.ModePerm); err != nil {
		return nil, err
	}

	// initialize managers
	am, err := atlas.NewAtlasManager(cfg)
	if err != nil {
		return nil, err
	}

	sm, err := storage.NewStorageManager(cfg, am)
	if err != nil {
		return nil, err
	}

	// initialize api server
	apiServer := api.NewAPI(&cfg.APICfg)
	apiServer.SetupRoutes(cfg, am, sm)

	// initialize event listener
	receiver := &chainEventReceiver{
		atlas:   am,
		storage: sm,
	}
	eventListener, err := atlas.NewEventListener(cfg, receiver)
	if err != nil {
		log.Warn().Err(err).Msg("Chain event listener not started (RPC may be unavailable)")
		eventListener = nil
	}

	return &App{
		cfg:            cfg,
		home:           home,
		atlas:          am,
		api:            apiServer,
		storageManager: sm,
		eventListener:  eventListener,
		chainReceiver:  receiver,
	}, nil
}

// Start starts the storage provider.
func (app *App) Start() error {
	defer app.storageManager.Close()
	defer app.atlas.Close()
	log.Info().Msg("Starting Cygnus...")
	log.Debug().Object("config", app.cfg).Msg("cygnus config")

	// create app context
	ctx, cancel := context.WithCancel(context.Background())
	app.eventCancel = cancel

	// establish gRPC connection & initialize an Atlas Protocol wallet
	if err := app.atlas.ConnectGRPC(); err != nil {
		return err
	}
	if err := app.atlas.ConnectWallet(); err != nil {
		return err
	}
	log.Debug().Msg("wallet connected")

	// fetch storage module params from chain
	if err := app.atlas.RefreshStorageParams(ctx); err != nil {
		log.Warn().Err(err).Msg("Failed to fetch storage module params; using defaults")
	}

	// validate that the provider is registered on-chain
	if err := app.ensureProviderRegistration(ctx); err != nil {
		return err
	}

	// start Cygnus API server
	go app.api.Serve()
	defer app.api.Close()
	log.Debug().Msg("api server started")

	// start Atlas Protocol block height polling
	go app.atlas.PollBlockHeight(ctx, app.blockEventHandler)

	// start Atlas event listener
	if app.eventListener != nil {
		go func() {
			if err := app.eventListener.Start(ctx); err != nil && ctx.Err() == nil {
				log.Error().Err(err).Msg("Chain event listener stopped with error")
			}
		}()
	}
	log.Debug().Msg("atlas event listener started")

	// create & configure shutdown signal
	shutdown := make(chan os.Signal, 1)
	defer signal.Stop(shutdown)
	signal.Notify(shutdown, syscall.SIGINT, syscall.SIGTERM)

	// await shutdown
	log.Info().Msg("Cygnus is READY!")
	<-shutdown

	// shutdown proceedure
	log.Info().Msg("Shutting Cygnus down safely...")
	if app.eventCancel != nil {
		app.eventCancel()
	}
	if app.eventListener != nil {
		app.eventListener.Stop()
	}
	return nil
}

// blockEventHandler is meant to be run at every height to handle
// height-dependent actions such as challenge round start actions.
func (app *App) blockEventHandler(ctx context.Context, height int64) {
	app.chainReceiver.OnNewBlock(ctx, height)
	proofRoundBlocks := int64(app.cfg.ChainCfg.ProofRoundBlocks)
	if proofRoundBlocks == 0 {
		proofRoundBlocks = 180
	}

	roundHeight := height - 1
	if height >= 0 && roundHeight%proofRoundBlocks == 0 {
		round := strconv.FormatInt(roundHeight/proofRoundBlocks, 10)
		if err := app.chainReceiver.OnStartProofRound(ctx, roundHeight, round); err != nil {
			log.Error().
				Int64("height", roundHeight).
				Str("round", round).
				Err(err).
				Msg("new challenge round handler failed")
		}
	}
}

// ensureProviderRegistration queries Atlas Protocol to determine whether this instance of Cygnus
// is a registered provider on-chain.
func (app *App) ensureProviderRegistration(ctx context.Context) error {
	queryProviderParams := &storageTypes.QueryProviderRequest{
		Address: app.atlas.Wallet.GetAddress(),
	}

	res, err := app.atlas.QueryClients.Storage.Provider(ctx, queryProviderParams)
	if err != nil {
		log.Info().Err(err).Msg("Failed to query storage provider information.")
		return err
	}

	if res.Provider == nil {
		log.Info().Err(err).Msg("Provider does not exist on network. Creating storage provider...")
		if err := app.initProviderOnChain(); err != nil {
			log.Error().Err(err).Msg("Failed to create storage provider:")
			return err
		}
		return nil
	}

	log.Debug().
		Str("address", res.Provider.Address).
		Str("hostname", res.Provider.Hostname).
		Int64("created_at", res.Provider.CreatedAt).
		Int64("space_available", res.Provider.SpaceAvailable).
		Int64("space_used", res.Provider.SpaceUsed).
		Msg("Provider information:")

	// TODO: add a "run `cygnus sync` to sync local config to on-chain parameters", once cygnus sync is implemented
	if res.Provider.Hostname != app.cfg.Ip {
		log.Warn().
			Str("chain_hostname", res.Provider.Hostname).
			Str("configured_hostname", app.cfg.Ip).
			Msg("Provider hostname differs from local config")
	}
	if res.Provider.SpaceAvailable > app.cfg.TotalSpace {
		log.Warn().
			Int64("chain_space_available", res.Provider.SpaceAvailable).
			Int64("configured_total_space", app.cfg.TotalSpace).
			Msg("Configured total space is less than on-chain available space")
	}

	return nil
}

// initProviderOnChain registers the provider's information on-chain.
func (app *App) initProviderOnChain() error {
	msg := &storageTypes.MsgRegisterProvider{
		Creator:  app.atlas.Wallet.GetAddress(),
		Hostname: app.cfg.Ip,
		Capacity: app.cfg.TotalSpace,
	}

	resp, err := app.atlas.Wallet.BroadcastTxGrpc(3, true, msg)
	if err != nil {
		return fmt.Errorf("failed to broadcast transaction: %w", err)
	}
	if resp.Code != 0 {
		return fmt.Errorf("transaction failed: %s", resp.RawLog)
	}

	log.Info().Str("Tx Hash", resp.TxHash).Msg("Provider registered!")
	return nil
}
