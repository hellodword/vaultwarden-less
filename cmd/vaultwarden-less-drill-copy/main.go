package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/hellodword/vaultwarden-less/internal/buildinfo"
	"github.com/hellodword/vaultwarden-less/internal/drillcopy"
)

func main() {
	if len(os.Args) != 1 {
		_, _ = fmt.Fprintln(os.Stderr, "vaultwarden-less-drill-copy accepts no arguments")
		os.Exit(2)
	}
	if !buildinfo.Ready() {
		_, _ = fmt.Fprintln(os.Stderr, "vaultwarden-less-drill-copy requires a clean release image")
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := drillcopy.Run(ctx, drillcopy.SourceRoot, drillcopy.DestinationRoot); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "vaultwarden-less-drill-copy failed")
		os.Exit(1)
	}
}
