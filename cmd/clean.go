package cmd

import (
	"context"
	"fmt"
	"os"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/spf13/cobra"

	"cygnus/atlas"
	"cygnus/cmd/types"
	"cygnus/config"
	"cygnus/storage"
)

func CleanCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "clean",
		Short: "Remove local files not proved for 3+ proof windows",
		Long: `Scans all locally stored files and removes those whose last proof
was more than 3 proof-window blocks ago. Frees disk space from
files that are no longer being challenged on-chain.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			home, err := cmd.Flags().GetString(types.FlagHome)
			if err != nil {
				return err
			}

			cw := zerolog.ConsoleWriter{Out: os.Stderr}
			log.Logger = zerolog.New(cw).With().Timestamp().Caller().Logger()

			cfg, err := config.Init(home)
			if err != nil {
				return fmt.Errorf("failed to load config: %w", err)
			}

			am, err := atlas.NewAtlasManager(cfg)
			if err != nil {
				return err
			}
			defer func() { _ = am.Close() }()

			if err := am.ConnectGRPC(); err != nil {
				return fmt.Errorf("failed to connect gRPC: %w", err)
			}

			ctx := context.Background()

			if err := am.RefreshStorageParams(ctx); err != nil {
				log.Warn().Err(err).Msg("Failed to fetch storage params from chain; using defaults")
			}

			currentHeight, err := am.GetLatestBlockHeight(ctx)
			if err != nil {
				return fmt.Errorf("failed to get current block height: %w", err)
			}

			proofWindowBlocks := int64(am.GetProofWindowBlocks())
			if proofWindowBlocks <= 0 {
				proofWindowBlocks = 180
			}

			sm, err := storage.NewStorageManager(cfg, am)
			if err != nil {
				return fmt.Errorf("failed to initialize storage: %w", err)
			}
			defer sm.Close()

			fmt.Printf("Current block height:  %d\n", currentHeight)
			fmt.Printf("Proof window blocks:   %d\n", proofWindowBlocks)
			fmt.Printf("Threshold (2 windows): %d blocks\n", 2*proofWindowBlocks)

			cleaned := sm.CleanStaleFiles(ctx, currentHeight, proofWindowBlocks)
			fmt.Printf("\nCleaned %d stale file(s)\n", cleaned)
			return nil
		},
	}

	return cmd
}
