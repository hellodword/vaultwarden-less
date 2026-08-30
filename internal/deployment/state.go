// Package deployment owns the durable active release identity.
package deployment

import (
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/hellodword/vaultwarden-less/internal/privatefs"
	"github.com/hellodword/vaultwarden-less/internal/statefile"
)

const (
	MaxStateBytes = 1 << 20

	StatusInitialized        = "initialized"
	StatusAlreadyInitialized = "already_initialized"
)

const timestampLayout = "2006-01-02T15:04:05.000Z"

var releasePattern = regexp.MustCompile(`^RELEASE\.[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}-[0-9]{2}-[0-9]{2}Z-g([0-9a-f]{12})$`)

type ErrorKind string

const (
	ErrorConflict       ErrorKind = "conflict"
	ErrorInvalidState   ErrorKind = "invalid_state"
	ErrorNotInitialized ErrorKind = "not_initialized"
)

// StateError exposes a stable category without leaking a filesystem path or
// state document into a CLI error.
type StateError struct {
	Kind  ErrorKind
	cause error
}

func (err *StateError) Error() string {
	return "deployment state operation failed: " + string(err.Kind)
}
func (err *StateError) Unwrap() error { return err.cause }

func stateError(kind ErrorKind, cause error) error {
	return &StateError{Kind: kind, cause: cause}
}

func IsKind(err error, kind ErrorKind) bool {
	var state *StateError
	return errors.As(err, &state) && state.Kind == kind
}

type Identity struct {
	ProjectCommit string
	Release       string
}

func (identity Identity) Validate() error {
	if !validHex(identity.ProjectCommit, 40) {
		return fmt.Errorf("project commit is invalid")
	}
	match := releasePattern.FindStringSubmatch(identity.Release)
	if len(match) != 2 || match[1] != identity.ProjectCommit[:12] {
		return fmt.Errorf("release does not match project commit")
	}
	return nil
}

type Active struct {
	ProjectCommit string `json:"project_commit"`
	Release       string `json:"release"`
	ActivatedAt   string `json:"activated_at"`
}

func (active Active) Identity() Identity {
	return Identity{ProjectCommit: active.ProjectCommit, Release: active.Release}
}

func (active Active) validate() error {
	if err := active.Identity().Validate(); err != nil {
		return err
	}
	parsed, err := time.Parse(timestampLayout, active.ActivatedAt)
	if err != nil || parsed.UTC().Format(timestampLayout) != active.ActivatedAt {
		return fmt.Errorf("activation timestamp is invalid")
	}
	return nil
}

type InitResult struct {
	Status        string `json:"status"`
	ProjectCommit string `json:"project_commit"`
	Release       string `json:"release"`
}

type Manager struct {
	StateDir      string
	DataDir       string
	ProjectCommit string
	Release       string
	Now           func() time.Time
}

func (manager Manager) Init() (InitResult, error) {
	identity := Identity{ProjectCommit: manager.ProjectCommit, Release: manager.Release}
	if err := manager.validate(identity); err != nil {
		return InitResult{}, stateError(ErrorInvalidState, err)
	}
	if err := privatefs.ValidatePrivateDirectory(manager.StateDir); err != nil {
		return InitResult{}, stateError(ErrorInvalidState, fmt.Errorf("validate state root: %w", err))
	}
	if err := privatefs.ValidatePrivateDirectory(manager.DataDir); err != nil {
		return InitResult{}, stateError(ErrorInvalidState, fmt.Errorf("validate data root: %w", err))
	}

	deploymentDir, activePath := activePaths(manager.StateDir)
	deploymentExists, err := pathExists(deploymentDir)
	if err != nil {
		return InitResult{}, stateError(ErrorInvalidState, err)
	}
	if deploymentExists {
		if err := privatefs.ValidatePrivateDirectory(deploymentDir); err != nil {
			return InitResult{}, stateError(ErrorInvalidState, fmt.Errorf("validate deployment state directory: %w", err))
		}
	}
	activeExists, err := pathExists(activePath)
	if err != nil {
		return InitResult{}, stateError(ErrorInvalidState, err)
	}
	if activeExists {
		active, readErr := ReadActive(manager.StateDir)
		if readErr != nil {
			return InitResult{}, readErr
		}
		if active.Identity() != identity {
			return InitResult{}, stateError(ErrorConflict, fmt.Errorf("another release is already active"))
		}
		return initResult(StatusAlreadyInitialized, identity), nil
	}

	entries, err := os.ReadDir(manager.DataDir)
	if err != nil {
		return InitResult{}, stateError(ErrorInvalidState, fmt.Errorf("read data root: %w", err))
	}
	if len(entries) != 0 {
		return InitResult{}, stateError(ErrorConflict, fmt.Errorf("data root is not empty"))
	}
	for _, current := range []string{
		filepath.Join(manager.StateDir, "upgrade", "current"),
		filepath.Join(manager.StateDir, "import", "current"),
	} {
		exists, existsErr := pathExists(current)
		if existsErr != nil {
			return InitResult{}, stateError(ErrorInvalidState, existsErr)
		}
		if exists {
			return InitResult{}, stateError(ErrorConflict, fmt.Errorf("a workflow is already current"))
		}
	}

	if !deploymentExists {
		if err := privatefs.EnsurePrivateDirectory(deploymentDir); err != nil {
			return InitResult{}, stateError(ErrorInvalidState, fmt.Errorf("create deployment state directory: %w", err))
		}
		if err := privatefs.SyncDirectory(manager.StateDir); err != nil {
			return InitResult{}, stateError(ErrorInvalidState, fmt.Errorf("sync deployment state directory: %w", err))
		}
	}
	active := Active{
		ProjectCommit: identity.ProjectCommit,
		Release:       identity.Release,
		ActivatedAt:   manager.now().Format(timestampLayout),
	}
	if err := writeNewActive(activePath, active); err != nil {
		// A concurrent initializer may have published the exact same identity.
		published, readErr := ReadActive(manager.StateDir)
		if readErr == nil {
			if published.Identity() == identity {
				return initResult(StatusAlreadyInitialized, identity), nil
			}
			return InitResult{}, stateError(ErrorConflict, fmt.Errorf("another release is already active"))
		}
		return InitResult{}, stateError(ErrorInvalidState, fmt.Errorf("publish active identity: %w", err))
	}
	return initResult(StatusInitialized, identity), nil
}

