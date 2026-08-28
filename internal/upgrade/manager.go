package upgrade

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/hellodword/vaultwarden-less/internal/config"
	"github.com/hellodword/vaultwarden-less/internal/privatefs"
)

type EvidenceCollector func(context.Context, string, []string) ([]TargetEvidence, error)

type Manager struct {
	StateDir      string
	DataDir       string
	Deployment    config.Deployment
	ProjectCommit string
	Release       string
	Now           func() time.Time
	Exchange      func(string, string) error
	Device        func(string) (uint64, error)
	Database      func(context.Context, string) (DatabaseContract, error)
	PrepareFreeze func(context.Context, string) error
}

func (manager Manager) Prepare(toCommit, toRelease string) (Result, error) {
	if err := manager.validate(); err != nil {
		return Result{}, err
	}
	if !ValidCommit(toCommit) || !ValidRelease(toRelease, toCommit) {
		return Result{}, stateError(ErrorInvalidState, fmt.Errorf("candidate identity is invalid"))
	}
	if toCommit == manager.ProjectCommit {
		return Result{}, stateError(ErrorConflict, fmt.Errorf("upgrade commits must differ"))
	}
	state, err := ensureStateLayout(manager.StateDir)
	if err != nil {
		return Result{}, err
	}
	exists, err := pathExists(state.current)
	if err != nil {
		return Result{}, stateError(ErrorInvalidState, err)
	}
	if exists {
		if err := privatefs.ValidatePrivateDirectory(state.current); err != nil {
			return Result{}, stateError(ErrorInvalidState, err)
		}
		if err := validateCurrentEntries(state.current); err != nil {
			return Result{}, stateError(ErrorInvalidState, err)
		}
		prepare, err := readPrepare(state, manager.Deployment)
		if err != nil {
			return Result{}, err
		}
		if prepare.FromCommit != manager.ProjectCommit || prepare.FromRelease != manager.Release ||
			prepare.ToCommit != toCommit || prepare.ToRelease != toRelease {
			return Result{}, stateError(ErrorConflict, fmt.Errorf("another upgrade is already current"))
		}
		for _, path := range []string{state.ticket, state.candidate, state.rollback} {
			if exists, err := pathExists(path); err != nil {
				return Result{}, stateError(ErrorInvalidState, err)
			} else if exists {
				return Result{}, stateError(ErrorConflict, fmt.Errorf("prepare phase has already completed"))
			}
		}
		return result(PhasePrepared, prepare, nil), nil
	}

	prepare := Prepare{
		SchemaVersion: SchemaVersion,
		UpgradeID:     upgradeID(manager.ProjectCommit, toCommit),
		Phase:         PhasePrepared,
		FromCommit:    manager.ProjectCommit,
		FromRelease:   manager.Release,
		ToCommit:      toCommit,
		ToRelease:     toRelease,
		PreparedAt:    formatTimestamp(manager.now()),
	}
	if err := prepare.validate(); err != nil {
		return Result{}, stateError(ErrorInvalidState, err)
	}
	for _, completed := range []string{
		filepath.Join(state.history, prepare.UpgradeID),
		filepath.Join(state.quarantine, prepare.UpgradeID),
	} {
		if exists, err := pathExists(completed); err != nil {
			return Result{}, stateError(ErrorInvalidState, err)
		} else if exists {
			return Result{}, stateError(ErrorConflict, fmt.Errorf("upgrade identity already has durable state"))
		}
	}
	temporary, err := os.MkdirTemp(state.upgrade, ".current-")
	if err != nil {
		return Result{}, stateError(ErrorInvalidState, err)
	}
	published := false
	defer func() {
		if !published {
			_ = os.RemoveAll(temporary)
		}
	}()
	if err := os.Chmod(temporary, 0o700); err != nil {
		return Result{}, stateError(ErrorInvalidState, err)
	}
	if err := writeNewJSON(filepath.Join(temporary, prepareName), prepare); err != nil {
		return Result{}, stateError(ErrorInvalidState, err)
	}
	if err := writeNewFile(filepath.Join(temporary, composeName), composeEnvironment(manager.Deployment, prepare)); err != nil {
		return Result{}, stateError(ErrorInvalidState, err)
	}
	if err := privatefs.SyncDirectory(temporary); err != nil {
		return Result{}, stateError(ErrorInvalidState, err)
	}
	if err := privatefs.RenameNoReplace(temporary, state.current); err != nil {
		return Result{}, stateError(ErrorConflict, err)
	}
	published = true
	if err := privatefs.SyncDirectory(state.upgrade); err != nil {
		return Result{}, stateError(ErrorInvalidState, err)
	}
	return result(PhasePrepared, prepare, nil), nil
}

