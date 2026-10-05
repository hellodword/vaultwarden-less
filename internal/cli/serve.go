package cli

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/hellodword/vaultwarden-less/internal/capture"
	"github.com/hellodword/vaultwarden-less/internal/config"
	"github.com/hellodword/vaultwarden-less/internal/notify"
	"github.com/hellodword/vaultwarden-less/internal/restic"
	"github.com/hellodword/vaultwarden-less/internal/worker"
)

func serve(ctx context.Context, path string, stderr io.Writer) error {
	cfg, err := config.LoadBackup(path)
	if err != nil {
		return inputError("Invalid local configuration")
	}
	logger := slog.New(slog.NewJSONHandler(stderr, nil))
	if len(cfg.Targets) == 0 {
		logger.Warn("No backup targets configured; data is not being backed up")
	}
	notifications := notify.NewManager(cfg.Notifiers)
	repo := restic.NewRunner("restic", config.CacheDir)
	backupWorker := worker.New(cfg, capture.Builder{DataDir: config.DataDir, CacheDir: config.CacheDir}, repo, notifications, logger)
	server := worker.NewHTTPServer(backupWorker)
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var background sync.WaitGroup
	background.Go(func() { notifications.Run(runCtx) })
	background.Go(func() { backupWorker.Run(runCtx) })
	done := make(chan error, 1)
	go func() { done <- server.ListenAndServe() }()
	logger.Info("backup process started")
	var result error
	select {
	case <-ctx.Done():
	case err := <-done:
		if !errors.Is(err, http.ErrServerClosed) {
			result = &commandError{message: "Backup HTTP service stopped"}
		}
	}
	cancel()
	shutdownCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer stop()
	if err := server.Shutdown(shutdownCtx); err != nil {
		result = &commandError{message: "Backup shutdown failed"}
	}
	background.Wait()
	return result
}