// PublishInitialAtOrAfter publishes the first active identity after a caller
// has atomically promoted verified initial data. Unlike Init, the data root is
// expected to be non-empty; the owning workflow must prove that promotion
// before calling this internal boundary.
func (manager Manager) PublishInitialAtOrAfter(notBefore time.Time) (Active, error) {
	identity := Identity{ProjectCommit: manager.ProjectCommit, Release: manager.Release}
	if err := manager.validate(identity); err != nil || notBefore.IsZero() {
		if err == nil {
			err = fmt.Errorf("minimum activation time is required")
		}
		return Active{}, stateError(ErrorInvalidState, err)
	}
	if err := privatefs.ValidatePrivateDirectory(manager.StateDir); err != nil {
		return Active{}, stateError(ErrorInvalidState, fmt.Errorf("validate state root: %w", err))
	}
	if err := privatefs.ValidatePrivateDirectory(manager.DataDir); err != nil {
		return Active{}, stateError(ErrorInvalidState, fmt.Errorf("validate data root: %w", err))
	}
	deploymentDir, activePath := activePaths(manager.StateDir)
	deploymentExists, err := pathExists(deploymentDir)
	if err != nil {
		return Active{}, stateError(ErrorInvalidState, err)
	}
	if deploymentExists {
		if err := privatefs.ValidatePrivateDirectory(deploymentDir); err != nil {
			return Active{}, stateError(ErrorInvalidState, fmt.Errorf("validate deployment state directory: %w", err))
		}
	}
	activeExists, err := pathExists(activePath)
	if err != nil {
		return Active{}, stateError(ErrorInvalidState, err)
	}
	if activeExists {
		active, readErr := ReadActive(manager.StateDir)
		if readErr != nil {
			return Active{}, readErr
		}
		activatedAt, parseErr := time.Parse(timestampLayout, active.ActivatedAt)
		if active.Identity() != identity || parseErr != nil || activatedAt.Before(notBefore) {
			return Active{}, stateError(ErrorConflict, fmt.Errorf("another or stale release is already active"))
		}
		return active, nil
	}
	if !deploymentExists {
		if err := privatefs.EnsurePrivateDirectory(deploymentDir); err != nil {
			return Active{}, stateError(ErrorInvalidState, fmt.Errorf("create deployment state directory: %w", err))
		}
		if err := privatefs.SyncDirectory(manager.StateDir); err != nil {
			return Active{}, stateError(ErrorInvalidState, fmt.Errorf("sync deployment state directory: %w", err))
		}
	}
	activatedAt := manager.now()
	if activatedAt.Before(notBefore) {
		activatedAt = notBefore.UTC()
	}
	active := Active{
		ProjectCommit: identity.ProjectCommit,
		Release:       identity.Release,
		ActivatedAt:   activatedAt.Format(timestampLayout),
	}
	if err := writeNewActive(activePath, active); err != nil {
		published, readErr := ReadActive(manager.StateDir)
		if readErr == nil {
			publishedAt, parseErr := time.Parse(timestampLayout, published.ActivatedAt)
			if published.Identity() == identity && parseErr == nil && !publishedAt.Before(notBefore) {
				return published, nil
			}
			return Active{}, stateError(ErrorConflict, fmt.Errorf("another release is already active"))
		}
		return Active{}, stateError(ErrorInvalidState, fmt.Errorf("publish active identity: %w", err))
	}
	return active, nil
}

// Activate moves an existing active identity to the manager's exact identity.
// The caller supplies the only identity from which the transition is allowed;
// repeating a completed transition preserves the original activation time.
func (manager Manager) Activate(expected Identity) (Active, error) {
	return manager.activateAtOrAfter(expected, time.Time{})
}