func (manager Manager) Freeze(ctx context.Context, targetNames []string, collect EvidenceCollector) (Result, error) {
	state, prepare, err := manager.currentForRole(false)
	if err != nil {
		return Result{}, err
	}
	if err := validateTargetNames(targetNames); err != nil {
		return Result{}, stateError(ErrorInvalidState, err)
	}
	if candidate, err := pathExists(state.candidate); err != nil {
		return Result{}, stateError(ErrorInvalidState, err)
	} else if candidate {
		return Result{}, stateError(ErrorConflict, fmt.Errorf("candidate phase already started"))
	}
	if ticketExists, err := pathExists(state.ticket); err != nil {
		return Result{}, stateError(ErrorInvalidState, err)
	} else if ticketExists {
		ticket, err := readTicket(state, prepare)
		if err != nil {
			return Result{}, err
		}
		if err := validateEvidence(ticket.Targets, targetNames); err != nil {
			return Result{}, stateError(ErrorConflict, err)
		}
		if err := manager.validateFrozenTree(ctx, state.rollback, ticket); err != nil {
			return Result{}, err
		}
		return result(PhaseFrozen, prepare, ticket.Targets), nil
	}
	if exists, err := pathExists(state.rollback); err != nil {
		return Result{}, stateError(ErrorInvalidState, err)
	} else if exists {
		return Result{}, stateError(ErrorInvalidState, fmt.Errorf("rollback tree exists without a ticket"))
	}
	if collect == nil {
		return Result{}, stateError(ErrorVerification, fmt.Errorf("evidence collector is unavailable"))
	}
	if err := privatefs.ValidatePrivateDirectory(manager.DataDir); err != nil {
		return Result{}, stateError(ErrorSourceInconsistent, err)
	}
	if manager.PrepareFreeze == nil {
		return Result{}, stateError(ErrorSourceInconsistent, fmt.Errorf("freeze preparation is unavailable"))
	}
	if err := manager.PrepareFreeze(ctx, manager.DataDir); err != nil {
		return Result{}, stateError(ErrorSourceInconsistent, err)
	}
	if err := rejectLiveDatabaseSidecars(manager.DataDir); err != nil {
		return Result{}, stateError(ErrorSourceInconsistent, err)
	}
	if err := normalizePrivateTree(manager.DataDir); err != nil {
		return Result{}, stateError(ErrorSourceInconsistent, err)
	}
	before, err := rollbackTreeID(ctx, manager.DataDir)
	if err != nil {
		return Result{}, stateError(ErrorSourceInconsistent, err)
	}
	evidence, err := collect(ctx, prepare.FromCommit, slices.Clone(targetNames))
	if err != nil {
		return Result{}, err
	}
	if err := validateEvidence(evidence, targetNames); err != nil {
		return Result{}, stateError(ErrorVerification, err)
	}
	if err := rejectLiveDatabaseSidecars(manager.DataDir); err != nil {
		return Result{}, stateError(ErrorSourceInconsistent, err)
	}
	afterEvidence, err := rollbackTreeID(ctx, manager.DataDir)
	if err != nil || afterEvidence != before {
		return Result{}, stateError(ErrorSourceInconsistent, fmt.Errorf("active tree changed while collecting evidence"))
	}
	temporary, err := absentTemporaryDirectory(state.current, ".rollback-data-")
	if err != nil {
		return Result{}, stateError(ErrorInvalidState, err)
	}
	published := false
	defer func() {
		if !published {
			_ = os.RemoveAll(temporary)
		}
	}()
	if err := copyPrivateTree(ctx, manager.DataDir, temporary); err != nil {
		return Result{}, stateError(ErrorSourceInconsistent, err)
	}
	copiedID, err := rollbackTreeID(ctx, temporary)
	if err != nil {
		return Result{}, stateError(ErrorVerification, err)
	}
	finalSourceID, err := rollbackTreeID(ctx, manager.DataDir)
	if err != nil || before != finalSourceID || before != copiedID {
		return Result{}, stateError(ErrorSourceInconsistent, fmt.Errorf("rollback copy does not match frozen source"))
	}
	activeDevice, err := manager.device()(manager.DataDir)
	if err != nil {
		return Result{}, stateError(ErrorInvalidState, err)
	}
	rollbackDevice, err := manager.device()(temporary)
	if err != nil || activeDevice == 0 || activeDevice != rollbackDevice {
		return Result{}, stateError(ErrorConflict, fmt.Errorf("active and rollback paths are on different filesystems"))
	}
	if err := privatefs.RenameNoReplace(temporary, state.rollback); err != nil {
		return Result{}, stateError(ErrorConflict, err)
	}
	published = true
	if err := privatefs.SyncDirectory(state.current); err != nil {
		return Result{}, stateError(ErrorInvalidState, err)
	}
	ticket := RollbackTicket{
		SchemaVersion:  SchemaVersion,
		UpgradeID:      prepare.UpgradeID,
		Phase:          PhaseFrozen,
		FromCommit:     prepare.FromCommit,
		FromRelease:    prepare.FromRelease,
		ToCommit:       prepare.ToCommit,
		ToRelease:      prepare.ToRelease,
		ActivePath:     "data",
		RollbackPath:   "state/upgrade/current/rollback-data",
		ActiveDevice:   activeDevice,
		RollbackDevice: rollbackDevice,
		RollbackTreeID: copiedID,
		FrozenAt:       formatTimestamp(manager.now()),
		Targets:        slices.Clone(evidence),
	}
	if err := ticket.validate(prepare); err != nil {
		return Result{}, stateError(ErrorInvalidState, err)
	}
	if err := writeNewJSON(state.ticket, ticket); err != nil {
		return Result{}, stateError(ErrorInvalidState, err)
	}
	return result(PhaseFrozen, prepare, ticket.Targets), nil
}

