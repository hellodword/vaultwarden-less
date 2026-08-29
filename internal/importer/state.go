package importer

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unicode/utf8"

	"github.com/hellodword/vaultwarden-less/internal/privatefs"
	"github.com/hellodword/vaultwarden-less/internal/strictjson"
)

const (
	prepareName      = "prepare.json"
	verificationName = "verification.json"
	decisionName     = "decision.json"
	currentTransient = ".current-initial"
	verifyTransient  = ".verification-initial"
	stateWritePrefix = ".vaultwarden-less-import-state-"
	stateWriteSuffix = ".tmp"
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
	data, err := readStateBytes(path)
	if err != nil {
		return err
	}
	if len(data) == 0 || !utf8.Valid(data) {
		return fmt.Errorf("state JSON is empty or invalid UTF-8")
	}
	if err := strictjson.RejectDuplicateKeys(data); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return fmt.Errorf("state JSON contains trailing values")
	}
	canonical, err := json.Marshal(destination)
	if err != nil {
		return err
	}
	canonical = append(canonical, '\n')
	if !bytes.Equal(data, canonical) {
		return fmt.Errorf("state JSON is not canonical")
	}
	return nil
}

func readStateBytes(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || !ok || stat.Nlink != 1 || info.Size() > MaxStateBytes {
		return nil, fmt.Errorf("state path is not one bounded non-linked regular file")
	}
	data, err := privatefs.ReadBytes(path, MaxStateBytes)
	if err != nil {
		return nil, err
	}
	latest, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	latestStat, ok := latest.Sys().(*syscall.Stat_t)
	if !ok || latestStat.Nlink != 1 || !privatefs.SameFileVersion(info, latest) {
		return nil, fmt.Errorf("state path changed while reading")
	}
	return data, nil
}

func writeNewJSON(path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return writeNewFile(path, data)
}

func writeNewFile(path string, data []byte) error {
	if len(data) == 0 || int64(len(data)) > MaxStateBytes {
		return fmt.Errorf("state file has invalid size")
	}
	parent := filepath.Dir(path)
	if err := privatefs.ValidatePrivateDirectory(parent); err != nil {
		return err
	}
	temporary, err := createSyncedTemporary(parent, filepath.Base(path), data)
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(temporary) }()
	if err := privatefs.RenameNoReplace(temporary, path); err != nil {
		return err
	}
	return privatefs.SyncDirectory(parent)
}

func createSyncedTemporary(parent, target string, data []byte) (string, error) {
	if !stateWriteTarget(target) {
		return "", fmt.Errorf("import state write target is invalid")
	}
	file, err := os.CreateTemp(parent, stateWritePrefix+target+"-*"+stateWriteSuffix)
	if err != nil {
		return "", err
	}
	path := file.Name()
	open := true
	defer func() {
		if open {
			_ = file.Close()
		}
	}()
	if err := file.Chmod(0o600); err != nil {
		_ = os.Remove(path)
		return "", err
	}
	if _, err := file.Write(data); err != nil {
		_ = os.Remove(path)
		return "", err
	}
	if err := file.Sync(); err != nil {
		_ = os.Remove(path)
		return "", err
	}
	if err := file.Close(); err != nil {
		open = false
		_ = os.Remove(path)
		return "", err
	}
	open = false
	return path, nil
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
		if !stateWriteTemporary(entry.Name()) {
			return fmt.Errorf("import current contains an unexpected entry")
		}
		if err := validateStateWriteTemporary(current, entry.Name()); err != nil {
			return err
		}
	}
	return nil
}

func cleanupCurrentWriteTransients(current string) error {
	if err := privatefs.ValidatePrivateDirectory(current); err != nil {
		return err
	}
	entries, err := os.ReadDir(current)
	if err != nil {
		return err
	}
	removed := false
	for _, entry := range entries {
		if !stateWriteTemporary(entry.Name()) {
			continue
		}
		if err := validateStateWriteTemporary(current, entry.Name()); err != nil {
			return err
		}
		if err := os.Remove(filepath.Join(current, entry.Name())); err != nil {
			return err
		}
		removed = true
	}
	if removed {
		return privatefs.SyncDirectory(current)
	}
	return nil
}

func stateWriteTarget(name string) bool {
	return name == prepareName || name == verificationName || name == decisionName
}

func stateWriteTemporary(name string) bool {
	if !strings.HasSuffix(name, stateWriteSuffix) {
		return false
	}
	for _, target := range []string{prepareName, verificationName, decisionName} {
		prefix := stateWritePrefix + target + "-"
		if strings.HasPrefix(name, prefix) && len(name) > len(prefix)+len(stateWriteSuffix) {
			return true
		}
	}
	return false
}

func validateStateWriteTemporary(current, name string) error {
	info, err := os.Lstat(filepath.Join(current, name))
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o600 ||
		!ok || stat.Uid != uint32(os.Geteuid()) || stat.Gid != uint32(os.Getegid()) || stat.Nlink != 1 || info.Size() > MaxStateBytes {
		return fmt.Errorf("import current contains an unsafe state-write transient")
	}
	return nil
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
