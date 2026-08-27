package restic

import (
	"log/slog"

	"github.com/hellodword/vaultwarden-less/internal/config"
)

func NewRunner(binary, cacheDir, passwordRoot, recoveryPath string, credentials *config.RoleCredentials, logger *slog.Logger) Runner {
	return Runner{
		Binary: binary, CacheDir: cacheDir, PasswordRoot: passwordRoot,
		Credentials: credentials, Logger: logger, recoveryPath: recoveryPath,
	}
}
