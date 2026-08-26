//go:build systemtest

package restic

import "github.com/hellodword/vaultwarden-less/internal/config"

// NewSystemTestRunner preserves every production command and identity field
// while allowing a test-owned source path in place of the container's fixed
// /staging/point bind mount.
func NewSystemTestRunner(binary, cacheDir, passwordFile, recoveryPath string, credentials *config.RoleCredentials) Runner {
	return Runner{
		Binary: binary, CacheDir: cacheDir, PasswordFile: passwordFile,
		Credentials: credentials, recoveryPath: recoveryPath,
	}
}
