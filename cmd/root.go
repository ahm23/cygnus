package cmd

import (
	"fmt"
	"os"

	"github.com/rs/zerolog/log"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"cygnus/cmd/types"
	"cygnus/config"
)

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

	r.PersistentFlags().String(types.FlagHome, types.DefaultHome, "sets the home directory for cygnus")
	r.PersistentFlags().String(types.FlagLogLevel, types.DefaultLogLevel, "log level. info|error|debug")

	err := viper.BindPFlags(r.PersistentFlags())
	if err != nil {
		panic(err)
	}

	r.AddCommand(InitCmd(), VersionCmd(), StartCmd(), StressTestCmd())

	return r
}

func Execute(rootCmd *cobra.Command) {
	if err := rootCmd.Execute(); err != nil {

		log.Error().Err(err)
		os.Exit(1)
	}
}
