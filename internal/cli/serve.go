package cli

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/hellodword/vaultwarden-less/internal/capture"
	"github.com/hellodword/vaultwarden-less/internal/config"
	"github.com/hellodword/vaultwarden-less/internal/notify"
	"github.com/hellodword/vaultwarden-less/internal/restic"
	"github.com/hellodword/vaultwarden-less/internal/state"
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
	store := state.Store{Directory: config.StateDir}
	lock, err := store.Lock()
	if err != nil {
		return &commandError{message: "Worker state unavailable or another instance is running"}
	}
	defer lock.Close()
	work := filepath.Join(config.CacheDir, "work")
	if err := os.RemoveAll(work); err != nil {
		return &commandError{message: "Work cleanup failed"}
	}
	if err := os.MkdirAll(work, 0o700); err != nil {
		return &commandError{message: "Work directory unavailable"}
	}
	notifications := notify.NewManager(cfg.Notifications.Channels)
	if err := notifications.Restore(store); err != nil {
		return &commandError{message: "Notification state unavailable"}
	}
	repo := restic.NewRunner("restic", config.CacheDir)
	backupWorker := worker.New(cfg, capture.Builder{DataDir: config.DataDir, CacheDir: work}, repo, notifications, logger)
	if err := backupWorker.Restore(store); err != nil {
		return &commandError{message: "Worker state unavailable"}
	}
	server := worker.NewHTTPServer(backupWorker)
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var background sync.WaitGroup
	failed := make(chan error, 2)
	background.Go(func() {
		if err := notifications.Run(runCtx); err != nil {
			failed <- err
		}
	})
	background.Go(func() {
		if err := backupWorker.Run(runCtx); err != nil {
			failed <- err
		}
	})
	done := make(chan error, 1)
	go func() { done <- server.ListenAndServe() }()
	logger.Info("backup process started")
	var result error
	select {
	case <-ctx.Done():
	case <-failed:
		result = &commandError{message: "Worker persistent state could not be saved"}
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