func (manager Manager) MigrateCheck(ctx context.Context) (Result, error) {
	state, prepare, ticket, err := manager.frozenForRole(true)
	if err != nil {
		return Result{}, err
	}
	if exists, err := pathExists(state.candidate); err != nil {
		return Result{}, stateError(ErrorInvalidState, err)
	} else if exists {
		return Result{}, stateError(ErrorConflict, fmt.Errorf("candidate state already exists"))
	}
	if err := manager.validateFrozenTree(ctx, state.rollback, ticket); err != nil {
		return Result{}, err
	}
	database, err := manager.databaseContract(ctx)
	if err != nil {
		return Result{}, stateError(ErrorVerification, err)
	}
	candidate := Candidate{
		SchemaVersion:             SchemaVersion,
		UpgradeID:                 prepare.UpgradeID,
		Phase:                     PhaseMigrated,
		FromCommit:                prepare.FromCommit,
		ToCommit:                  prepare.ToCommit,
		DatabaseMigration:         database.Migration,
		DatabaseSchemaFingerprint: database.SchemaFingerprint,
		UpdatedAt:                 formatTimestamp(manager.now()),
		AcceptedAt:                nil,
		Targets:                   []TargetEvidence{},
	}
	if err := candidate.validate(prepare); err != nil {
		return Result{}, stateError(ErrorInvalidState, err)
	}
	if err := writeNewJSON(state.candidate, candidate); err != nil {
		return Result{}, stateError(ErrorInvalidState, err)
	}
	return result(PhaseMigrated, prepare, nil), nil
}

