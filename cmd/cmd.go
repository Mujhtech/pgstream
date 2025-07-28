package main

import (
	"log"
	"os"

	"github.com/mujhtech/pgstream/cmd/server"
	"github.com/mujhtech/pgstream/cmd/version"
	"github.com/spf13/cobra"
)

func main() {
	err := os.Setenv("TZ", "")
	if err != nil {
		log.Fatal(err)
	}

	cmd := &cobra.Command{
		Use:     "pgstream",
		Version: version.Version,
		Short:   "Tools for stream data to Postgres from MySQL",
	}

	cmd.AddCommand(server.RegisterServerCommand())
	cmd.AddCommand(version.RegisterVersionCommand())

	err = cmd.Execute()
	if err != nil {
		log.Fatal(err)
	}
}
