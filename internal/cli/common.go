package cli

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/hellodword/vaultwarden-less/internal/config"
	"github.com/hellodword/vaultwarden-less/internal/notify"
	"github.com/hellodword/vaultwarden-less/internal/restic"
	restoretool "github.com/hellodword/vaultwarden-less/internal/restore"
	"github.com/hellodword/vaultwarden-less/internal/securefile"
	"github.com/hellodword/vaultwarden-less/internal/snapshot"
)

type repository interface {
	VerifyVersion(context.Context) error
	Ensure(context.Context, config.Target, snapshot.Manifest) (restic.SyncResult, error)
	Snapshots(context.Context, config.Target) ([]restic.Snapshot, error)
	Init(context.Context, config.Target) error
	Unlock(context.Context, config.Target) error
	Check(context.Context, config.Target, string) error
	Prune(context.Context, config.Target, bool) (restic.PruneResult, error)
	ResolveSnapshot(context.Context, config.Target, string) (restic.Snapshot, error)
	Restore(context.Context, config.Target, string, string) error
}

type restoreFunc func(context.Context, restoretool.Repository, restoretool.Request) (restoretool.Result, error)

type commandEnvironment struct {
	loadBackup      func(string) (*config.Backup, error)
	loadCredentials func(string, *config.Backup) (*config.RoleCredentials, error)
	readSecret      func(string) (string, error)
	validateNotify  func([]config.Notifier) error
	repository      func(*config.Backup) (repository, error)
	restore         restoreFunc
	httpClient      *http.Client
	workerURL       string
	credentialsPath string
	passwordPath    string
	resticBinary    string
	dataDir         string
	stagingDir      string
	cacheDir        string
	restoreRoot     string
}

func defaultEnvironment() commandEnvironment {
	environment := commandEnvironment{
		loadBackup:      config.LoadBackup,
		loadCredentials: config.LoadRoleCredentials,
		readSecret:      securefile.Read,
		validateNotify:  notify.ValidateConfigs,
		restore:         restoretool.Run,
		httpClient: &http.Client{
			Timeout: 5 * time.Second,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		workerURL:       config.WorkerURL,
		credentialsPath: config.RoleCredentialsPath,
		passwordPath:    config.ResticPasswordPath,
		resticBinary:    config.ResticBinary,
		dataDir:         config.DataDir,
		stagingDir:      config.StagingDir,
		cacheDir:        config.CacheDir,
		restoreRoot:     config.RestoreRoot,
	}
	environment.repository = func(cfg *config.Backup) (repository, error) {
		credentials, err := environment.loadCredentials(environment.credentialsPath, cfg)
		if err != nil {
			return nil, err
		}
		return restic.Runner{
			Binary:       environment.resticBinary,
			CacheDir:     environment.cacheDir,
			PasswordFile: environment.passwordPath,
			Credentials:  credentials,
		}, nil
	}
	return environment
}

func (environment commandEnvironment) backup(path string) (*config.Backup, error) {
	if environment.loadBackup == nil {
		return nil, fmt.Errorf("configuration loader is unavailable")
	}
	return environment.loadBackup(path)
}

func (environment commandEnvironment) repositoryFor(cfg *config.Backup) (repository, error) {
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
