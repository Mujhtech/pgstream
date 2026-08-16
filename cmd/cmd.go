package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/mujhtech/pgstream/cmd/cli"
	"github.com/mujhtech/pgstream/cmd/server"
	"github.com/mujhtech/pgstream/cmd/version"
	"github.com/rs/zerolog"
	zLog "github.com/rs/zerolog/log"
	"github.com/spf13/cobra"
)

func main() {
	err := os.Setenv("TZ", "")
	if err != nil {
		log.Fatal(err)
	}

	cmd := &cobra.Command{
		Use:          "pgstream",
		Version:      version.Version,
		Short:        "Migrate MySQL databases to PostgreSQL",
		SilenceUsage: true,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// First Ctrl+C cancels the context for a graceful stop (checkpointed
	// progress, recorded events); restoring default signal handling right
	// after lets a second Ctrl+C terminate immediately.
	go func() {
		<-ctx.Done()
		stop()
	}()

	switch os.Getenv("LOG_LEVEL") {
	case "debug":
		zerolog.SetGlobalLevel(zerolog.DebugLevel)
	case "trace":
		zerolog.SetGlobalLevel(zerolog.TraceLevel)
	default:
		zerolog.SetGlobalLevel(zerolog.InfoLevel)
	}

	zerolog.TimeFieldFormat = time.RFC3339Nano

	// attach logger to context
	logger := zLog.Logger.With().Logger()
	ctx = logger.WithContext(ctx)

	cmd.AddCommand(server.RegisterServerCommand())
	cmd.AddCommand(version.RegisterVersionCommand())
	cmd.AddCommand(cli.RegisterCliCommand())
	cmd.AddCommand(cli.RegisterStatusCommand())

	// Cobra already printed the error; exiting non-zero without re-logging
	// avoids the duplicated final line.
	if err := cmd.ExecuteContext(ctx); err != nil {
		os.Exit(1)
	}
}
