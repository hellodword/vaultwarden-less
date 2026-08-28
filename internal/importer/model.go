// Package importer owns the one-time external Vaultwarden data import.
package importer

import (
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/hellodword/vaultwarden-less/internal/deployment"
)

const (
	SchemaVersion = 1
	MaxStateBytes = 1 << 20

	ImportIDInitial = "initial"

	PhasePrepared  = "prepared"
	PhaseVerified  = "verified"
	PhaseAccepted  = "accepted"
	PhaseFinalized = "finalized"
	PhaseCancelled = "cancelled"

	DecisionAccept = "accept"

	sourceRelative    = "import/source"
	candidateRelative = "import/candidate"
	dataRelative      = "data"
)

const timestampLayout = "2006-01-02T15:04:05.000Z"

type ErrorKind string

const (
	ErrorConflict           ErrorKind = "conflict"
	ErrorInvalidState       ErrorKind = "invalid_state"
	ErrorSourceInconsistent ErrorKind = "source_inconsistent"
	ErrorVerification       ErrorKind = "verification"
)

// StateError exposes a stable category without leaking imported data, paths,
// database diagnostics, or state contents through the CLI boundary.
type StateError struct {
	Kind  ErrorKind
	cause error
}

func (err *StateError) Error() string { return "import state operation failed: " + string(err.Kind) }
func (err *StateError) Unwrap() error { return err.cause }

func stateError(kind ErrorKind, cause error) error {
	return &StateError{Kind: kind, cause: cause}
}

func IsKind(err error, kind ErrorKind) bool {
	var state *StateError
	return errors.As(err, &state) && state.Kind == kind
}

type Prepare struct {
	SchemaVersion    int    `json:"schema_version"`
	ImportID         string `json:"import_id"`
	Phase            string `json:"phase"`
	ProjectCommit    string `json:"project_commit"`
	Release          string `json:"release"`
	SourcePath       string `json:"source_path"`
	CandidatePath    string `json:"candidate_path"`
	DataPath         string `json:"data_path"`
	FilesystemDevice uint64 `json:"filesystem_device"`
	SourceTreeID     string `json:"source_tree_id"`
	CandidateTreeID  string `json:"candidate_tree_id"`
	SourceRSAKeyID   string `json:"source_rsa_key_id"`
	PreparedAt       string `json:"prepared_at"`
}

func (prepare Prepare) Identity() deployment.Identity {
	return deployment.Identity{ProjectCommit: prepare.ProjectCommit, Release: prepare.Release}
}

func (prepare Prepare) validate() error {
	if prepare.SchemaVersion != SchemaVersion || prepare.ImportID != ImportIDInitial || prepare.Phase != PhasePrepared ||
		prepare.SourcePath != sourceRelative || prepare.CandidatePath != candidateRelative || prepare.DataPath != dataRelative ||
		prepare.FilesystemDevice == 0 || !validDigest(prepare.SourceTreeID) || !validDigest(prepare.CandidateTreeID) ||
		!validDigest(prepare.SourceRSAKeyID) || !validTimestamp(prepare.PreparedAt) {
		return fmt.Errorf("import preparation is invalid")
	}
	if err := prepare.Identity().Validate(); err != nil {
		return fmt.Errorf("import preparation identity is invalid")
	}
	return nil
}

type Verification struct {
	SchemaVersion             int    `json:"schema_version"`
	ImportID                  string `json:"import_id"`
	Phase                     string `json:"phase"`
	ProjectCommit             string `json:"project_commit"`
	Release                   string `json:"release"`
	SourceTreeID              string `json:"source_tree_id"`
	CandidateTreeID           string `json:"candidate_tree_id"`
	DatabaseMigration         string `json:"database_migration"`
	DatabaseSchemaFingerprint string `json:"database_schema_fingerprint"`
	ContentID                 string `json:"content_id"`
	ArchiveID                 string `json:"archive_id"`
	ClientLoginConfirmed      bool   `json:"client_login_confirmed"`
	VerifiedAt                string `json:"verified_at"`
}

func (verification Verification) validate(prepare Prepare) error {
	if verification.SchemaVersion != SchemaVersion || verification.ImportID != ImportIDInitial ||
		verification.Phase != PhaseVerified || verification.ProjectCommit != prepare.ProjectCommit ||
		verification.Release != prepare.Release || verification.SourceTreeID != prepare.SourceTreeID ||
		!validDigest(verification.CandidateTreeID) || verification.DatabaseMigration == "" ||
		!validDigest(verification.DatabaseSchemaFingerprint) || !validDigest(verification.ContentID) ||
		!validDigest(verification.ArchiveID) || !verification.ClientLoginConfirmed ||
		!validTimestamp(verification.VerifiedAt) {
		return fmt.Errorf("import verification is invalid")
	}
	return nil
}

type Decision struct {
	SchemaVersion       int    `json:"schema_version"`
	ImportID            string `json:"import_id"`
	Decision            string `json:"decision"`
	ActiveProjectCommit string `json:"active_project_commit"`
	ActiveRelease       string `json:"active_release"`
	DecidedAt           string `json:"decided_at"`
}

func (decision Decision) Identity() deployment.Identity {
	return deployment.Identity{ProjectCommit: decision.ActiveProjectCommit, Release: decision.ActiveRelease}
}

func (decision Decision) validate(prepare Prepare) error {
	if decision.SchemaVersion != SchemaVersion || decision.ImportID != ImportIDInitial || decision.Decision != DecisionAccept ||
		decision.Identity() != prepare.Identity() || !validTimestamp(decision.DecidedAt) {
		return fmt.Errorf("import decision is invalid")
	}
	return nil
}

type CandidateEvidence struct {
	DatabaseMigration         string
	DatabaseSchemaFingerprint string
	ContentID                 string
	ArchiveID                 string
}

func (evidence CandidateEvidence) validate() error {
	if evidence.DatabaseMigration == "" || !validDigest(evidence.DatabaseSchemaFingerprint) ||
		!validDigest(evidence.ContentID) || !validDigest(evidence.ArchiveID) {
		return fmt.Errorf("candidate evidence is invalid")
	}
	return nil
}

type Result struct {
	SchemaVersion        int    `json:"schema_version"`
	ImportID             string `json:"import_id"`
	Phase                string `json:"phase"`
	ProjectCommit        string `json:"project_commit"`
	Release              string `json:"release"`
	SourceTreeID         string `json:"source_tree_id"`
	CandidateTreeID      string `json:"candidate_tree_id"`
	ClientLoginConfirmed bool   `json:"client_login_confirmed"`
}

func result(phase string, prepare Prepare, candidateTreeID string, confirmed bool) Result {
	return Result{
		SchemaVersion: SchemaVersion,
		ImportID:      ImportIDInitial,
		Phase:         phase,
		ProjectCommit: prepare.ProjectCommit,
		Release:       prepare.Release,
		SourceTreeID:  prepare.SourceTreeID,
		CandidateTreeID: func() string {
			if candidateTreeID != "" {
				return candidateTreeID
			}
			return prepare.CandidateTreeID
		}(),
		ClientLoginConfirmed: confirmed,
	}
}

func validDigest(value string) bool {
	return strings.HasPrefix(value, "sha256:") && validHex(strings.TrimPrefix(value, "sha256:"), 64)
}

func validHex(value string, size int) bool {
	if len(value) != size || value != strings.ToLower(value) {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func validTimestamp(value string) bool {
	parsed, err := time.Parse(timestampLayout, value)
	return err == nil && parsed.UTC().Format(timestampLayout) == value
}

func formatTimestamp(value time.Time) string { return value.UTC().Format(timestampLayout) }