// ActivateAtOrAfter also refreshes an unchanged identity when its activation
// predates notBefore. Terminal workflows use this to publish completion only
// after their data transition while preserving idempotency on every rerun.
func (manager Manager) ActivateAtOrAfter(expected Identity, notBefore time.Time) (Active, error) {
	if notBefore.IsZero() {
		return Active{}, stateError(ErrorInvalidState, fmt.Errorf("minimum activation time is required"))
	}
	return manager.activateAtOrAfter(expected, notBefore.UTC())
}

func (manager Manager) activateAtOrAfter(expected Identity, notBefore time.Time) (Active, error) {
	target := Identity{ProjectCommit: manager.ProjectCommit, Release: manager.Release}
	if err := manager.validate(target); err != nil {
		return Active{}, stateError(ErrorInvalidState, err)
	}
	if err := expected.Validate(); err != nil {
		return Active{}, stateError(ErrorInvalidState, err)
	}
	active, err := ReadActive(manager.StateDir)
	if err != nil {
		return Active{}, err
	}
	if active.Identity() == target {
		activatedAt, parseErr := time.Parse(timestampLayout, active.ActivatedAt)
		if parseErr != nil {
			return Active{}, stateError(ErrorInvalidState, fmt.Errorf("activation timestamp is invalid"))
		}
		if notBefore.IsZero() || !activatedAt.Before(notBefore) {
			return active, nil
		}
	}
	if active.Identity() != expected {
		return Active{}, stateError(ErrorConflict, fmt.Errorf("active identity is not the expected transition source"))
	}
	activatedAt := manager.now()
	if activatedAt.Before(notBefore) {
		activatedAt = notBefore
	}

	next := Active{
		ProjectCommit: target.ProjectCommit,
		Release:       target.Release,
		ActivatedAt:   activatedAt.Format(timestampLayout),
	}
	if err := replaceActive(filepath.Join(manager.StateDir, "deployment", "active.json"), next); err != nil {
		published, readErr := ReadActive(manager.StateDir)
		if readErr == nil && published.Identity() == target {
			return published, nil
		}
		return Active{}, stateError(ErrorInvalidState, fmt.Errorf("replace active identity: %w", err))
	}
	return next, nil
}

func ReadActive(stateDir string) (Active, error) {
	deploymentDir, activePath := activePaths(stateDir)
	if err := privatefs.ValidatePrivateDirectory(stateDir); err != nil {
		return Active{}, stateError(ErrorInvalidState, fmt.Errorf("validate state root: %w", err))
	}
	if err := privatefs.ValidatePrivateDirectory(deploymentDir); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Active{}, stateError(ErrorNotInitialized, err)
		}
		return Active{}, stateError(ErrorInvalidState, fmt.Errorf("validate deployment state directory: %w", err))
	}
	var active Active
	if err := statefile.ReadJSON(activePath, MaxStateBytes, &active); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Active{}, stateError(ErrorNotInitialized, err)
		}
		return Active{}, stateError(ErrorInvalidState, fmt.Errorf("read active identity: %w", err))
	}
	if err := active.validate(); err != nil {
		return Active{}, stateError(ErrorInvalidState, err)
	}
	return active, nil
}

func RequireActive(stateDir string, identity Identity) (Active, error) {
	if err := identity.Validate(); err != nil {
		return Active{}, stateError(ErrorInvalidState, err)
	}
	active, err := ReadActive(stateDir)
	if err != nil {
		return Active{}, err
	}
	if active.Identity() != identity {
		return Active{}, stateError(ErrorConflict, fmt.Errorf("active identity does not match this release"))
	}
	return active, nil
}

func (manager Manager) validate(identity Identity) error {
	if manager.StateDir == "" || manager.DataDir == "" || !filepath.IsAbs(manager.StateDir) || !filepath.IsAbs(manager.DataDir) ||
		filepath.Clean(manager.StateDir) != manager.StateDir || filepath.Clean(manager.DataDir) != manager.DataDir || manager.StateDir == manager.DataDir {
		return fmt.Errorf("deployment paths are invalid")
	}
	return identity.Validate()
}

func (manager Manager) now() time.Time {
	if manager.Now != nil {
		return manager.Now().UTC()
	}
	return time.Now().UTC()
}

func initResult(status string, identity Identity) InitResult {
	return InitResult{
		Status:        status,
		ProjectCommit: identity.ProjectCommit,
		Release:       identity.Release,
	}
}

func activePaths(stateDir string) (string, string) {
	deploymentDir := filepath.Join(stateDir, "deployment")
	return deploymentDir, filepath.Join(deploymentDir, "active.json")
}

func writeNewActive(path string, active Active) error {
	if err := active.validate(); err != nil {
		return err
	}
	return statefile.WriteNewJSON(path, MaxStateBytes, active)
}

func replaceActive(path string, active Active) error {
	if err := active.validate(); err != nil {
		return err
	}
	return statefile.ReplaceJSON(path, MaxStateBytes, active)
}

func pathExists(path string) (bool, error) {
	_, err := os.Lstat(path)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, err
}

func validHex(value string, size int) bool {
	if len(value) != size || value != strings.ToLower(value) {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
