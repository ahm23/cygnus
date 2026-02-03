package core

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/rs/zerolog/log"
	"go.uber.org/zap"

	"cygnus/api"
	"cygnus/atlas"
	"cygnus/config"
	"cygnus/storage"

	storageTypes "nebulix/x/storage/types"
)

type App struct {
	cfg            *config.Config
	log            *zap.Logger
	home           string
	api            *api.API
	atlas          *atlas.AtlasManager
	storageManager *storage.StorageManager
	// q            *queue.Queue
	// prover       *proofs.Prover
	// monitor    *monitoring.Monitor
	// fileSystem *file_system.FileSystem
}

// NewApp initializes and returns a new App instance using the provided home directory.
// It sets up configuration, data directories, database, IPFS datastore and blockstore, API server, wallet, and file system.
// Returns the initialized App or an error if any component fails to initialize.
func NewApp(home string) (*App, error) {
	cfg, err := config.Init(home)
	if err != nil {
		return nil, err
	}
	logger, err := zap.NewProduction()

	dataDir := os.ExpandEnv(cfg.DataDirectory)

	err = os.MkdirAll(dataDir, os.ModePerm)
	if err != nil {
		return nil, err
	}

	// Initialize PebbleDB for metadata
	// storageDB, err := storage.NewPebbleStore(cfg.HomeDir)
	// if err != nil {
	// 	log.Fatal().Err(err)
	// }
	// defer storageDB.Close()

	// wc, err := config.InitWallet(home)
	// if err != nil {
	// 	return nil, err
	// }
	// log.Info().Str("provider_address", wc.Address).Send()
	// log.Info().Str("home", home).Send()

	atlas, err := atlas.NewAtlasManager(cfg, logger)
	if err != nil {
		return nil, err
	}

	// Initialize storage manager
	storageManager, err := storage.NewStorageManager(cfg, logger, atlas)
	if err != nil {
		log.Fatal().Err(err)
	}

	apiServer := api.NewAPI(&cfg.APICfg)
	apiServer.SetupRoutes(cfg, logger, atlas, storageManager)

	return &App{
		cfg:            cfg,
		log:            logger,
		home:           home,
		atlas:          atlas,
		api:            apiServer,
		storageManager: storageManager,
	}, nil
}

func (app *App) Start() error {
	log.Info().Msg("Starting Cygnus...")
	log.Debug().Object("config", app.cfg).Msg("cygnus config")

	app.genTestFile()

	if err := app.atlas.ConnectGRPC(); err != nil {
		return err
	}
	if err := app.atlas.ConnectWallet(); err != nil {
		return err
	}

	queryProviderParams := &storageTypes.QueryProviderRequest{
		Address: app.atlas.Wallet.GetAddress(),
	}
	cl := app.atlas.QueryClients.Storage

	res, err := cl.Provider(context.Background(), queryProviderParams)
	if err != nil || res.Provider == nil {
		log.Info().Err(err).Msg("Provider does not exist on network or is not connected...")
		err := initProviderOnChain(app.atlas.Wallet, app.cfg.Ip, app.cfg.TotalSpace)
		if err != nil {
			log.Error().Err(err)
			return err
		}
	} else {
		log.Info().Str("res", res.String()).Send()
		log.Info().
			Str("address", res.Provider.Address).
			Str("hostname", res.Provider.Hostname).
			Int64("created_at", res.Provider.CreatedAt).
			Int64("space_available", res.Provider.SpaceAvailable).
			Int64("space_used", res.Provider.SpaceUsed).
			Msg("provider query result")

		// claimers = res.Provider.AuthClaimers

		// totalSpace, err := strconv.ParseInt(res.Provider.Totalspace, 10, 64)
		// if err != nil {
		// 	return err
		// }
		// if totalSpace != cfg.TotalSpace {
		// 	err := updateSpace(app.wallet, cfg.TotalSpace)
		// 	if err != nil {
		// 		return err
		// 	}
		// }
		// if res.Provider.Ip != cfg.Ip {
		// 	err := updateIp(app.wallet, cfg.Ip)
		// 	if err != nil {
		// 		return err
		// 	}
		// }
	}

	// Starting concurrent services
	app.log.Info("Starting API Server...")
	go app.api.Serve()
	// go app.prover.Start()
	// go app.strayManager.Start(app.fileSystem, app.q, myUrl, params.ChunkSize)

	done := make(chan os.Signal, 1)
	defer signal.Stop(done)

	signal.Notify(done, syscall.SIGINT, syscall.SIGTERM)
	<-done // Will block here until user hits ctrl+c

	fmt.Println("Shutting cygnus down safely...")

	_ = app.api.Close()
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

// [TODO]: rmeove this when doen with CLI tesdting
func (app *App) genTestFile() {
	file, _ := os.ReadFile("go.mod")

	tree, _ := app.storageManager.BuildMerkleTree(context.Background(), file)

	fmt.Printf("Merkle Root: %x\n", tree.Root)
}
