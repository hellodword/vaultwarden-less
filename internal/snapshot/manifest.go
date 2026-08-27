package snapshot

import (
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
)

const ManifestVersion = 1

type File struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

type Manifest struct {
	SchemaVersion int    `json:"schema_version"`
	ProjectCommit string `json:"project_commit"`
	ContentID     string `json:"content_id"`
	ArchiveID     string `json:"archive_id"`
	Files         []File `json:"files"`
}

func NewManifest(projectCommit, contentID, archiveID string, files []File) (Manifest, error) {
	files = slices.Clone(files)
	if files == nil {
		files = []File{}
	}
	slices.SortFunc(files, func(left, right File) int {
		return strings.Compare(left.Path, right.Path)
	})
	manifest := Manifest{
		SchemaVersion: ManifestVersion,
		ProjectCommit: projectCommit,
		ContentID:     contentID,
		ArchiveID:     archiveID,
		Files:         files,
	}
	if err := manifest.Validate(); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

func (m Manifest) Validate() error {
	if m.SchemaVersion != ManifestVersion {
		return fmt.Errorf("unsupported manifest schema")
	}
	if !validCommit(m.ProjectCommit) {
		return fmt.Errorf("invalid project commit")
	}
	if !IsContentID(m.ContentID) || !IsContentID(m.ArchiveID) {
		return fmt.Errorf("invalid recovery identity")
	}
	if len(m.Files) == 0 || len(m.Files) > 1_000_000 {
		return fmt.Errorf("manifest files must be a nonempty bounded array")
	}
	previous := ""
	databasePresent := false
	for _, file := range m.Files {
		if !validManifestPath(file.Path) || file.Path <= previous || file.Size < 0 || !IsContentID(file.SHA256) {
			return fmt.Errorf("invalid, duplicate, or unsorted manifest file entry")
		}
		if file.Path == "db.sqlite3" {
			databasePresent = true
		}
		previous = file.Path
	}
	if !databasePresent {
		return fmt.Errorf("manifest does not contain db.sqlite3")
	}
	return nil
}

func validHash(value string) bool {
	if len(value) != 64 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func validCommit(value string) bool {
	if len(value) != 40 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func ContentIDHex(contentID string) (string, error) {
	if !IsContentID(contentID) {
		return "", fmt.Errorf("invalid content ID")
	}
	return strings.TrimPrefix(contentID, "sha256:"), nil
}
