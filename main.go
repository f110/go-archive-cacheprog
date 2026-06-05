package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"go.f110.dev/go-archive-cacheprog/cmd"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := cmd.NewRootCommand().ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
