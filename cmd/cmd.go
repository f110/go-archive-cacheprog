package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"go.f110.dev/go-archive-cacheprog/internal/cacheprog"
)

const archiveEnvVar = "GO_ARCHIVE_CACHE_FILE"

func NewRootCommand() *cobra.Command {
	return &cobra.Command{
		Use:           "go-archive-cacheprog",
		Short:         "GOCACHEPROG implementation backed by a single zip archive file",
		Long:          fmt.Sprintf("go-archive-cacheprog implements the GOCACHEPROG protocol.\nThe archive file path is taken from the %s environment variable.", archiveEnvVar),
		SilenceUsage:  true,
		SilenceErrors: true,
		Args:          cobra.NoArgs,
		RunE: func(c *cobra.Command, args []string) error {
			archivePath := os.Getenv(archiveEnvVar)
			if archivePath == "" {
				return fmt.Errorf("%s is required", archiveEnvVar)
			}
			return cacheprog.Run(c.Context(), archivePath, os.Stdin, os.Stdout, os.Stderr)
		},
	}
}
