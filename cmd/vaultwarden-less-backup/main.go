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
	command := cli.New(os.Stdout, os.Stderr)
	command.SetContext(ctx)
	err := cli.Execute(command, os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
	stop()
	os.Exit(cli.ExitCode(err))
}
