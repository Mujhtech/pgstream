package server

import (
	"errors"
	"fmt"
	"os"

	"github.com/joho/godotenv"
	"github.com/mujhtech/pgstream/config"
	"github.com/mujhtech/pgstream/internal/encrypt"
	"github.com/mujhtech/pgstream/internal/server"
	"github.com/mujhtech/pgstream/internal/storage"
	"github.com/spf13/cobra"
)

const defaultListenAddr = "127.0.0.1:8080"

func RegisterServerCommand() *cobra.Command {

	var (
		configFile string
		addr       string
	)

	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Start the pgstream server and web UI",
		Long: `Starts an HTTP server that exposes the migration engine as a JSON API
with live progress streaming and serves the bundled web UI.

The server binds to localhost by default because migration requests contain
database credentials. Put a reverse proxy with authentication in front of it
before exposing it beyond your machine.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := godotenv.Load(configFile); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("load %s: %w", configFile, err)
			}
			cfg, err := config.LoadConfig()
			if err != nil {
				return fmt.Errorf("load configuration: %w", err)
			}
			sessionCipher, err := encrypt.NewAesGcm(cfg.EncryptionKey)
			if err != nil {
				return fmt.Errorf("configure session credential encryption from ENCRYPTION_KEY: %w", err)
			}
			metadataStorage, err := storage.InitStorage(cmd.Context(), cfg.Database)
			if err != nil {
				return fmt.Errorf("initialize session storage: %w", err)
			}
			defer metadataStorage.Close()

			if !cmd.Flags().Changed("addr") && cfg.Server.Port != 0 {
				addr = fmt.Sprintf("127.0.0.1:%d", cfg.Server.Port)
			}

			apiServer := server.New(cmd.Context(), cfg, metadataStorage, sessionCipher)
			return apiServer.ListenAndServe(cmd.Context(), addr)
		},
	}

	cmd.Flags().StringVar(&configFile, "config", config.DefaultConfigFilePath, "configuration file")
	cmd.Flags().StringVar(&addr, "addr", defaultListenAddr, "listen address")

	return cmd

}
