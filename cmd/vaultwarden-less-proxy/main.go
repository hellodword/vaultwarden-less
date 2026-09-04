package main

import (
	"context"
	"encoding/json"
	"errors"
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

type proxyEnvironment struct {
	ready     func() bool
	healthURL string
	serve     func(context.Context, io.Writer) error
}

type proxyCommandError struct {
	code    string
	message string
	usage   bool
}

func (err *proxyCommandError) Error() string { return err.message }

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := execute(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

func execute(ctx context.Context, arguments []string, stdout, stderr io.Writer) int {
	environment := proxyEnvironment{
		ready:     buildinfo.Ready,
		healthURL: "http://127.0.0.1:8080" + proxyserver.HealthPath,
		serve:     serve,
	}
	return executeWithEnvironment(ctx, arguments, stdout, stderr, environment)
}

func executeWithEnvironment(ctx context.Context, arguments []string, stdout, stderr io.Writer, environment proxyEnvironment) int {
	if len(arguments) == 1 && (arguments[0] == "--help" || arguments[0] == "-h") {
		writeHelp(stdout)
		return 0
	}
	if len(arguments) == 0 {
		return writeProxyError(stderr, &proxyCommandError{code: "usage_error", message: "A subcommand is required", usage: true})
	}
	command := arguments[0]
	if len(arguments) == 2 && (arguments[1] == "--help" || arguments[1] == "-h") {
		switch command {
		case "serve", "healthcheck", "version":
			_, _ = fmt.Fprintf(stdout, "Usage: vaultwarden-less-proxy %s\n", command)
			return 0
		}
	}
	if len(arguments) != 1 {
		return writeProxyError(stderr, &proxyCommandError{code: "usage_error", message: "Invalid command arguments", usage: true})
	}
	switch command {
	case "version":
		result := struct {
			ProjectCommit *string `json:"project_commit"`
			Release       string  `json:"release"`
			Clean         bool    `json:"clean"`
		}{Release: "development", Clean: false}
		if buildinfo.Ready() {
			commit := buildinfo.ProjectCommit
			result.ProjectCommit = &commit
			result.Release = buildinfo.Release
			result.Clean = true
		}
		_ = json.NewEncoder(stdout).Encode(result)
		return 0
	case "healthcheck":
		if err := healthcheck(ctx, environment.healthURL); err != nil {
			return writeProxyError(stderr, &proxyCommandError{code: "not_ready", message: "The proxy is not healthy"})
		}
		_ = json.NewEncoder(stdout).Encode(struct {
			Healthy bool `json:"healthy"`
		}{true})
		return 0
	case "serve":
		if environment.ready == nil || !environment.ready() {
			return writeProxyError(stderr, &proxyCommandError{code: "not_ready", message: "A clean release identity is required for this command"})
		}
		if environment.serve == nil {
			return writeProxyError(stderr, &proxyCommandError{code: "internal_error", message: "Proxy serving is unavailable"})
		}
		if err := environment.serve(ctx, stderr); err != nil {
			return writeProxyError(stderr, &proxyCommandError{code: "internal_error", message: "The proxy stopped unexpectedly"})
		}
		return 0
	default:
		return writeProxyError(stderr, &proxyCommandError{code: "usage_error", message: "Unknown command", usage: true})
	}
}

func writeProxyError(writer io.Writer, err *proxyCommandError) int {
	document := struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}{}
	document.Error.Code = err.code
	document.Error.Message = err.message
	_ = json.NewEncoder(writer).Encode(document)
	if err.usage {
		return 2
	}
	return 1
}

func writeHelp(writer io.Writer) {
	_, _ = fmt.Fprintln(writer, "Usage: vaultwarden-less-proxy <serve|healthcheck|version>")
}

func serve(ctx context.Context, stderr io.Writer) error {
	logger := slog.New(slog.NewJSONHandler(stderr, nil))
	serveContext, stop := context.WithCancel(ctx)
	defer stop()
	events := proxyserver.NewEventSender(logger)
	var eventWait sync.WaitGroup
	eventWait.Go(func() { events.Run(serveContext) })
	server := proxyserver.NewServer(events.MarkDirty, logger)
	serveError := make(chan error, 1)
	go func() { serveError <- server.ListenAndServe() }()
	logger.Info("proxy started", "listen_address", proxyserver.ListenAddress)
	var result error
	select {
	case <-ctx.Done():
	case err := <-serveError:
		if err != nil {
			logger.Error("proxy server stopped unexpectedly", "error_code", "listen_failed")
			result = errors.New("listen failed")
		}
	}
	stop()
	shutdownContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), proxyserver.ShutdownTimeout)
	defer cancel()
	if err := server.Shutdown(shutdownContext); err != nil {
		logger.Error("proxy shutdown did not complete", "error_code", "shutdown_timeout")
		result = errors.New("shutdown failed")
	}
	eventWait.Wait()
	logger.Info("proxy stopped")
	return result
}

func healthcheck(ctx context.Context, endpoint string) error {
	client := &http.Client{
		Timeout: 2 * time.Second,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	if response.StatusCode != http.StatusNoContent {
		return fmt.Errorf("unexpected status")
	}
	return nil
}
