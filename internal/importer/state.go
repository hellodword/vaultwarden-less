package importer

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/hellodword/vaultwarden-less/internal/privatefs"
	"github.com/hellodword/vaultwarden-less/internal/statefile"
)

const (
	prepareName      = "prepare.json"
	verificationName = "verification.json"
	decisionName     = "decision.json"
	currentTransient = ".current-initial"
	verifyTransient  = ".verification-initial"
)

type statePaths struct {
	root         string
	importRoot   string
	current      string
	history      string
	historyEntry string
	transient    string
	prepare      string
	verification string
	decision     string
}

func paths(stateDir string) statePaths {
	importRoot := filepath.Join(stateDir, "import")
	current := filepath.Join(importRoot, "current")
	history := filepath.Join(importRoot, "history")
	return statePaths{
		root:         stateDir,
		importRoot:   importRoot,
		current:      current,
		history:      history,
		historyEntry: filepath.Join(history, ImportIDInitial),
		transient:    filepath.Join(importRoot, currentTransient),
		prepare:      filepath.Join(current, prepareName),
		verification: filepath.Join(current, verificationName),
		decision:     filepath.Join(current, decisionName),
	}
}

func ensureStateLayout(stateDir string) (statePaths, error) {
	state := paths(stateDir)
	if err := privatefs.ValidatePrivateDirectory(state.root); err != nil {
		return state, stateError(ErrorInvalidState, fmt.Errorf("validate state root: %w", err))
	}
	for _, directory := range []string{state.importRoot, state.history} {
		if err := privatefs.EnsurePrivateDirectory(directory); err != nil {
			return state, stateError(ErrorInvalidState, fmt.Errorf("create import state layout: %w", err))
		}
	}
	return state, nil
}

func validateStateLayout(stateDir string) (statePaths, error) {
	state := paths(stateDir)
	for _, directory := range []string{state.root, state.importRoot, state.history, state.current} {
		if err := privatefs.ValidatePrivateDirectory(directory); err != nil {
			return state, stateError(ErrorInvalidState, fmt.Errorf("validate import state layout: %w", err))
		}
	}
	if err := validateCurrentEntries(state.current); err != nil {
		return state, stateError(ErrorInvalidState, err)
	}
	return state, nil
}

func readPrepare(state statePaths) (Prepare, error) {
	var prepare Prepare
	if err := readStrictJSON(state.prepare, &prepare); err != nil {
		return Prepare{}, stateError(ErrorInvalidState, fmt.Errorf("read import preparation: %w", err))
	}
	if err := prepare.validate(); err != nil {
		return Prepare{}, stateError(ErrorInvalidState, err)
	}
	return prepare, nil
}

func readVerification(state statePaths, prepare Prepare) (Verification, error) {
	var verification Verification
	if err := readStrictJSON(state.verification, &verification); err != nil {
		return Verification{}, stateError(ErrorInvalidState, fmt.Errorf("read import verification: %w", err))
	}
	if err := verification.validate(prepare); err != nil {
		return Verification{}, stateError(ErrorInvalidState, err)
	}
	return verification, nil
}

func readDecision(state statePaths, prepare Prepare) (Decision, error) {
	var decision Decision
	if err := readStrictJSON(state.decision, &decision); err != nil {
		return Decision{}, stateError(ErrorInvalidState, fmt.Errorf("read import decision: %w", err))
	}
	if err := decision.validate(prepare); err != nil {
		return Decision{}, stateError(ErrorInvalidState, err)
	}
	return decision, nil
}

func readStrictJSON(path string, destination any) error {
	return statefile.ReadJSON(path, MaxStateBytes, destination)
}

func writeNewJSON(path string, value any) error {
	return statefile.WriteNewJSON(path, MaxStateBytes, value)
}

func validateCurrentEntries(current string) error {
	entries, err := os.ReadDir(current)
	if err != nil {
		return err
	}
	allowed := map[string]struct{}{prepareName: {}, verificationName: {}, decisionName: {}}
	for _, entry := range entries {
		if _, ok := allowed[entry.Name()]; ok {
			continue
		}
		owned, err := statefile.ValidateOwnedTemporary(
			current, entry.Name(), MaxStateBytes, prepareName, verificationName, decisionName,
		)
		if err != nil {
			return err
		}
		if !owned {
			return fmt.Errorf("import current contains an unexpected entry")
		}
	}
	return nil
}

func cleanupCurrentWriteTransients(current string) error {
	return statefile.CleanupOwnedTemporaries(current, MaxStateBytes, prepareName, verificationName, decisionName)
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
