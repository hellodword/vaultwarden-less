package snapshot

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/hellodword/vaultwarden-less/internal/version"
)

type Status string

const (
	Complete   Status = "complete"
	Incomplete Status = "incomplete"
)

type File struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

type Missing struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

type Manifest struct {
	SchemaVersion      int       `json:"schema_version"`
	VaultwardenVersion string    `json:"vaultwarden_version"`
	DatabaseMigration  string    `json:"database_migration"`
	Status             Status    `json:"status"`
	Files              []File    `json:"files"`
	Missing            []Missing `json:"missing"`
	ContentID          string    `json:"content_id"`
}

type contentPayload struct {
	SchemaVersion      int       `json:"schema_version"`
	VaultwardenVersion string    `json:"vaultwarden_version"`
	DatabaseMigration  string    `json:"database_migration"`
	Status             Status    `json:"status"`
	Files              []File    `json:"files"`
	Missing            []Missing `json:"missing"`
}

func NewManifest(status Status, migration string, files []File, missing []Missing) (Manifest, error) {
	if status != Complete && status != Incomplete {
		return Manifest{}, fmt.Errorf("invalid manifest status %q", status)
	}
	if status == Complete && len(missing) != 0 {
		return Manifest{}, fmt.Errorf("complete manifest cannot contain missing files")
	}
	files = slices.Clone(files)
	missing = slices.Clone(missing)
	if files == nil {
		files = []File{}
	}
	if missing == nil {
		missing = []Missing{}
	}
	slices.SortFunc(files, func(a, b File) int { return strings.Compare(a.Path, b.Path) })
	slices.SortFunc(missing, func(a, b Missing) int {
		if cmp := strings.Compare(a.Path, b.Path); cmp != 0 {
			return cmp
		}
		return strings.Compare(a.Reason, b.Reason)
	})
	m := Manifest{
		SchemaVersion:      version.ManifestVersion,
		VaultwardenVersion: version.Vaultwarden,
		DatabaseMigration:  migration,
		Status:             status,
		Files:              files,
		Missing:            missing,
	}
	id, err := m.CalculateContentID()
	if err != nil {
		return Manifest{}, err
	}
	m.ContentID = id
	if err := m.Validate(); err != nil {
		return Manifest{}, err
	}
	return m, nil
}

func (m Manifest) CalculateContentID() (string, error) {
	payload := contentPayload{
		SchemaVersion:      m.SchemaVersion,
		VaultwardenVersion: m.VaultwardenVersion,
		DatabaseMigration:  m.DatabaseMigration,
		Status:             m.Status,
		Files:              m.Files,
		Missing:            m.Missing,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("encode manifest content: %w", err)
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func (m Manifest) Validate() error {
	if m.SchemaVersion != version.ManifestVersion || m.VaultwardenVersion != version.Vaultwarden {
		return fmt.Errorf("unsupported manifest or Vaultwarden version")
	}
	if m.DatabaseMigration != version.VaultwardenSchema {
		return fmt.Errorf("unsupported Vaultwarden database migration")
	}
	if m.Status != Complete && m.Status != Incomplete {
		return fmt.Errorf("invalid manifest status")
	}
	if m.Files == nil || m.Missing == nil {
		return fmt.Errorf("manifest file and missing lists must be arrays")
	}
	if m.Status == Complete && len(m.Missing) != 0 || m.Status == Incomplete && len(m.Missing) == 0 {
		return fmt.Errorf("manifest status and missing entries disagree")
	}
	previous := ""
	paths := make(map[string]struct{}, len(m.Files))
	databasePresent := false
	for _, file := range m.Files {
		if !validManifestPath(file.Path) || file.Path == "manifest.json" || file.Path <= previous || file.Size < 0 || !validHash(file.SHA256) {
			return fmt.Errorf("invalid or unsorted manifest file entry")
		}
		paths[file.Path] = struct{}{}
		databasePresent = databasePresent || file.Path == "data/db.sqlite3"
		previous = file.Path
	}
	if !databasePresent {
		return fmt.Errorf("manifest does not contain the snapshot database")
	}
	previous = ""
	for _, missing := range m.Missing {
		_, alsoPresent := paths[missing.Path]
		if !validManifestPath(missing.Path) || missing.Path == "manifest.json" || missing.Path == "data/db.sqlite3" ||
			missing.Path <= previous || !validMissingReason(missing.Reason) || alsoPresent {
			return fmt.Errorf("invalid or unsorted missing entry")
		}
		previous = missing.Path
	}
	want, err := m.CalculateContentID()
	if err != nil {
		return err
	}
	if m.ContentID != want {
		return fmt.Errorf("manifest content_id mismatch")
	}
	return nil
}

func validHash(value string) bool {
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil && strings.ToLower(value) == value
}

func validMissingReason(value string) bool {
	return value == "not_found" || value == "size_mismatch" || value == "changed_during_copy"
}
