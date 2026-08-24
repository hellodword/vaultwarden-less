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

	proxyserver "github.com/hellodword/vaultwarden-less/internal/proxy"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(healthcheck(os.Args[2:]))
	}
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	flags := flag.NewFlagSet("vaultwarden-less-proxy", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "unexpected positional argument")
		return 2
	}
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	events := proxyserver.NewEventSender(log)
	var eventWG sync.WaitGroup
	events.Run(ctx, &eventWG)
	server := proxyserver.NewServer(events, log)
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.ListenAndServe() }()
	log.Info("proxy started", "listen_address", proxyserver.ListenAddress)

	select {
	case <-ctx.Done():
	case err := <-serveErr:
		if err != nil {
			log.Error("proxy server stopped unexpectedly", "error_code", "listen_failed")
			return 1
		}
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), proxyserver.ShutdownTimeout)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Error("proxy shutdown did not complete", "error_code", "shutdown_timeout")
		return 1
	}
	stop()
	eventWG.Wait()
	log.Info("proxy stopped")
	return 0
}

func healthcheck(args []string) int {
	flags := flag.NewFlagSet("healthcheck", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	url := flags.String("url", "http://127.0.0.1:8080"+proxyserver.HealthPath, "health URL")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return 2
	}
	client := &http.Client{
		Timeout:       2 * time.Second,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
	}
	req, err := http.NewRequest(http.MethodGet, *url, nil)
	if err != nil {
		return 2
	}
	resp, err := client.Do(req)
	if err != nil {
		return 1
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusNoContent {
		return 1
	}
	return 0
}
