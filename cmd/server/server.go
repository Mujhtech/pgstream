package server

import (
	"github.com/mujhtech/pgstream/config"
	"github.com/rs/zerolog/log"
	"github.com/spf13/cobra"
)

func RegisterServerCommand() *cobra.Command {

	var (
		configFile string
		logLevel   string
	)

	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Start pgstream server",
		Long:  ``,
		Run: func(cmd *cobra.Command, args []string) {

			err := startServer(configFile, logLevel)

			if err != nil {
				log.Err(err).Msg("failed to start server")
			}

		},
	}

	cmd.Flags().StringVar(&configFile, "config", config.DefaultConfigFilePath, "configuration file")
	cmd.Flags().StringVar(&logLevel, "log-level", "info", "log level")

	return cmd

}

func startServer(configFile string, logLevel string) error {
	return nil
}
