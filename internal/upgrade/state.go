package upgrade

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/hellodword/vaultwarden-less/internal/config"
	"github.com/hellodword/vaultwarden-less/internal/privatefs"
	"github.com/hellodword/vaultwarden-less/internal/statefile"
)

const (
	prepareName      = "prepare.json"
	ticketName       = "rollback-ticket.json"
	candidateName    = "candidate.json"
	decisionName     = "decision.json"
	composeName      = "compose.env"
	rollbackDataName = "rollback-data"
	stateWritePrefix = ".vaultwarden-less-upgrade-state-"
	stateWriteSuffix = ".tmp"
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
	decision   string
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
		decision:   filepath.Join(current, decisionName),
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

func readDecision(state statePaths, prepare Prepare) (Decision, error) {
	var decision Decision
	if err := readStrictJSON(state.decision, &decision); err != nil {
		return Decision{}, stateError(ErrorInvalidState, fmt.Errorf("read upgrade decision: %w", err))
	}
	if err := decision.validate(prepare); err != nil {
		return Decision{}, stateError(ErrorInvalidState, err)
	}
	return decision, nil
}

func readStrictJSON(path string, destination any) error {
	return statefile.ReadJSON(path, MaxStateBytes, destination)
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
	return statefile.WriteNewJSON(path, MaxStateBytes, value)
}

func replaceJSON(path string, value any) error {
	return statefile.ReplaceJSON(path, MaxStateBytes, value)
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
	if target != composeName {
		return "", fmt.Errorf("upgrade state write target is invalid")
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

func composeEnvironment(deployment config.Deployment, prepare Prepare) []byte {
	return []byte(strings.Join([]string{
		"DEPLOYMENT_ROOT=" + deployment.Root,
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
		prepareName: {}, ticketName: {}, candidateName: {}, decisionName: {}, composeName: {}, rollbackDataName: {},
	}
	for _, entry := range entries {
		if _, ok := allowed[entry.Name()]; ok {
			continue
		}
		owned, err := statefile.ValidateOwnedTemporary(
			current, entry.Name(), MaxStateBytes, prepareName, ticketName, candidateName, decisionName,
		)
		if err != nil {
			return err
		}
		if owned {
			continue
		}
		if !composeWriteTemporary(entry.Name()) {
			return fmt.Errorf("upgrade current contains an unexpected entry")
		}
		if err := validateComposeWriteTemporary(current, entry.Name()); err != nil {
			return err
		}
	}
	return nil
}

func cleanupCurrentWriteTransients(current string) error {
	if err := privatefs.ValidatePrivateDirectory(current); err != nil {
		return err
	}
	if err := statefile.CleanupOwnedTemporaries(
		current, MaxStateBytes, prepareName, ticketName, candidateName, decisionName,
	); err != nil {
		return err
	}
	entries, err := os.ReadDir(current)
	if err != nil {
		return err
	}
	removed := false
	for _, entry := range entries {
		if !composeWriteTemporary(entry.Name()) {
			continue
		}
		if err := validateComposeWriteTemporary(current, entry.Name()); err != nil {
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

func composeWriteTemporary(name string) bool {
	prefix := stateWritePrefix + composeName + "-"
	return strings.HasPrefix(name, prefix) && strings.HasSuffix(name, stateWriteSuffix) &&
		len(name) > len(prefix)+len(stateWriteSuffix)
}

func validateComposeWriteTemporary(current, name string) error {
	info, err := os.Lstat(filepath.Join(current, name))
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o600 ||
		!ok || stat.Uid != uint32(os.Geteuid()) || stat.Gid != uint32(os.Getegid()) || stat.Nlink != 1 || info.Size() > MaxStateBytes {
		return fmt.Errorf("upgrade current contains an unsafe state-write transient")
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
