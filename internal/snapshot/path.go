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

// Vaultwarden currently generates lowercase hexadecimal attachment and Send
// file IDs, but its durable model treats both as opaque safe path components.
// Preserve older valid local data without accepting separators, Unicode, or a
// component too large for the supported local-filesystem contract.
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
	if strings.HasPrefix(path, "data/attachments/") || strings.HasPrefix(path, "data/sends/") || strings.HasPrefix(path, "data/templates/") || path == "data/config.json" {
		return true
	}
	return strings.HasPrefix(path, "data/rsa_key") && !strings.Contains(strings.TrimPrefix(path, "data/"), "/")
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
