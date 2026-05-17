package cmd

import (
	"fmt"
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
		Short: "Starts the provider",
		RunE: func(cmd *cobra.Command, args []string) error {
			home, err := cmd.Flags().GetString(types.FlagHome)
			if err != nil {
				return err
			}

			logLevel, err := cmd.Flags().GetString(types.FlagLogLevel)
			if err != nil {
				return err
			}

			switch logLevel {
			case "info":
				log.Logger = log.Logger.Level(zerolog.InfoLevel)
			case "debug":
				log.Logger = log.Logger.Level(zerolog.DebugLevel)
			case "error":
				log.Logger = log.Logger.Level(zerolog.ErrorLevel)
			}

			app, err := core.NewApp(home)
			if err != nil {
				return err
			}

			// TODO: fix logging
			err = app.Start()
			for restartAttempt := 1; restartAttempt <= 3 && err != nil; restartAttempt++ {
				fmt.Println(err)
				fmt.Printf("Attempting restart again in %d seconds (attempt %d of %d)...\n", time.Second*5, restartAttempt, 3)
				time.Sleep(time.Second * 5)
				err = app.Start()
			}

			return err
		},
	}

	return cmd
}