func (manager Manager) Verify(ctx context.Context, targetNames []string, collect EvidenceCollector) (Result, error) {
	state, prepare, ticket, err := manager.frozenForRole(true)
	if err != nil {
		return Result{}, err
	}
	if err := validateTargetNames(targetNames); err != nil {
		return Result{}, stateError(ErrorInvalidState, err)
	}
	if err := validateEvidence(ticket.Targets, targetNames); err != nil {
		return Result{}, stateError(ErrorConflict, fmt.Errorf("verification targets do not match frozen evidence"))
	}
	if err := manager.validateFrozenTree(ctx, state.rollback, ticket); err != nil {
		return Result{}, err
	}
	candidate, err := readCandidate(state, prepare)
	if err != nil {
		return Result{}, err
	}
	if candidate.Phase != PhaseMigrated {
		return Result{}, stateError(ErrorConflict, fmt.Errorf("candidate is not in migrated phase"))
	}
	database, err := manager.databaseContract(ctx)
	if err != nil || database.Migration != candidate.DatabaseMigration || database.SchemaFingerprint != candidate.DatabaseSchemaFingerprint {
		return Result{}, stateError(ErrorVerification, fmt.Errorf("active database does not match migrated evidence"))
	}
	if collect == nil {
		return Result{}, stateError(ErrorVerification, fmt.Errorf("evidence collector is unavailable"))
	}
	evidence, err := collect(ctx, prepare.ToCommit, slices.Clone(targetNames))
	if err != nil {
		return Result{}, err
	}
	if err := validateEvidence(evidence, targetNames); err != nil {
		return Result{}, stateError(ErrorVerification, err)
	}
	databaseAfter, err := manager.databaseContract(ctx)
	if err != nil || databaseAfter != database {
		return Result{}, stateError(ErrorVerification, fmt.Errorf("active database changed during verification"))
	}
	candidate.Phase = PhaseVerified
	candidate.UpdatedAt = formatTimestamp(manager.now())
	candidate.Targets = slices.Clone(evidence)
	if err := candidate.validate(prepare); err != nil {
		return Result{}, stateError(ErrorInvalidState, err)
	}
	if err := replaceJSON(state.candidate, candidate); err != nil {
		return Result{}, stateError(ErrorInvalidState, err)
	}
	return result(PhaseVerified, prepare, candidate.Targets), nil
}

func (manager Manager) Accept(ctx context.Context, drillCheck func(context.Context) error) (Result, error) {
	state, prepare, ticket, err := manager.frozenForRole(true)
	if err != nil {
		return Result{}, err
	}
	if err := manager.validateFrozenTree(ctx, state.rollback, ticket); err != nil {
		return Result{}, err
	}
	candidate, err := readCandidate(state, prepare)
	if err != nil {
		return Result{}, err
	}
	if candidate.Phase != PhaseVerified && candidate.Phase != PhaseAccepted {
		return Result{}, stateError(ErrorConflict, fmt.Errorf("candidate is not verified"))
	}
	targetNames := make([]string, len(ticket.Targets))
	for index := range ticket.Targets {
		targetNames[index] = ticket.Targets[index].Target
	}
	if err := validateEvidence(candidate.Targets, targetNames); err != nil {
		return Result{}, stateError(ErrorInvalidState, fmt.Errorf("candidate targets do not match frozen evidence"))
	}
	if drillCheck == nil {
		return Result{}, stateError(ErrorVerification, fmt.Errorf("drill verification is unavailable"))
	}
	if err := drillCheck(ctx); err != nil {
		return Result{}, stateError(ErrorVerification, err)
	}
	historyPath := filepath.Join(state.history, prepare.UpgradeID)
	if exists, err := pathExists(historyPath); err != nil {
		return Result{}, stateError(ErrorInvalidState, err)
	} else if exists {
		return Result{}, stateError(ErrorConflict, fmt.Errorf("upgrade history already exists"))
	}
	if exists, err := pathExists(filepath.Join(state.quarantine, prepare.UpgradeID)); err != nil {
		return Result{}, stateError(ErrorInvalidState, err)
	} else if exists {
		return Result{}, stateError(ErrorConflict, fmt.Errorf("rollback quarantine already exists"))
	}
	if candidate.Phase == PhaseVerified {
		acceptedAt := formatTimestamp(manager.now())
		candidate.Phase = PhaseAccepted
		candidate.UpdatedAt = acceptedAt
		candidate.AcceptedAt = &acceptedAt
		if err := candidate.validate(prepare); err != nil {
			return Result{}, stateError(ErrorInvalidState, err)
		}
		if err := replaceJSON(state.candidate, candidate); err != nil {
			return Result{}, stateError(ErrorInvalidState, err)
		}
	}
	if err := privatefs.RenameNoReplace(state.current, historyPath); err != nil {
		return Result{}, stateError(ErrorConflict, err)
	}
	if err := privatefs.SyncDirectory(state.history); err != nil {
		return Result{}, stateError(ErrorInvalidState, err)
	}
	if err := privatefs.SyncDirectory(state.upgrade); err != nil {
		return Result{}, stateError(ErrorInvalidState, err)
	}
	return result(PhaseAccepted, prepare, nil), nil
}

