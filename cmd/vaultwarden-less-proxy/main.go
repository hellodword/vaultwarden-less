package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/hellodword/vaultwarden-less/internal/buildinfo"
	proxyserver "github.com/hellodword/vaultwarden-less/internal/proxy"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(execute(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

func execute(ctx context.Context, arguments []string, stdout, stderr io.Writer) int {
	if len(arguments) == 1 && arguments[0] == "--version" {
		_, _ = fmt.Fprintf(stdout, "vaultwarden-less-proxy %s\n", buildinfo.Version)
		return 0
	}
	if len(arguments) == 1 && (arguments[0] == "--help" || arguments[0] == "-h") {
		writeHelp(stdout)
		return 0
	}
	if len(arguments) == 0 {
		_, _ = fmt.Fprintln(stderr, "a subcommand is required")
		return 2
	}
	switch arguments[0] {
	case "serve":
		if len(arguments) == 2 && (arguments[1] == "--help" || arguments[1] == "-h") {
			_, _ = fmt.Fprintln(stdout, "Usage: vaultwarden-less-proxy serve")
			return 0
		}
		if len(arguments) != 1 {
			_, _ = fmt.Fprintln(stderr, "serve does not accept arguments")
			return 2
		}
		return serve(ctx, stderr)
	case "healthcheck":
		if len(arguments) == 2 && (arguments[1] == "--help" || arguments[1] == "-h") {
			_, _ = fmt.Fprintln(stdout, "Usage: vaultwarden-less-proxy healthcheck [--url URL]")
			return 0
		}
		return healthcheck(ctx, arguments[1:], stderr)
	default:
		_, _ = fmt.Fprintf(stderr, "unknown command %q\n", arguments[0])
		return 2
	}
}

func writeHelp(writer io.Writer) {
	_, _ = fmt.Fprintln(writer, "Usage: vaultwarden-less-proxy <serve|healthcheck> [flags]")
	_, _ = fmt.Fprintln(writer, "       vaultwarden-less-proxy --version")
}

func serve(ctx context.Context, stderr io.Writer) int {
	logger := slog.New(slog.NewJSONHandler(stderr, nil))
	serveContext, stop := context.WithCancel(ctx)
	defer stop()
	events := proxyserver.NewEventSender(logger)
	var eventWait sync.WaitGroup
	events.Run(serveContext, &eventWait)
	server := proxyserver.NewServer(events, logger)
	serveError := make(chan error, 1)
	go func() { serveError <- server.ListenAndServe() }()
	logger.Info("proxy started", "listen_address", proxyserver.ListenAddress)
	result := 0
	select {
	case <-ctx.Done():
	case err := <-serveError:
		if err != nil {
			logger.Error("proxy server stopped unexpectedly", "error_code", "listen_failed")
			result = 1
		}
	}
	stop()
	shutdownContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), proxyserver.ShutdownTimeout)
	defer cancel()
	if err := server.Shutdown(shutdownContext); err != nil {
		logger.Error("proxy shutdown did not complete", "error_code", "shutdown_timeout")
		result = 1
	}
	eventWait.Wait()
	logger.Info("proxy stopped")
	return result
}

func healthcheck(ctx context.Context, arguments []string, stderr io.Writer) int {
	flags := flag.NewFlagSet("healthcheck", flag.ContinueOnError)
	flags.SetOutput(stderr)
	endpoint := flags.String("url", "http://127.0.0.1:8080"+proxyserver.HealthPath, "health URL")
	if err := flags.Parse(arguments); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if flags.NArg() != 0 {
		_, _ = fmt.Fprintln(stderr, "healthcheck does not accept positional arguments")
		return 2
	}
	client := &http.Client{
		Timeout: 2 * time.Second,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, *endpoint, nil)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "invalid health URL")
		return 2
	}
	response, err := client.Do(request)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "proxy healthcheck failed")
		return 1
	}
	defer func() { _ = response.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	if response.StatusCode != http.StatusNoContent {
		_, _ = fmt.Fprintf(stderr, "proxy health endpoint returned status %d\n", response.StatusCode)
		return 1
	}
	return 0
}
