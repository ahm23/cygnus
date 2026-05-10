package core

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/rs/zerolog/log"
	"go.uber.org/zap"

	"cygnus/api"
	"cygnus/atlas"
	"cygnus/config"
	"cygnus/storage"

	storageTypes "atlas/x/storage/types"
)

type App struct {
	cfg            *config.Config
	log            *zap.Logger
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

	logger, err := zap.NewDevelopment()
	if err != nil {
		return nil, err
	}

	dataDir := os.ExpandEnv(cfg.DataDirectory)
	if err := os.MkdirAll(dataDir, os.ModePerm); err != nil {
		return nil, err
	}

	// === initialize managers
	am, err := atlas.NewAtlasManager(cfg, logger)
	if err != nil {
		return nil, err
	}

	sm, err := storage.NewStorageManager(cfg, logger, am)
	if err != nil {
		return nil, err
	}

	// === initialize api server & rpc socket listeners
	apiServer := api.NewAPI(&cfg.APICfg)
	apiServer.SetupRoutes(cfg, logger, am, sm)

	receiver := &chainEventReceiver{atlas: am, storage: sm, logger: logger}
	eventListener, err := atlas.NewEventListener(cfg, logger, receiver)
	if err != nil {
		log.Warn().Err(err).Msg("Chain event listener not started (RPC may be unavailable)")
		eventListener = nil
	}

	return &App{
		cfg:            cfg,
		log:            logger,
		home:           home,
		atlas:          am,
		api:            apiServer,
		storageManager: sm,
		eventListener:  eventListener,
		chainReceiver:  receiver,
	}, nil
}

func (app *App) Start() error {
	log.Info().Msg("Starting Cygnus...")
	log.Debug().Object("config", app.cfg).Msg("cygnus config")

	if err := app.atlas.ConnectGRPC(); err != nil {
		return err
	}
	if err := app.atlas.ConnectWallet(); err != nil {
		return err
	}

	if err := app.ensureProviderRegistration(context.Background()); err != nil {
		return err
	}

	app.log.Info("Starting API Server...", zap.Int64("port", app.cfg.APICfg.Port))
	go app.api.Serve()

	ctx, cancel := context.WithCancel(context.Background())
	app.eventCancel = cancel

	if app.eventListener != nil {
		go func() {
			if err := app.eventListener.Start(ctx); err != nil && ctx.Err() == nil {
				app.log.Error("Chain event listener stopped with error", zap.Error(err))
			}
		}()
	}
	go app.pollChallengeRounds(ctx)

	done := make(chan os.Signal, 1)
	defer signal.Stop(done)

	signal.Notify(done, syscall.SIGINT, syscall.SIGTERM)
	<-done

	fmt.Println("Shutting cygnus down safely...")

	if app.eventCancel != nil {
		app.eventCancel()
	}
	if app.eventListener != nil {
		app.eventListener.Stop()
	}
	_ = app.storageManager.Close()
	_ = app.api.Close()
	_ = app.atlas.Close()
	return nil
}

func (app *App) pollChallengeRounds(ctx context.Context) {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()

	var lastSeenHeight int64
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		latestHeight, err := app.atlas.LatestBlockHeight(ctx)
		if err != nil {
			app.log.Warn("Challenge round poll failed", zap.Error(err))
			continue
		}

		if latestHeight <= lastSeenHeight {
			continue
		}
		if lastSeenHeight == 0 {
			lastSeenHeight = latestHeight - 1
		}

		if err := app.chainReceiver.OnNewBlock(ctx, latestHeight); err != nil {
			app.log.Warn("Challenge round poll block update failed",
				zap.Int64("height", latestHeight),
				zap.Error(err))
		}

		for height := lastSeenHeight + 1; height <= latestHeight; height++ {
			if height <= 0 || height%challengeRoundBlocks != 0 {
				continue
			}

			round := strconv.FormatInt(height/challengeRoundBlocks, 10)
			app.log.Info("Challenge round detected by RPC poller",
				zap.Int64("height", height),
				zap.String("round", round))
			if err := app.chainReceiver.OnStartProofRound(ctx, height, round); err != nil {
				app.log.Error("Challenge round poll handler failed",
					zap.Int64("height", height),
					zap.String("round", round),
					zap.Error(err))
			}
		}

		lastSeenHeight = latestHeight
	}
}

func (app *App) ensureProviderRegistration(ctx context.Context) error {
	queryProviderParams := &storageTypes.QueryProviderRequest{
		Address: app.atlas.Wallet.GetAddress(),
	}
	cl := app.atlas.QueryClients.Storage

	res, err := cl.Provider(ctx, queryProviderParams)
	if err != nil || res.Provider == nil {
		log.Info().Err(err).Msg("Provider does not exist on network or is not connected...")
		if err := initProviderOnChain(app.atlas.Wallet, app.cfg.Ip, app.cfg.TotalSpace); err != nil {
			log.Error().Err(err)
			return err
		}
		app.storageManager.RecordChainSync(time.Now().UTC())
		return nil
	}

	app.storageManager.RecordChainSync(time.Now().UTC())
	app.log.Info("Provider query result",
		zap.String("address", res.Provider.Address),
		zap.String("hostname", res.Provider.Hostname),
		zap.Int64("created_at", res.Provider.CreatedAt),
		zap.Int64("space_available", res.Provider.SpaceAvailable),
		zap.Int64("space_used", res.Provider.SpaceUsed))

	if res.Provider.Hostname != app.cfg.Ip {
		app.log.Warn("Provider hostname differs from local config",
			zap.String("chain_hostname", res.Provider.Hostname),
			zap.String("configured_hostname", app.cfg.Ip))
	}
	if res.Provider.SpaceAvailable > app.cfg.TotalSpace {
		app.log.Warn("Configured total space is lower than on-chain available space",
			zap.Int64("chain_space_available", res.Provider.SpaceAvailable),
			zap.Int64("configured_total_space", app.cfg.TotalSpace))
	}

	return nil
}

func initProviderOnChain(wallet *atlas.AtlasWallet, ip string, totalSpace int64) error {
	msg := &storageTypes.MsgRegisterProvider{
		Creator:  wallet.GetAddress(),
		Hostname: ip,
		Capacity: totalSpace,
	}

	resp, err := wallet.BroadcastTxGrpc(3, true, msg)
	if err != nil {
		return fmt.Errorf("failed to broadcast transaction: %w", err)
	}
	if resp.Code != 0 {
		return fmt.Errorf("transaction failed: %s", resp.RawLog)
	}

	fmt.Printf("Provider registered! Tx hash: %s\n", resp.TxHash)
	return nil
}
