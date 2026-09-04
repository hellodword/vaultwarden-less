package restic

import (
	"context"
	"log/slog"

	"github.com/hellodword/vaultwarden-less/internal/config"
)

const (
	maxJSONFrame     = 8 << 20
	maxCommandOutput = 64 << 20
	maxJSONFrames    = 100_000
	maxStderrOutput  = 64 << 10

	SnapshotHost          = "vaultwarden-less"
	DefaultSnapshotsLimit = 100
	MaxSnapshotsLimit     = 1000
)

type OperationError struct {
	Code     string
	ExitCode int
}

func (e *OperationError) Error() string { return "restic operation failed: " + e.Code }

type Snapshot struct {
	ID            string `json:"id"`
	Time          string `json:"time"`
	ProjectCommit string `json:"project_commit"`
	ContentID     string `json:"content_id"`
	ArchiveID     string `json:"archive_id"`
}

type SyncResult struct {
	SnapshotID string
	ContentID  string
	ArchiveID  string
	Created    bool
}

type PruneGroup struct {
	Host              string   `json:"host"`
	Paths             []string `json:"paths"`
	KeepSnapshotIDs   []string `json:"keep_snapshot_ids"`
	RemoveSnapshotIDs []string `json:"remove_snapshot_ids"`
}

type PruneResult struct {
	Applied bool         `json:"applied"`
	Groups  []PruneGroup `json:"groups"`
}

type executeFunc func(context.Context, string, []string, []string, string) ([]byte, error)

type Runner struct {
	binary       string
	cacheDir     string
	passwordRoot string
	credentials  *config.RoleCredentials
	logger       *slog.Logger

	execute      executeFunc
	recoveryPath string
}

func NewRunner(
	binary, cacheDir, passwordRoot, recoveryPath string,
	credentials *config.RoleCredentials,
	logger *slog.Logger,
) Runner {
	return Runner{
		binary: binary, cacheDir: cacheDir, passwordRoot: passwordRoot,
		credentials: credentials, logger: logger, recoveryPath: recoveryPath,
	}
}
