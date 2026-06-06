package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"go.f110.dev/go-archive-cacheprog/internal/cacheprog"
)

const (
	archiveEnvVar     = "GO_ARCHIVE_CACHE_FILE"
	compressionEnvVar = "GO_ARCHIVE_CACHE_COMPRESSION"
)

func NewRootCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "go-archive-cacheprog",
		Short: "GOCACHEPROG implementation backed by a single zip archive file",
		Long: fmt.Sprintf("go-archive-cacheprog implements the GOCACHEPROG protocol.\n"+
			"The archive file path is taken from %s.\n"+
			"The compression for new entries is taken from %s (deflate, zstd, store; default deflate).",
			archiveEnvVar, compressionEnvVar),
		SilenceUsage:  true,
		SilenceErrors: true,
		Args:          cobra.NoArgs,
		RunE: func(c *cobra.Command, args []string) error {
			archivePath := os.Getenv(archiveEnvVar)
			if archivePath == "" {
				return fmt.Errorf("%s is required", archiveEnvVar)
			}
			compression, err := cacheprog.ParseCompression(os.Getenv(compressionEnvVar))
			if err != nil {
				return fmt.Errorf("%s: %w", compressionEnvVar, err)
			}
			return cacheprog.Run(c.Context(), archivePath, compression, os.Stdin, os.Stdout, os.Stderr)
		},
	}
}
