package core

import (
	"context"

	"github.com/rs/zerolog/log"

	"cygnus/config"
	"cygnus/wallet"

	storageTypes "nebulix/x/storage/types"
)

type App struct {
	home string
	// api    *api.API
	wallet *wallet.Wallet
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

	ctx := context.Background()

	// dataDir := os.ExpandEnv(cfg.DataDirectory)

	// err = os.MkdirAll(dataDir, os.ModePerm)
	// if err != nil {
	// 	return nil, err
	// }

	// db, err := utils.OpenBadger(dataDir)
	// if err != nil {
	// 	return nil, err
	// }

	// ds, err := ipfs.NewBadgerDataStore(db)
	// if err != nil {
	// 	return nil, err
	// }
	// log.Info().Msg("Data store initialized")

	// bsDir := os.ExpandEnv(cfg.BlockStoreConfig.Directory)
	// var bs blockstore.Blockstore
	// bs = nil
	// switch cfg.BlockStoreConfig.Type {
	// case config.OptBadgerDS:
	// case config.OptFlatFS:
	// 	bs, err = ipfs.NewFlatfsBlockStore(bsDir)
	// 	if err != nil {
	// 		return nil, err
	// 	}
	// }
	// log.Info().Msg("Blockstore initialized")

	// apiServer := api.NewAPI(&cfg.APICfg)

	// pprofServer := monitoring.NewPProf("localhost:6060")

	w, err := config.InitWallet(home)
	if err != nil {
		return nil, err
	}
	log.Info().Str("provider_address", w.Address).Send()

	// f, err := file_system.NewFileSystem(ctx, db, cfg.BlockStoreConfig.Key, ds, bs, cfg.APICfg.IPFSPort, cfg.APICfg.IPFSDomain)
	// if err != nil {
	// 	return nil, err
	// }
	// log.Info().Msg("File system initialized")

	return &App{
		// fileSystem:  f,
		// api:         apiServer,
		// pprofServer: pprofServer,
		home:   home,
		wallet: w,
	}, nil
}

func (a *App) Start() error {
	cfg, err := config.Init(a.home)
	if err != nil {
		return err
	}
	log.Info().Msg("Starting Cygnus...")
	log.Debug().Object("config", cfg).Msg("cygnus config")

	// claimers := make([]string, 0)

	queryProviderParams := &storageTypes.QueryProviderRequest{
		Address: a.wallet.Address.String(),
	}
	cl := a.wallet.QueryClient.Storage

	res, err := cl.Provider(context.Background(), queryProviderParams)
	if err != nil {
		log.Info().Err(err).Msg("Provider does not exist on network or is not connected...")
		err := initProviderOnChain(a.wallet, cfg.Ip, cfg.TotalSpace)
		if err != nil {
			return err
		}
	} else {
		log.Debug().
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
		// 	err := updateSpace(a.wallet, cfg.TotalSpace)
		// 	if err != nil {
		// 		return err
		// 	}
		// }
		// if res.Provider.Ip != cfg.Ip {
		// 	err := updateIp(a.wallet, cfg.Ip)
		// 	if err != nil {
		// 		return err
		// 	}
		// }
	}

	// params, err := a.GetStorageParams(a.wallet.Client.GRPCConn)
	// if err != nil {
	// 	return err
	// }

	// a.q = queue.NewQueue(a.wallet, cfg.QueueInterval, cfg.MaxSizeBytes, cfg.Ip, cfg.QueueRateLimit)
	// go a.q.Listen()

	// prover := proofs.NewProver(a.wallet, a.q, a.fileSystem, cfg.ProofInterval, cfg.ProofThreads, int(params.ChunkSize))

	// myUrl := cfg.Ip

	// log.Info().Msg(fmt.Sprintf("Provider started as: %s", myAddress))

	// a.prover = prover
	// a.strayManager = strays.NewStrayManager(a.wallet, a.q, cfg.StrayManagerCfg.CheckInterval, cfg.StrayManagerCfg.RefreshInterval, cfg.StrayManagerCfg.HandCount, claimers)
	// a.monitor = monitoring.NewMonitor(a.wallet)

	// // Starting the 4 concurrent services
	// if cfg.APICfg.IPFSSearch {
	// 	// nolint:all
	// 	go a.ConnectPeers()
	// }
	// go a.api.Serve(a.fileSystem, a.prover, a.wallet, params.ChunkSize, myUrl)
	// go a.prover.Start()
	// go a.strayManager.Start(a.fileSystem, a.q, myUrl, params.ChunkSize)
	// go a.monitor.Start()
	// go a.pprofServer.Start()

	// done := make(chan os.Signal, 1)
	// defer signal.Stop(done) // undo signal.Notify effect

	// signal.Notify(done, syscall.SIGINT, syscall.SIGTERM)
	// <-done // Will block here until user hits ctrl+c

	// fmt.Println("Shutting cygnus down safely...")

	// _ = a.api.Close()
	// a.q.Stop()
	// a.prover.Stop()
	// a.strayManager.Stop()
	// a.monitor.Stop()

	// time.Sleep(time.Second * 30) // give the program some time to shut down
	// a.fileSystem.Close()
	// _ = a.pprofServer.Stop()

	return nil
}

func initProviderOnChain(wallet *wallet.Wallet, ip string, totalSpace int64) error {
	msg := storageTypes.MsgRegisterProvider{
		Hostname: "test",
		Capacity: 10000000000,
	}

	_, err := wallet.MsgClient.Storage.RegisterProvider(
		context.Background(),
		&msg,
	)

	return err
}