func (manager Manager) Rollback(ctx context.Context) (Result, error) {
	state, prepare, ticket, err := manager.frozenForRole(false)
	if err != nil {
		return Result{}, err
	}
	historyPath := filepath.Join(state.history, prepare.UpgradeID)
	if exists, err := pathExists(historyPath); err != nil {
		return Result{}, stateError(ErrorInvalidState, err)
	} else if exists {
		return Result{}, stateError(ErrorConflict, fmt.Errorf("upgrade history already exists"))
	}
	candidateExists, err := validateOpaqueCandidate(state.candidate)
	if err != nil {
		return Result{}, stateError(ErrorInvalidState, err)
	}
	activeDevice, err := manager.device()(manager.DataDir)
	if err != nil || activeDevice != ticket.ActiveDevice {
		return Result{}, stateError(ErrorConflict, fmt.Errorf("active filesystem device changed"))
	}
	rollbackExists, err := pathExists(state.rollback)
	if err != nil {
		return Result{}, stateError(ErrorInvalidState, err)
	}
	quarantinePath := filepath.Join(state.quarantine, prepare.UpgradeID)
	quarantineData := filepath.Join(quarantinePath, "data")
	quarantineCandidate := filepath.Join(quarantinePath, candidateName)
	quarantineExists, err := pathExists(quarantinePath)
	if err != nil {
		return Result{}, stateError(ErrorInvalidState, err)
	}
	if quarantineExists {
		if err := validateQuarantineDirectory(quarantinePath); err != nil {
			return Result{}, stateError(ErrorInvalidState, err)
		}
	}
	quarantineDataExists, err := pathExists(quarantineData)
	if err != nil {
		return Result{}, stateError(ErrorInvalidState, err)
	}
	quarantineCandidateExists, err := validateOpaqueCandidate(quarantineCandidate)
	if err != nil {
		return Result{}, stateError(ErrorInvalidState, err)
	}
	if rollbackExists && quarantineDataExists {
		return Result{}, stateError(ErrorInvalidState, fmt.Errorf("rollback data exists in both current and quarantine"))
	}
	if candidateExists && quarantineCandidateExists {
		return Result{}, stateError(ErrorInvalidState, fmt.Errorf("candidate exists in both current and quarantine"))
	}
	exchanged := false
	activeID := ""
	if rollbackExists {
		rollbackDevice, err := manager.device()(state.rollback)
		if err != nil || rollbackDevice != ticket.RollbackDevice {
			return Result{}, stateError(ErrorConflict, fmt.Errorf("rollback filesystem device changed"))
		}
		rollbackID, rollbackErr := rollbackTreeID(ctx, state.rollback)
		if rollbackErr == nil && rollbackID == ticket.RollbackTreeID {
			exchanged = false
		} else if activeID, err = rollbackTreeID(ctx, manager.DataDir); err == nil && activeID == ticket.RollbackTreeID {
			exchanged = true
		} else {
			return Result{}, stateError(ErrorVerification, fmt.Errorf("neither rollback position contains frozen A data"))
		}
	} else {
		activeID, err = rollbackTreeID(ctx, manager.DataDir)
		if err != nil {
			return Result{}, stateError(ErrorVerification, err)
		}
		if !quarantineDataExists || activeID != ticket.RollbackTreeID {
			return Result{}, stateError(ErrorInvalidState, fmt.Errorf("rollback exchange state is incomplete"))
		}
		exchanged = true
	}
	if !quarantineExists {
		if err := os.Mkdir(quarantinePath, 0o700); err != nil {
			return Result{}, stateError(ErrorInvalidState, err)
		}
		if err := privatefs.SyncDirectory(state.quarantine); err != nil {
			return Result{}, stateError(ErrorInvalidState, err)
		}
	}
	if !exchanged {
		if err := manager.exchange()(manager.DataDir, state.rollback); err != nil {
			return Result{}, stateError(ErrorConflict, err)
		}
		if err := privatefs.SyncDirectory(filepath.Dir(manager.DataDir)); err != nil {
			return Result{}, stateError(ErrorInvalidState, err)
		}
		activeID, err = rollbackTreeID(ctx, manager.DataDir)
		if err != nil || activeID != ticket.RollbackTreeID {
			return Result{}, stateError(ErrorVerification, fmt.Errorf("atomic exchange did not restore A data"))
		}
	}
	if rollbackExists {
		if err := privatefs.RenameNoReplace(state.rollback, quarantineData); err != nil {
			return Result{}, stateError(ErrorInvalidState, err)
		}
	}
	if candidateExists {
		if err := privatefs.RenameNoReplace(state.candidate, quarantineCandidate); err != nil {
			return Result{}, stateError(ErrorInvalidState, err)
		}
	}
	if err := privatefs.SyncDirectory(quarantinePath); err != nil {
		return Result{}, stateError(ErrorInvalidState, err)
	}
	if err := removeRSAState(manager.DataDir); err != nil {
		return Result{}, stateError(ErrorVerification, err)
	}
	if err := privatefs.RenameNoReplace(state.current, historyPath); err != nil {
		return Result{}, stateError(ErrorConflict, err)
	}
	if err := privatefs.SyncDirectory(state.history); err != nil {
		return Result{}, stateError(ErrorInvalidState, err)
	}
	if err := privatefs.SyncDirectory(state.upgrade); err != nil {
		return Result{}, stateError(ErrorInvalidState, err)
	}
	return result(PhaseRolledBack, prepare, nil), nil
}

