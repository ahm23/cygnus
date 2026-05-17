package cmd

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/rs/zerolog/log"
	"github.com/spf13/cobra"

	"cygnus/cmd/types"
	"cygnus/config"
)

func InitCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Initialize cygnus config folder and wallet",
		Long: `Initialize the storage provider by creating configuration files
		and setting up a wallet. If a wallet already exists, it will be loaded.`,
		Example: `  cygnus init --home ~/.cygnus
  	cygnus init --home /path/to/provider`,
		RunE: func(cmd *cobra.Command, args []string) error {
			home, err := cmd.Flags().GetString(types.FlagHome)
			if err != nil {
				return err
			}
			if home == "" {
				home, err = os.UserHomeDir()
				if err != nil {
					return fmt.Errorf("failed to get home directory: %w", err)
				}
				home = filepath.Join(home, ".cygnus")
			}

			log.Info().Msgf("Initializing provider at: %s\n", home)
			cfg, err := config.Init(home)
			if err != nil {
				return fmt.Errorf("failed to initialize config: %w", err)
			}
			wallet, err := config.InitWallet(home)
			if err != nil {
				return fmt.Errorf("failed to initialize wallet: %w", err)
			}

			log.Info().
				Str("home_dir", cfg.HomeDirectory).
				Str("address", wallet.Address).
				Msg("Provider initialization completed successfuly!")

			return nil
		},
	}

	cmd.Flags().String(types.FlagHome, "", "Home directory for config and data (default: $HOME/.cygnus)")

	return cmd
}
