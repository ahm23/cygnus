package cmd

import (
	"context"
	"fmt"
	"os"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/spf13/cobra"

	storagetypes "atlas/x/storage/types"

	"cygnus/atlas"
	"cygnus/cmd/types"
	"cygnus/config"
)

func SyncCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Sync provider details to the blockchain using local config",
		Long: `Reads the local config (hostname/domain, total space) and updates 
the on-chain provider record via an UpdateProvider transaction.

This is useful after changing your provider configuration to push those 
changes to the blockchain.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			home, err := cmd.Flags().GetString(types.FlagHome)
			if err != nil {
				return err
			}

			cw := zerolog.ConsoleWriter{Out: os.Stderr}
			log.Logger = zerolog.New(cw).With().Timestamp().Caller().Logger()

			// load config
			cfg, err := config.Init(home)
			if err != nil {
				return fmt.Errorf("failed to load config: %w", err)
			}

			log.Info().
				Str("provider", cfg.ProviderName).
				Str("domain", cfg.Ip).
				Int64("total_space", cfg.TotalSpace).
				Str("rpc", cfg.ChainCfg.GRPCAddr).
				Msg("Loaded local config")

			// create AtlasManager and connect to chain
			am, err := atlas.NewAtlasManager(cfg)
			if err != nil {
				return fmt.Errorf("failed to create atlas manager: %w", err)
			}
			defer func() { _ = am.Close() }()

			if err := am.ConnectGRPC(); err != nil {
				return fmt.Errorf("failed to connect gRPC: %w", err)
			}

			if err := am.ConnectWallet(); err != nil {
				return fmt.Errorf("failed to connect wallet: %w", err)
			}

			address := am.Wallet.GetAddress()
			log.Info().Str("address", address).Msg("Wallet connected")

			// query existing provider from chain using the pre-configured query client
			providerResp, err := am.QueryClients.Storage.Provider(
				context.Background(),
				&storagetypes.QueryProviderRequest{Address: address},
			)
			if err != nil {
				log.Warn().Err(err).Msg("Provider not found on chain (may need to register first)")
				return fmt.Errorf("provider not found at address %s: %w", address, err)
			}

			existing := providerResp.Provider
			log.Info().
				Str("hostname", existing.Hostname).
				Int64("space_available", existing.SpaceAvailable).
				Int64("space_used", existing.SpaceUsed).
				Msg("Current on-chain provider record")

			// build the update message
			updateMsg := &storagetypes.MsgUpdateProvider{
				Creator:  address,
				Hostname: cfg.Ip,
				Capacity: cfg.TotalSpace,
			}

			changes := false
			if updateMsg.Hostname != "" && updateMsg.Hostname != existing.Hostname {
				fmt.Printf("  Hostname:     %s -> %s\n", existing.Hostname, updateMsg.Hostname)
				changes = true
			}
			if updateMsg.Capacity > 0 && updateMsg.Capacity != existing.SpaceAvailable {
				fmt.Printf("  Capacity:     %d -> %d\n", existing.SpaceAvailable, updateMsg.Capacity)
				changes = true
			}
			if !changes {
				fmt.Println("No changes detected between local config and on-chain provider record.")
				return nil
			}

			// broadcast the provdier details update
			fmt.Println("Broadcasting update...")
			resp, err := am.Wallet.BroadcastTxGrpc(3, true, updateMsg)
			if err != nil {
				return fmt.Errorf("failed to broadcast update: %w", err)
			}

			if resp.Code != 0 {
				return fmt.Errorf("update failed with code %d: %s", resp.Code, resp.RawLog)
			}

			fmt.Printf("Provider updated successfully!\n")
			fmt.Printf("  Tx Hash:  %s\n", resp.TxHash)
			fmt.Printf("  Height:   %d\n", resp.Height)

			return nil
		},
	}

	return cmd
}