func ServingAllowed(stateDir, projectCommit string, deployment config.Deployment) error {
	if !ValidCommit(projectCommit) {
		return stateError(ErrorInvalidState, fmt.Errorf("serving identity is invalid"))
	}
	state := paths(stateDir)
	currentExists, err := pathExists(state.current)
	if err != nil {
		return stateError(ErrorInvalidState, err)
	}
	if !currentExists {
		return nil
	}
	if err := privatefs.ValidatePrivateDirectory(state.current); err != nil {
		return stateError(ErrorInvalidState, err)
	}
	if err := validateCurrentEntries(state.current); err != nil {
		return stateError(ErrorInvalidState, err)
	}
	prepare, err := readPrepare(state, deployment)
	if err != nil {
		return stateError(ErrorInvalidState, fmt.Errorf("prepare state is invalid"))
	}
	ticketExists, err := pathExists(state.ticket)
	if err != nil {
		return stateError(ErrorInvalidState, err)
	}
	candidateExists, err := pathExists(state.candidate)
	if err != nil {
		return stateError(ErrorInvalidState, err)
	}
	rollbackExists, err := pathExists(state.rollback)
	if err != nil {
		return stateError(ErrorInvalidState, err)
	}
	if ticketExists {
		if _, err := readTicket(state, prepare); err != nil {
			return err
		}
		return stateError(ErrorConflict, fmt.Errorf("serving is frozen pending accept or rollback"))
	}
	if candidateExists || rollbackExists {
		return stateError(ErrorInvalidState, fmt.Errorf("upgrade state is incomplete"))
	}
	if projectCommit == prepare.FromCommit {
		return nil
	}
	if projectCommit == prepare.ToCommit {
		return stateError(ErrorConflict, fmt.Errorf("candidate serving is not accepted"))
	}
	return stateError(ErrorInvalidState, fmt.Errorf("serving identity is unrelated to current upgrade"))
}

func (manager Manager) validate() error {
	if !ValidCommit(manager.ProjectCommit) || !ValidRelease(manager.Release, manager.ProjectCommit) {
		return stateError(ErrorInvalidState, fmt.Errorf("manager release identity is invalid"))
	}
	if manager.StateDir == "" || manager.DataDir == "" || !filepath.IsAbs(manager.StateDir) || !filepath.IsAbs(manager.DataDir) ||
		filepath.Clean(manager.StateDir) != manager.StateDir || filepath.Clean(manager.DataDir) != manager.DataDir ||
		manager.StateDir == string(filepath.Separator) || manager.DataDir == string(filepath.Separator) {
		return stateError(ErrorInvalidState, fmt.Errorf("manager paths are invalid"))
	}
	if manager.Deployment.SchemaVersion != config.SchemaVersion || manager.Deployment.RuntimeUID == 0 ||
		manager.Deployment.RuntimeGID == 0 || manager.Deployment.ListenPort < 1024 ||
		manager.Deployment.DrillPort < 1024 || manager.Deployment.ListenPort == manager.Deployment.DrillPort {
		return stateError(ErrorInvalidState, fmt.Errorf("deployment identity is invalid"))
	}
	return nil
}

