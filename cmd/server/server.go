package server

import (
	"fmt"

	"github.com/mujhtech/pgstream/config"
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
		RunE: func(cmd *cobra.Command, args []string) error {
			return startServer(configFile, logLevel)
		},
	}

	cmd.Flags().StringVar(&configFile, "config", config.DefaultConfigFilePath, "configuration file")
	cmd.Flags().StringVar(&logLevel, "log-level", "info", "log level")

	return cmd

}

func startServer(configFile string, logLevel string) error {
	return fmt.Errorf("server mode is not implemented yet (config %q, log level %q)", configFile, logLevel)
}
