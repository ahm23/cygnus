package cmd

import (
	"os"
	"time"

	"cygnus/cmd/types"
	"cygnus/core"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/spf13/cobra"
)

func StartCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "start",
		Short: "Starts the storage provider",
		RunE: func(cmd *cobra.Command, args []string) error {
			home, err := cmd.Flags().GetString(types.FlagHome)
			if err != nil {
				return err
			}

			logLevel, err := cmd.Flags().GetString(types.FlagLogLevel)
			if err != nil {
				return err
			}

			cw := zerolog.ConsoleWriter{Out: os.Stderr}
			log.Logger = zerolog.New(cw).With().Timestamp().Caller().Logger()

			switch logLevel {
			case "debug":
				log.Logger = log.Logger.Level(zerolog.DebugLevel)
			case "info":
				log.Logger = log.Logger.Level(zerolog.InfoLevel)
			case "warn":
				log.Logger = log.Logger.Level(zerolog.WarnLevel)
			case "error":
				log.Logger = log.Logger.Level(zerolog.ErrorLevel)
			}

			app, err := core.NewApp(home)
			if err != nil {
				return err
			}

			err = app.Start()

			for restartAttempt := 1; restartAttempt <= 3 && err != nil; restartAttempt++ {
				log.Err(err).Msg("Failed to start Cygnus.")
				log.Info().Msgf("Attempting restart again in %d seconds (attempt %d of %d)...\n", time.Second*5, restartAttempt, 3)
				time.Sleep(time.Second * 5)
				err = app.Start()
			}

			return err
		},
	}

	return cmd
}
