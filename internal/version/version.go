package version

var Product = "dev"

const (
	SchemaVersion     = 1
	ManifestVersion   = 1
	Vaultwarden       = "1.37.2"
	VaultwardenSchema = "20260505120000"
	VaultwardenImage  = "vaultwarden/server:1.37.2@sha256:5d326778c22f063d093d6b0c9c766a28249561632266776f2c93132ab0ad3a80"
	Restic            = "0.19.1"
	SnapshotHost      = "vaultwarden-less"
	SnapshotPath      = "/staging/point"
	SnapshotTag       = "vaultwarden-less-recovery-v1"
)
