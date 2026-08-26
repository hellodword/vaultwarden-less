package snapshot

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/hellodword/vaultwarden-less/internal/safefs"
)

var (
	uuidPattern         = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	attachmentIDPattern = regexp.MustCompile(`^[0-9a-f]{20}$`)
	sendFileIDPattern   = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

func validUUID(value string) bool         { return uuidPattern.MatchString(value) }
func validAttachmentID(value string) bool { return attachmentIDPattern.MatchString(value) }
func validSendFileID(value string) bool   { return sendFileIDPattern.MatchString(value) }

func validManifestPath(path string) bool {
	if path == "" || filepath.IsAbs(path) || filepath.Clean(path) != path || filepath.ToSlash(path) != path {
		return false
	}
	if path == "data/db.sqlite3" {
		return true
	}
	parts := strings.Split(path, "/")
	if len(parts) != 4 || parts[0] != "data" {
		return false
	}
	switch parts[1] {
	case "attachments":
		return validUUID(parts[2]) && validAttachmentID(parts[3])
	case "sends":
		return validUUID(parts[2]) && validSendFileID(parts[3])
	default:
		return false
	}
}

func secureJoin(base, relative string) (string, error) {
	if !validManifestPath(relative) {
		return "", fmt.Errorf("invalid recovery path")
	}
	joined := filepath.Join(base, filepath.FromSlash(relative))
	relativeToBase, err := filepath.Rel(base, joined)
	if err != nil || relativeToBase == ".." || strings.HasPrefix(relativeToBase, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("recovery path escapes root")
	}
	return joined, nil
}

func mkdirAllSecure(path string) error {
	return safefs.EnsurePrivateDirectory(path)
}
