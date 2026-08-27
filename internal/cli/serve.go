package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"

	"github.com/hellodword/vaultwarden-less/internal/config"
	"github.com/hellodword/vaultwarden-less/internal/notify"
	"github.com/hellodword/vaultwarden-less/internal/snapshot"
	"github.com/hellodword/vaultwarden-less/internal/worker"
	"github.com/spf13/cobra"
)

func serveCommand(configPath *string, environment commandEnvironment) *cobra.Command {
	return &cobra.Command{
		Use:   "serve",
		Short: "Run the in-memory scheduler, backup worker, and internal HTTP API",
		Args:  noArguments,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := environment.backup(*configPath)
			if err != nil {
				return commandFailure(codeInvalidConfig, "The backup configuration is invalid", err)
			}
			notifiers, err := environment.notifiersFor(cfg)
			if err != nil {
				return commandFailure(codeInvalidCredentials, "Notifier credentials are invalid", err)
			}
			logger := environment.logger
			if logger == nil {
				logger = slog.New(slog.NewJSONHandler(cmd.ErrOrStderr(), nil))
			}
			notifications, err := notify.NewManager(notifiers, logger)
			if err != nil {
				return commandFailure(codeInvalidCredentials, "Notifier credentials are invalid", err)
			}
			repository, err := environment.repositoryFor(cfg)
			if err != nil {
				return commandFailure(codeInvalidCredentials, "Backup role credentials are invalid", err)
			}
			if err := repository.VerifyVersion(cmd.Context()); err != nil {
				return commandFailure(codeRepositoryFailure, "Restic version verification failed", err)
			}

			for _, target := range cfg.Targets {
				for _, warning := range target.Warnings() {
					logger.Warn("configuration warning", "target", target.Name, "warning", warning)
				}
			}
			ctx, cancel := context.WithCancel(cmd.Context())
			defer cancel()
			builder := snapshot.Builder{DataDir: environment.dataDir, StagingDir: environment.stagingDir}
			backupWorker := worker.New(cfg, builder.Build, repository.Ensure, notifications.Enqueue, logger)
			server := worker.NewHTTPServer(backupWorker)
			serverError := make(chan error, 1)
			workerDone := make(chan struct{})
			var background sync.WaitGroup
			background.Go(func() { notifications.Run(ctx) })
			background.Go(func() {
				defer close(workerDone)
				backupWorker.Run(ctx)
			})
			go func() { serverError <- server.ListenAndServe() }()
			logger.Info("backup worker started", "listen_address", server.Addr)

			var result error
			select {
			case <-cmd.Context().Done():
			case err := <-serverError:
				if err != nil && !errors.Is(err, http.ErrServerClosed) {
					result = fmt.Errorf("internal HTTP server failed")
				}
			case <-workerDone:
				if cmd.Context().Err() == nil {
					result = fmt.Errorf("backup scheduler stopped unexpectedly")
				}
			}

			cancel()
			shutdownContext, shutdownCancel := context.WithTimeout(context.WithoutCancel(cmd.Context()), config.ShutdownTimeout)
			defer shutdownCancel()
			if err := server.Shutdown(shutdownContext); err != nil && result == nil {
				result = fmt.Errorf("internal HTTP server shutdown failed")
			}
			select {
			case <-workerDone:
			case <-shutdownContext.Done():
				if result == nil {
					result = fmt.Errorf("backup scheduler shutdown timed out")
				}
			}
			background.Wait()
			logger.Info("backup worker stopped")
			if result != nil {
				return commandFailure(codeInternal, "The backup worker stopped unexpectedly", result)
			}
			return nil
		},
	}
}
