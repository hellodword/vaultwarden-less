package upgrade

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"unicode/utf8"

	"github.com/hellodword/vaultwarden-less/internal/config"
	"github.com/hellodword/vaultwarden-less/internal/privatefs"
)

const (
	prepareName      = "prepare.json"
	ticketName       = "rollback-ticket.json"
	candidateName    = "candidate.json"
	composeName      = "compose.env"
	rollbackDataName = "rollback-data"
)

type statePaths struct {
	root       string
	upgrade    string
	current    string
	history    string
	quarantine string
	prepare    string
	ticket     string
	candidate  string
	compose    string
	rollback   string
}

func paths(stateDir string) statePaths {
	upgradeRoot := filepath.Join(stateDir, "upgrade")
	current := filepath.Join(upgradeRoot, "current")
	return statePaths{
		root:       stateDir,
		upgrade:    upgradeRoot,
		current:    current,
		history:    filepath.Join(upgradeRoot, "history"),
		quarantine: filepath.Join(upgradeRoot, "quarantine"),
		prepare:    filepath.Join(current, prepareName),
		ticket:     filepath.Join(current, ticketName),
		candidate:  filepath.Join(current, candidateName),
		compose:    filepath.Join(current, composeName),
		rollback:   filepath.Join(current, rollbackDataName),
	}
}

func ensureStateLayout(stateDir string) (statePaths, error) {
	state := paths(stateDir)
	if err := privatefs.ValidatePrivateDirectory(state.root); err != nil {
		return state, stateError(ErrorInvalidState, fmt.Errorf("validate state root: %w", err))
	}
	for _, directory := range []string{state.upgrade, state.history, state.quarantine} {
		if err := privatefs.EnsurePrivateDirectory(directory); err != nil {
			return state, stateError(ErrorInvalidState, fmt.Errorf("create upgrade state layout: %w", err))
		}
	}
	return state, nil
}

func validateStateLayout(stateDir string) (statePaths, error) {
	state := paths(stateDir)
	for _, directory := range []string{state.root, state.upgrade, state.history, state.quarantine, state.current} {
		if err := privatefs.ValidatePrivateDirectory(directory); err != nil {
			return state, stateError(ErrorInvalidState, fmt.Errorf("validate upgrade state layout: %w", err))
		}
	}
	return state, nil
}

func readPrepare(state statePaths, deployment config.Deployment) (Prepare, error) {
	var prepare Prepare
	if err := readStrictJSON(state.prepare, &prepare); err != nil {
		return Prepare{}, stateError(ErrorInvalidState, fmt.Errorf("read prepare state: %w", err))
	}
	if err := prepare.validate(); err != nil {
		return Prepare{}, stateError(ErrorInvalidState, err)
	}
	compose, err := readStateBytes(state.compose)
	if err != nil {
		return Prepare{}, stateError(ErrorInvalidState, fmt.Errorf("read compose state: %w", err))
	}
	if !bytes.Equal(compose, composeEnvironment(deployment, prepare)) {
		return Prepare{}, stateError(ErrorInvalidState, fmt.Errorf("compose state does not match prepare state"))
	}
	return prepare, nil
}

func readTicket(state statePaths, prepare Prepare) (RollbackTicket, error) {
	var ticket RollbackTicket
	if err := readStrictJSON(state.ticket, &ticket); err != nil {
		return RollbackTicket{}, stateError(ErrorInvalidState, fmt.Errorf("read rollback ticket: %w", err))
	}
	if err := ticket.validate(prepare); err != nil {
		return RollbackTicket{}, stateError(ErrorInvalidState, err)
	}
	return ticket, nil
}

func readCandidate(state statePaths, prepare Prepare) (Candidate, error) {
	var candidate Candidate
	if err := readStrictJSON(state.candidate, &candidate); err != nil {
		return Candidate{}, stateError(ErrorInvalidState, fmt.Errorf("read candidate state: %w", err))
	}
	if err := candidate.validate(prepare); err != nil {
		return Candidate{}, stateError(ErrorInvalidState, err)
	}
	return candidate, nil
}

