package upgrade

import (
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

const (
	MaxStateBytes = 1 << 20

	PhasePrepared   = "prepared"
	PhaseFrozen     = "frozen"
	PhaseMigrated   = "migrated"
	PhaseVerified   = "verified"
	PhaseAccepted   = "accepted"
	PhaseRolledBack = "rolled_back"

	DecisionAccept   = "accept"
	DecisionRollback = "rollback"
)

const timestampLayout = "2006-01-02T15:04:05.000Z"

var (
	releasePattern = regexp.MustCompile(`^RELEASE\.[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}-[0-9]{2}-[0-9]{2}Z-g([0-9a-f]{12})$`)
	targetPattern  = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
)

type ErrorKind string

const (
	ErrorConflict           ErrorKind = "conflict"
	ErrorInvalidState       ErrorKind = "invalid_state"
	ErrorSourceInconsistent ErrorKind = "source_inconsistent"
	ErrorVerification       ErrorKind = "verification"
)

// StateError deliberately exposes only a stable category. Callers must not
// serialize the wrapped filesystem, database, or repository diagnostic.
type StateError struct {
	Kind  ErrorKind
	cause error
}

func (err *StateError) Error() string { return "upgrade state operation failed: " + string(err.Kind) }
func (err *StateError) Unwrap() error { return err.cause }

func stateError(kind ErrorKind, cause error) error {
	return &StateError{Kind: kind, cause: cause}
}

func IsKind(err error, kind ErrorKind) bool {
	var state *StateError
	return errors.As(err, &state) && state.Kind == kind
}

type TargetEvidence struct {
	Target     string `json:"target"`
	SnapshotID string `json:"snapshot_id"`
	ContentID  string `json:"content_id"`
	ArchiveID  string `json:"archive_id"`
}

type DatabaseContract struct {
	Migration         string
	SchemaFingerprint string
}

type Result struct {
	UpgradeID  string           `json:"upgrade_id"`
	Phase      string           `json:"phase"`
	FromCommit string           `json:"from_commit"`
	ToCommit   string           `json:"to_commit"`
	Targets    []TargetEvidence `json:"targets"`
}

type Prepare struct {
	UpgradeID   string `json:"upgrade_id"`
	Phase       string `json:"phase"`
	FromCommit  string `json:"from_commit"`
	FromRelease string `json:"from_release"`
	ToCommit    string `json:"to_commit"`
	ToRelease   string `json:"to_release"`
	PreparedAt  string `json:"prepared_at"`
}

type RollbackTicket struct {
	UpgradeID      string           `json:"upgrade_id"`
	Phase          string           `json:"phase"`
	FromCommit     string           `json:"from_commit"`
	FromRelease    string           `json:"from_release"`
	ToCommit       string           `json:"to_commit"`
	ToRelease      string           `json:"to_release"`
	ActivePath     string           `json:"active_path"`
	RollbackPath   string           `json:"rollback_path"`
	ActiveDevice   uint64           `json:"active_device"`
	RollbackDevice uint64           `json:"rollback_device"`
	RollbackTreeID string           `json:"rollback_tree_id"`
	FrozenAt       string           `json:"frozen_at"`
	Targets        []TargetEvidence `json:"targets"`
}

type Candidate struct {
	UpgradeID                 string           `json:"upgrade_id"`
	Phase                     string           `json:"phase"`
	FromCommit                string           `json:"from_commit"`
	ToCommit                  string           `json:"to_commit"`
	DatabaseMigration         string           `json:"database_migration"`
	DatabaseSchemaFingerprint string           `json:"database_schema_fingerprint"`
	UpdatedAt                 string           `json:"updated_at"`
	Targets                   []TargetEvidence `json:"targets"`
}

type Decision struct {
	UpgradeID           string `json:"upgrade_id"`
	Decision            string `json:"decision"`
	ActiveProjectCommit string `json:"active_project_commit"`
	ActiveRelease       string `json:"active_release"`
	DecidedAt           string `json:"decided_at"`
}

func (prepare Prepare) validate() error {
	if prepare.Phase != PhasePrepared ||
		!ValidCommit(prepare.FromCommit) || !ValidCommit(prepare.ToCommit) || prepare.FromCommit == prepare.ToCommit ||
		!ValidRelease(prepare.FromRelease, prepare.FromCommit) || !ValidRelease(prepare.ToRelease, prepare.ToCommit) ||
		prepare.UpgradeID != upgradeID(prepare.FromCommit, prepare.ToCommit) || !validTimestamp(prepare.PreparedAt) {
		return fmt.Errorf("prepare state is invalid")
	}
	return nil
}

func (ticket RollbackTicket) validate(prepare Prepare) error {
	if ticket.Phase != PhaseFrozen ||
		ticket.UpgradeID != prepare.UpgradeID || ticket.FromCommit != prepare.FromCommit ||
		ticket.FromRelease != prepare.FromRelease || ticket.ToCommit != prepare.ToCommit ||
		ticket.ToRelease != prepare.ToRelease || ticket.ActivePath != "data" ||
		ticket.RollbackPath != "state/upgrade/current/rollback-data" || ticket.ActiveDevice == 0 ||
		ticket.RollbackDevice == 0 || ticket.ActiveDevice != ticket.RollbackDevice ||
		!validDigest(ticket.RollbackTreeID) || !validTimestamp(ticket.FrozenAt) || len(ticket.Targets) == 0 {
		return fmt.Errorf("rollback ticket is invalid")
	}
	return validateEvidence(ticket.Targets, nil)
}

func (candidate Candidate) validate(prepare Prepare) error {
	if candidate.UpgradeID != prepare.UpgradeID ||
		candidate.FromCommit != prepare.FromCommit || candidate.ToCommit != prepare.ToCommit ||
		candidate.DatabaseMigration == "" || !validDigest(candidate.DatabaseSchemaFingerprint) ||
		!validTimestamp(candidate.UpdatedAt) || candidate.Targets == nil {
		return fmt.Errorf("candidate state is invalid")
	}
	switch candidate.Phase {
	case PhaseMigrated:
		if len(candidate.Targets) != 0 {
			return fmt.Errorf("migrated candidate is invalid")
		}
	case PhaseVerified:
		if len(candidate.Targets) == 0 {
			return fmt.Errorf("verified candidate is invalid")
		}
	default:
		return fmt.Errorf("candidate phase is invalid")
	}
	return validateEvidence(candidate.Targets, nil)
}

func (decision Decision) validate(prepare Prepare) error {
	if decision.UpgradeID != prepare.UpgradeID ||
		!validTimestamp(decision.DecidedAt) {
		return fmt.Errorf("upgrade decision is invalid")
	}
	wantCommit, wantRelease := prepare.ToCommit, prepare.ToRelease
	if decision.Decision == DecisionRollback {
		wantCommit, wantRelease = prepare.FromCommit, prepare.FromRelease
	} else if decision.Decision != DecisionAccept {
		return fmt.Errorf("upgrade decision is invalid")
	}
	if decision.ActiveProjectCommit != wantCommit || decision.ActiveRelease != wantRelease {
		return fmt.Errorf("upgrade decision identity is invalid")
	}
	return nil
}

func validateEvidence(evidence []TargetEvidence, names []string) error {
	if evidence == nil || names != nil && len(evidence) != len(names) {
		return fmt.Errorf("target evidence set is invalid")
	}
	seen := make(map[string]struct{}, len(evidence))
	for index, item := range evidence {
		if !targetPattern.MatchString(item.Target) || !validHex(item.SnapshotID, 64) ||
			!validDigest(item.ContentID) || !validDigest(item.ArchiveID) {
			return fmt.Errorf("target evidence is invalid")
		}
		if names != nil && item.Target != names[index] {
			return fmt.Errorf("target evidence order is invalid")
		}
		if _, duplicate := seen[item.Target]; duplicate {
			return fmt.Errorf("target evidence names are not unique")
		}
		seen[item.Target] = struct{}{}
	}
	return nil
}

func result(phase string, prepare Prepare, targets []TargetEvidence) Result {
	if targets == nil {
		targets = []TargetEvidence{}
	}
	return Result{
		UpgradeID:  prepare.UpgradeID,
		Phase:      phase,
		FromCommit: prepare.FromCommit,
		ToCommit:   prepare.ToCommit,
		Targets:    targets,
	}
}

func upgradeID(fromCommit, toCommit string) string { return fromCommit + ".." + toCommit }

func ValidCommit(value string) bool { return validHex(value, 40) }

func ValidRelease(value, commit string) bool {
	match := releasePattern.FindStringSubmatch(value)
	return ValidCommit(commit) && len(match) == 2 && match[1] == commit[:12]
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
