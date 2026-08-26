package snapshot

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/hellodword/vaultwarden-less/internal/version"
)

type File struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

type VaultwardenTuple struct {
	Version           string `json:"version"`
	DatabaseMigration string `json:"database_migration"`
	Image             string `json:"image"`
}

type Manifest struct {
	SchemaVersion  int              `json:"schema_version"`
	CreatorVersion string           `json:"creator_version"`
	Vaultwarden    VaultwardenTuple `json:"vaultwarden"`
	Files          []File           `json:"files"`
	ContentID      string           `json:"content_id"`
}

type contentPayload struct {
	SchemaVersion int              `json:"schema_version"`
	Vaultwarden   VaultwardenTuple `json:"vaultwarden"`
	Files         []File           `json:"files"`
}

var supportedVaultwardenTuples = []VaultwardenTuple{{
	Version:           version.Vaultwarden,
	DatabaseMigration: version.VaultwardenSchema,
	Image:             version.VaultwardenImage,
}}

func NewManifest(files []File) (Manifest, error) {
	files = slices.Clone(files)
	if files == nil {
		files = []File{}
	}
	slices.SortFunc(files, func(left, right File) int {
		return strings.Compare(left.Path, right.Path)
	})
	manifest := Manifest{
		SchemaVersion:  version.ManifestVersion,
		CreatorVersion: version.Product,
		Vaultwarden:    supportedVaultwardenTuples[0],
		Files:          files,
	}
	contentID, err := manifest.CalculateContentID()
	if err != nil {
		return Manifest{}, err
	}
	manifest.ContentID = contentID
	if err := manifest.Validate(); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

func (m Manifest) CalculateContentID() (string, error) {
	payload := contentPayload{
		SchemaVersion: m.SchemaVersion,
		Vaultwarden:   m.Vaultwarden,
		Files:         m.Files,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("encode manifest content: %w", err)
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func (m Manifest) Validate() error {
	if m.SchemaVersion != version.ManifestVersion {
		return fmt.Errorf("unsupported manifest schema")
	}
	if !validCreatorVersion(m.CreatorVersion) {
		return fmt.Errorf("invalid creator version")
	}
	if !supportedVaultwardenTuple(m.Vaultwarden) {
		return fmt.Errorf("unsupported Vaultwarden tuple")
	}
	if m.Files == nil {
		return fmt.Errorf("manifest files must be an array")
	}
	previous := ""
	databasePresent := false
	for _, file := range m.Files {
		if !validManifestPath(file.Path) || file.Path <= previous || file.Size < 0 || !validHash(file.SHA256) {
			return fmt.Errorf("invalid, duplicate, or unsorted manifest file entry")
		}
		if file.Path == "data/db.sqlite3" {
			databasePresent = true
		}
		previous = file.Path
	}
	if !databasePresent {
		return fmt.Errorf("manifest does not contain data/db.sqlite3")
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

func supportedVaultwardenTuple(tuple VaultwardenTuple) bool {
	return slices.Contains(supportedVaultwardenTuples, tuple)
}

func validCreatorVersion(value string) bool {
	if value == "" || len(value) > 128 || !utf8.ValidString(value) {
		return false
	}
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return false
		}
	}
	return true
}

func validHash(value string) bool {
	if len(value) != 64 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func ContentIDHex(contentID string) (string, error) {
	if !strings.HasPrefix(contentID, "sha256:") || !validHash(strings.TrimPrefix(contentID, "sha256:")) {
		return "", fmt.Errorf("invalid content ID")
	}
	return strings.TrimPrefix(contentID, "sha256:"), nil
}