func (manager Manager) currentForRole(candidate bool) (statePaths, Prepare, error) {
	if err := manager.validate(); err != nil {
		return statePaths{}, Prepare{}, err
	}
	state, err := validateStateLayout(manager.StateDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return state, Prepare{}, stateError(ErrorConflict, err)
		}
		return state, Prepare{}, err
	}
	if err := validateCurrentEntries(state.current); err != nil {
		return state, Prepare{}, stateError(ErrorInvalidState, err)
	}
	prepare, err := readPrepare(state, manager.Deployment)
	if err != nil {
		return state, Prepare{}, err
	}
	wantCommit, wantRelease := prepare.FromCommit, prepare.FromRelease
	if candidate {
		wantCommit, wantRelease = prepare.ToCommit, prepare.ToRelease
	}
	if manager.ProjectCommit != wantCommit || manager.Release != wantRelease {
		return state, Prepare{}, stateError(ErrorConflict, fmt.Errorf("command is owned by the other release"))
	}
	return state, prepare, nil
}

func (manager Manager) frozenForRole(candidate bool) (statePaths, Prepare, RollbackTicket, error) {
	state, prepare, err := manager.currentForRole(candidate)
	if err != nil {
		return state, Prepare{}, RollbackTicket{}, err
	}
	ticket, err := readTicket(state, prepare)
	if err != nil {
		return state, prepare, RollbackTicket{}, err
	}
	return state, prepare, ticket, nil
}

func (manager Manager) validateFrozenTree(ctx context.Context, rollback string, ticket RollbackTicket) error {
	activeDevice, err := manager.device()(manager.DataDir)
	if err != nil || activeDevice != ticket.ActiveDevice {
		return stateError(ErrorConflict, fmt.Errorf("active filesystem device changed"))
	}
	rollbackDevice, err := manager.device()(rollback)
	if err != nil || rollbackDevice != ticket.RollbackDevice || rollbackDevice != activeDevice {
		return stateError(ErrorConflict, fmt.Errorf("rollback filesystem device changed"))
	}
	identity, err := rollbackTreeID(ctx, rollback)
	if err != nil || identity != ticket.RollbackTreeID {
		return stateError(ErrorVerification, fmt.Errorf("rollback tree identity changed"))
	}
	return nil
}

func validateTargetNames(names []string) error {
	if len(names) == 0 {
		return fmt.Errorf("verification targets must not be empty")
	}
	seen := make(map[string]struct{}, len(names))
	for _, name := range names {
		if !targetPattern.MatchString(name) {
			return fmt.Errorf("verification target name is invalid")
		}
		if _, duplicate := seen[name]; duplicate {
			return fmt.Errorf("verification target names are not unique")
		}
		seen[name] = struct{}{}
	}
	return nil
}

func absentTemporaryDirectory(parent, pattern string) (string, error) {
	temporary, err := os.MkdirTemp(parent, pattern)
	if err != nil {
		return "", err
	}
	if err := os.Remove(temporary); err != nil {
		return "", err
	}
	return temporary, nil
}

func validateQuarantineDirectory(path string) error {
	if err := privatefs.ValidatePrivateDirectory(path); err != nil {
		return err
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Name() != "data" && entry.Name() != candidateName {
			return fmt.Errorf("quarantine contains an unexpected entry")
		}
	}
	return nil
}

func (manager Manager) now() time.Time {
	if manager.Now != nil {
		return manager.Now().UTC()
	}
	return time.Now().UTC()
}

func (manager Manager) exchange() func(string, string) error {
	if manager.Exchange != nil {
		return manager.Exchange
	}
	return privatefs.RenameExchange
}

func (manager Manager) device() func(string) (uint64, error) {
	if manager.Device != nil {
		return manager.Device
	}
	return deviceID
}

func (manager Manager) databaseContract(ctx context.Context) (DatabaseContract, error) {
	if manager.Database == nil {
		return DatabaseContract{}, fmt.Errorf("database contract validator is unavailable")
	}
	return manager.Database(ctx, filepath.Join(manager.DataDir, "db.sqlite3"))
}
