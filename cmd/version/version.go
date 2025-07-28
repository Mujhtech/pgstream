package version

import (
	"fmt"

	"github.com/spf13/cobra"
)

// Version holds the current version - injected during build time
var Version = "dev"

func RegisterVersionCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "version",
		Short: "Print the version and exit",
		Long:  ``,
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Printf("Version: %s\n", Version)
		},
	}

	return cmd
}
