package cmd

import (
	"fmt"
	"os"
	"path/filepath"

	"cygnus/config"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

const (
	FlagHome     = "home"
	FlagLogLevel = "log-level"

	DefaultHome     = "$HOME/.cygnus"
	DefaultLogLevel = "info"
)

func init() {
	log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr})
	log.Logger = log.With().Caller().Logger()
	log.Logger = log.Level(zerolog.InfoLevel)
}

func InitCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Initialize cygnus config folder and wallet",
		Long: `Initialize the storage provider by creating configuration files
and setting up a wallet. If a wallet already exists, it will be loaded.`,
		Example: `  cygnus init --home ~/.cygnus
  cygnus init --home /path/to/provider`,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Get home directory
			home, err := cmd.Flags().GetString(FlagHome)
			if err != nil {
				return err
			}

			// Expand home directory path
			if home == "" {
				homeDir, err := os.UserHomeDir()
				if err != nil {
					return fmt.Errorf("failed to get home directory: %w", err)
				}
				home = filepath.Join(homeDir, ".cygnus")
			}

			fmt.Printf("Initializing provider at: %s\n", home)

			// Initialize config
			cfg, err := config.Init(home)
			if err != nil {
				return fmt.Errorf("failed to initialize config: %w", err)
			}
			fmt.Println("✓ Configuration initialized")

			// Initialize wallet
			walletInfo, err := config.InitWallet(home)
			if err != nil {
				return fmt.Errorf("failed to initialize wallet: %w", err)
			}
			fmt.Printf("✓ Wallet initialized: %s (%s)\n", walletInfo.Name, walletInfo.Address)

			// Summary
			fmt.Println("\n" + "==============")
			fmt.Println("Provider Initialization Complete!")
			fmt.Println("===============")
			fmt.Printf("Home Directory: %s\n", cfg.HomeDir)
			fmt.Printf("Wallet: %s (%s)\n", walletInfo.Name, walletInfo.Address)
			// fmt.Printf("Storage Path: %s\n", cfg.StoragePath)
			// fmt.Printf("Max Storage: %d GB\n", cfg.MaxStorageGB)
			fmt.Println("===============")

			return nil
		},
	}

	// Add flags
	cmd.Flags().String(FlagHome, "", "Home directory for config and data (default: $HOME/.cygnus)")

	return cmd
}

func VersionCmd() *cobra.Command {
	r := &cobra.Command{
		Use:   "version",
		Short: "checks the version of cygnus",
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Printf("Version: %s\nCommit: %s\n", config.Version(), config.Commit())
			return nil
		},
	}

	return r
}

// RootCmd creates and returns the root Cobra command for the cygnus CLI, configuring global flags and adding all subcommands.
func RootCmd() *cobra.Command {
	r := &cobra.Command{
		Use:   "cygnus",
		Short: "cygnus is a fast and light-weight Jackal Storage Provider.",
	}

	r.PersistentFlags().String(FlagHome, DefaultHome, "sets the home directory for cygnus")
	r.PersistentFlags().String(FlagLogLevel, DefaultLogLevel, "log level. info|error|debug")

	// r.PersistentFlags().String("domain", "http://example.com", "provider domain")
	// r.PersistentFlags().Int64("api_config.port", 3333, "port to serve api requests")
	// r.PersistentFlags().Int("api_config.ipfs_port", 4005, "port for IPFS")
	// r.PersistentFlags().String("api_config.ipfs_domain", "dns4/ipfs.example.com/tcp/4001", "IPFS domain")
	// r.PersistentFlags().Bool("api_config.ipfs_search", true, "Search for IPFS connections on Jackal on startup")
	// r.PersistentFlags().Bool("api_config.open_gateway", true, "Open gateway for file retrieval even if file is on a different provider")
	// r.PersistentFlags().Int64("proof_threads", 1000, "maximum threads for proofs")
	// r.PersistentFlags().String("data_directory", "$HOME/.cygnus/data", "directory to store database files")
	// r.PersistentFlags().Int64("queue_interval", 10, "seconds to wait until next cycle to flush the transaction queue")
	// r.PersistentFlags().Int64("proof_interval", 120, "seconds to wait until next cycle to post proofs")
	// r.PersistentFlags().Int64("total_bytes_offered", 1092616192, "maximum storage space to provide in bytes")

	err := viper.BindPFlags(r.PersistentFlags())
	if err != nil {
		panic(err)
	}

	// r.AddCommand(StartCmd(), wallet.WalletCmd(), InitCmd(), VersionCmd(), IPFSCmd(), ShutdownCmd(), database.DataCmd())
	r.AddCommand(InitCmd(), VersionCmd(), StartCmd())

	return r
}

func Execute(rootCmd *cobra.Command) {
	if err := rootCmd.Execute(); err != nil {

		log.Error().Err(err)
		os.Exit(1)
	}
}
