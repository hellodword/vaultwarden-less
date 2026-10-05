package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/hellodword/vaultwarden-less/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := cli.Execute(ctx, os.Args[1:], os.Stdout, os.Stderr)
	cli.WriteError(os.Stderr, err)
	stop()
	os.Exit(cli.ExitCode(err))
}
