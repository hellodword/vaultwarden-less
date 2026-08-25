package snapshot

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/hellodword/vaultwarden-less/internal/safefs"
)

var (
	uuidPattern     = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	opaqueIDPattern = regexp.MustCompile(`^[A-Za-z0-9-]+$`)
)

func validUUID(value string) bool { return uuidPattern.MatchString(value) }

// Attachment IDs are opaque path-safe components in the v1 path contract.
func validOpaqueID(value string) bool {
	return len(value) >= 1 && len(value) <= 255 && opaqueIDPattern.MatchString(value)
}

func validManifestPath(path string) bool {
	if path == "" || filepath.IsAbs(path) || filepath.Clean(path) != path || filepath.ToSlash(path) != path {
		return false
	}
	if path == "manifest.json" || path == "data/db.sqlite3" {
		return true
	}
	parts := strings.Split(path, "/")
	if len(parts) == 4 && parts[0] == "data" && parts[1] == "attachments" && validUUID(parts[2]) && validOpaqueID(parts[3]) {
		return true
	}
	return path == "data/rsa_key.pem" || path == "data/rsa_key.pub.pem"
}

func secureJoin(base, relative string) (string, error) {
	if !validManifestPath(relative) {
		return "", fmt.Errorf("invalid snapshot path")
	}
	joined := filepath.Join(base, filepath.FromSlash(relative))
	rel, err := filepath.Rel(base, joined)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("snapshot path escapes base")
	}
	return joined, nil
}

func mkdirAllSecure(path string) error {
	return safefs.EnsurePrivateDirectory(path)
}
