package config

const (
	MaxConfigBytes   = 1 << 20
	MaxPasswordBytes = 64 << 10

	DefaultBackupConfigPath = "/config/backup.yaml"
	RoleCredentialsPath     = "/run/credentials/role.yaml"
	ResticPasswordRoot      = "/run/credentials/restic"

	WorkerURL         = "http://backup:9090"
	DataDir           = "/data"
	StagingDir        = "/staging"
	RecoveryPointPath = "/staging/point"
	CacheDir          = "/cache"
	RestoreRoot       = "/restore"
	ResticBinary      = "/bin/restic"
)
