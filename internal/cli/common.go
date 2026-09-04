package cli

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/hellodword/vaultwarden-less/internal/buildinfo"
	"github.com/hellodword/vaultwarden-less/internal/config"
	"github.com/hellodword/vaultwarden-less/internal/privatefs"
	"github.com/hellodword/vaultwarden-less/internal/restic"
	restoretool "github.com/hellodword/vaultwarden-less/internal/restore"
	"github.com/hellodword/vaultwarden-less/internal/snapshot"
)

type repository interface {
	VerifyVersion(context.Context) error
	Ensure(context.Context, config.Target, snapshot.Manifest) (restic.SyncResult, error)
	Snapshots(context.Context, config.Target, int) ([]restic.Snapshot, bool, error)
	Init(context.Context, config.Target) error
	Unlock(context.Context, config.Target) error
	Check(context.Context, config.Target) error
	Prune(context.Context, config.Target, bool) (restic.PruneResult, error)
	ResolveSnapshot(context.Context, config.Target, string) (restic.Snapshot, error)
	Restore(context.Context, config.Target, string, string) error
}

type restoreFunc func(context.Context, restoretool.Repository, restoretool.Request) (restoretool.Result, error)

type commandEnvironment struct {
	loadBackup        func(string) (*config.Backup, error)
	loadCredentials   func(string, *config.Backup) (*config.RoleCredentials, error)
	validatePasswords func(string, *config.Backup) error
	validateDirectory func(string) error
	repository        func(*config.Backup) (repository, error)
	restore           restoreFunc
	httpClient        *http.Client
	workerURL         string
	credentialsPath   string
	passwordRoot      string
	resticBinary      string
	dataDir           string
	stagingDir        string
	cacheDir          string
	restoreRoot       string
	now               func() time.Time
	ready             func() bool
	logger            *slog.Logger
}

func defaultEnvironment(stderr io.Writer) commandEnvironment {
	logger := slog.New(slog.NewJSONHandler(stderr, nil))
	environment := commandEnvironment{
		loadBackup:        config.LoadBackup,
		loadCredentials:   config.LoadRoleCredentials,
		validatePasswords: config.ValidatePasswordDirectory,
		validateDirectory: privatefs.ValidatePrivateDirectory,
		restore:           restoretool.Run,
		httpClient: &http.Client{
			Timeout: 5 * time.Second,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		workerURL:       config.WorkerURL,
		credentialsPath: config.RoleCredentialsPath,
		passwordRoot:    config.ResticPasswordRoot,
		resticBinary:    config.ResticBinary,
		dataDir:         config.DataDir,
		stagingDir:      config.StagingDir,
		cacheDir:        config.CacheDir,
		restoreRoot:     config.RestoreRoot,
		now:             time.Now,
		ready:           buildinfo.Ready,
		logger:          logger,
	}
	environment.repository = func(cfg *config.Backup) (repository, error) {
		credentials, err := environment.loadCredentials(environment.credentialsPath, cfg)
		if err != nil {
			return nil, err
		}
		return restic.NewRunner(
			environment.resticBinary,
			environment.cacheDir,
			environment.passwordRoot,
			config.RecoveryPointPath,
			credentials,
			environment.logger,
		), nil
	}
	return environment
}

func (environment commandEnvironment) isReady() bool {
	return environment.ready != nil && environment.ready()
}

func (environment commandEnvironment) backup(path string) (*config.Backup, error) {
	if environment.loadBackup == nil {
		return nil, fmt.Errorf("configuration loader is unavailable")
	}
	return environment.loadBackup(path)
}

func (environment commandEnvironment) repositoryFor(cfg *config.Backup) (repository, error) {
	if cfg == nil || len(cfg.Targets) == 0 {
		return nil, fmt.Errorf("no backup targets are configured")
	}
	if environment.repository == nil {
		return nil, fmt.Errorf("repository client is unavailable")
	}
	return environment.repository(cfg)
}

func findTarget(cfg *config.Backup, name string) (config.Target, error) {
	if name == "" {
		return config.Target{}, usageError("--target is required")
	}
	if target, ok := cfg.Target(name); ok {
		return target, nil
	}
	return config.Target{}, usageError("unknown target %q", name)
}

func writeJSON(writer io.Writer, value any) error {
	encoder := json.NewEncoder(writer)
	encoder.SetEscapeHTML(true)
	return encoder.Encode(value)
}

func validSnapshotID(value string) bool {
	if len(value) != 64 || value != strings.ToLower(value) {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
