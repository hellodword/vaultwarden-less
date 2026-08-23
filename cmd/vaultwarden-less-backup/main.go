package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/hellodword/vaultwarden-less/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	command := cli.New(os.Stdout, os.Stderr)
	command.SetContext(ctx)
	err := command.Execute()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
	os.Exit(cli.ExitCode(err))
}
