package config

import "time"

const (
	MaxConfigBytes   = 1 << 20
	MaxPasswordBytes = 64 << 10

	DefaultBackupConfigPath = "/config/backup.yaml"
	RoleCredentialsPath     = "/run/credentials/role.yaml"
	ResticPasswordRoot      = "/run/credentials/restic"

	ListenAddress     = "0.0.0.0:9090"
	WorkerURL         = "http://backup:9090"
	DataDir           = "/data"
	StagingDir        = "/staging"
	RecoveryPointPath = "/staging/point"
	CacheDir          = "/cache"
	RestoreRoot       = "/restore"
	ResticBinary      = "/bin/restic"

	QuietWindow           = 30 * time.Second
	MaximumQuietWindow    = 2 * time.Minute
	MinimumCheckInterval  = 5 * time.Minute
	FallbackCheckInterval = 12 * time.Hour
	ConsistencyRetry      = 30 * time.Second
	FailureRetry          = 5 * time.Minute
	DegradedAfter         = 30 * time.Minute
	EventRetry            = 30 * time.Second
	TargetTimeout         = 2 * time.Hour
	MaintenanceTimeout    = 24 * time.Hour
	NotifierTimeout       = 10 * time.Second
	ShutdownTimeout       = 2 * time.Minute
	EventMaxBytes         = 1024
)