func readStrictJSON(path string, destination any) error {
	data, err := readStateBytes(path)
	if err != nil {
		return err
	}
	if len(data) == 0 || !utf8.Valid(data) {
		return fmt.Errorf("state JSON is empty or invalid UTF-8")
	}
	if err := rejectDuplicateJSONKeys(data); err != nil {
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
		return fmt.Errorf("state JSON is not in canonical form")
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

func rejectDuplicateJSONKeys(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := walkJSONValue(decoder); err != nil {
		return fmt.Errorf("state JSON is invalid: %w", err)
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("state JSON contains trailing data")
		}
		return err
	}
	return nil
}

func walkJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, composite := token.(json.Delim)
	if !composite {
		return nil
	}
	switch delimiter {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			nameToken, err := decoder.Token()
			if err != nil {
				return err
			}
			name, ok := nameToken.(string)
			if !ok {
				return fmt.Errorf("JSON object key is not a string")
			}
			if _, duplicate := seen[name]; duplicate {
				return fmt.Errorf("duplicate JSON key")
			}
			seen[name] = struct{}{}
			if err := walkJSONValue(decoder); err != nil {
				return err
			}
		}
	case '[':
		for decoder.More() {
			if err := walkJSONValue(decoder); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("unexpected JSON delimiter")
	}
	closing, err := decoder.Token()
	want := map[json.Delim]json.Delim{'{': '}', '[': ']'}[delimiter]
	if err != nil || closing != want {
		return fmt.Errorf("invalid JSON composite")
	}
	return nil
}

func writeNewJSON(path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return writeNewFile(path, data)
}

func replaceJSON(path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if int64(len(data)) > MaxStateBytes {
		return fmt.Errorf("state file exceeds limit")
	}
	if _, err := readStateBytes(path); err != nil {
		return fmt.Errorf("validate replaced state file: %w", err)
	}
	temporary, err := createSyncedTemporary(filepath.Dir(path), "."+filepath.Base(path)+"-", data)
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(temporary) }()
	if err := os.Rename(temporary, path); err != nil {
		return err
	}
	return privatefs.SyncDirectory(filepath.Dir(path))
}

func writeNewFile(path string, data []byte) error {
	if len(data) == 0 || int64(len(data)) > MaxStateBytes {
		return fmt.Errorf("state file has invalid size")
	}
	parent := filepath.Dir(path)
	if err := privatefs.ValidatePrivateDirectory(parent); err != nil {
		return err
	}
	temporary, err := createSyncedTemporary(parent, "."+filepath.Base(path)+"-", data)
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(temporary) }()
	if err := privatefs.RenameNoReplace(temporary, path); err != nil {
		return err
	}
	return privatefs.SyncDirectory(parent)
}

func createSyncedTemporary(parent, pattern string, data []byte) (string, error) {
	file, err := os.CreateTemp(parent, pattern)
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

func composeEnvironment(deployment config.Deployment, prepare Prepare) []byte {
	return []byte(strings.Join([]string{
		"DEPLOYMENT_SCHEMA_VERSION=1",
		"RUNTIME_UID=" + strconv.FormatUint(uint64(deployment.RuntimeUID), 10),
		"RUNTIME_GID=" + strconv.FormatUint(uint64(deployment.RuntimeGID), 10),
		"LISTEN_PORT=" + strconv.FormatUint(uint64(deployment.ListenPort), 10),
		"DRILL_PORT=" + strconv.FormatUint(uint64(deployment.DrillPort), 10),
		"A_RELEASE=" + prepare.FromRelease,
		"B_RELEASE=" + prepare.ToRelease,
	}, "\n") + "\n")
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

func validateCurrentEntries(current string) error {
	entries, err := os.ReadDir(current)
	if err != nil {
		return err
	}
	allowed := map[string]struct{}{
		prepareName: {}, ticketName: {}, candidateName: {}, composeName: {}, rollbackDataName: {},
	}
	for _, entry := range entries {
		if _, ok := allowed[entry.Name()]; !ok {
			return fmt.Errorf("upgrade current contains an unexpected entry")
		}
	}
	return nil
}

func validateOpaqueCandidate(path string) (bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o600 ||
		!ok || stat.Uid != uint32(os.Geteuid()) || stat.Gid != uint32(os.Getegid()) || stat.Nlink != 1 || info.Size() > MaxStateBytes {
		return false, fmt.Errorf("candidate path is unsafe")
	}
	return true, nil
}
