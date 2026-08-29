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
	"github.com/hellodword/vaultwarden-less/internal/deployment"
	"github.com/hellodword/vaultwarden-less/internal/privatefs"
)

type EvidenceCollector func(context.Context, string, []string) ([]TargetEvidence, error)
type DrillCleaner func(context.Context, string, string) error

type Manager struct {
	StateDir      string
	DataDir       string
	RestoreRoot   string
	Deployment    config.Deployment
	ProjectCommit string
	Release       string
	Now           func() time.Time
	Exchange      func(string, string) error
	Device        func(string) (uint64, error)
	Database      func(context.Context, string) (DatabaseContract, error)
	PrepareFreeze func(context.Context, string) error
	Activate      func(deployment.Identity, deployment.Identity, time.Time) error
	RemoveDrill   DrillCleaner
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
		if err := cleanupCurrentWriteTransients(state.current); err != nil {
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
		for _, path := range []string{state.ticket, state.candidate, state.decision, state.rollback} {
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
	if err := requireDecisionAbsent(state); err != nil {
		return Result{}, err
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
	if err := requireDecisionAbsent(state); err != nil {
		return Result{}, err
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
	decisionExists, err := pathExists(state.decision)
	if err != nil {
		return Result{}, stateError(ErrorInvalidState, err)
	}
	var decision Decision
	if decisionExists {
		decision, err = readDecision(state, prepare)
		if err != nil {
			return Result{}, err
		}
		if decision.Decision != DecisionAccept {
			return Result{}, stateError(ErrorConflict, fmt.Errorf("rollback is already the terminal decision"))
		}
	}
	if err := manager.validateFrozenTree(ctx, state.rollback, ticket); err != nil {
		return Result{}, err
	}
	candidate, err := readCandidate(state, prepare)
	if err != nil {
		return Result{}, err
	}
	if candidate.Phase != PhaseVerified {
		return Result{}, stateError(ErrorConflict, fmt.Errorf("candidate is not verified"))
	}
	targetNames := make([]string, len(ticket.Targets))
	for index := range ticket.Targets {
		targetNames[index] = ticket.Targets[index].Target
	}
	if err := validateEvidence(candidate.Targets, targetNames); err != nil {
		return Result{}, stateError(ErrorInvalidState, fmt.Errorf("candidate targets do not match frozen evidence"))
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
	if !decisionExists {
		active, err := deployment.RequireActive(manager.StateDir, deployment.Identity{
			ProjectCommit: prepare.FromCommit, Release: prepare.FromRelease,
		})
		if err != nil {
			return Result{}, deploymentUpgradeError(err)
		}
		if drillCheck == nil {
			return Result{}, stateError(ErrorVerification, fmt.Errorf("drill verification is unavailable"))
		}
		if err := drillCheck(ctx); err != nil {
			return Result{}, stateError(ErrorVerification, err)
		}
		decidedAt, err := decisionTimeAfter(active, manager.now())
		if err != nil {
			return Result{}, err
		}
		decision = Decision{
			SchemaVersion:       SchemaVersion,
			UpgradeID:           prepare.UpgradeID,
			Decision:            DecisionAccept,
			ActiveProjectCommit: prepare.ToCommit,
			ActiveRelease:       prepare.ToRelease,
			DecidedAt:           formatTimestamp(decidedAt),
		}
		if err := decision.validate(prepare); err != nil {
			return Result{}, stateError(ErrorInvalidState, err)
		}
		if err := writeNewJSON(state.decision, decision); err != nil {
			return Result{}, stateError(ErrorConflict, err)
		}
	}
	decidedAt, err := time.Parse(timestampLayout, decision.DecidedAt)
	if err != nil {
		return Result{}, stateError(ErrorInvalidState, fmt.Errorf("accept decision timestamp is invalid"))
	}
	if err := manager.activate(
		deployment.Identity{ProjectCommit: prepare.FromCommit, Release: prepare.FromRelease},
		deployment.Identity{ProjectCommit: prepare.ToCommit, Release: prepare.ToRelease},
		decidedAt,
	); err != nil {
		return Result{}, deploymentUpgradeError(err)
	}
	return result(PhaseAccepted, prepare, nil), nil
}

func (manager Manager) Rollback(ctx context.Context) (Result, error) {
	state, prepare, ticket, err := manager.frozenForRole(false)
	if err != nil {
		return Result{}, err
	}
	decisionExists, err := pathExists(state.decision)
	if err != nil {
		return Result{}, stateError(ErrorInvalidState, err)
	}
	var decision Decision
	if decisionExists {
		decision, err = readDecision(state, prepare)
		if err != nil {
			return Result{}, err
		}
		if decision.Decision != DecisionRollback {
			return Result{}, stateError(ErrorConflict, fmt.Errorf("accept is already the terminal decision"))
		}
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
	if _, err := validateOpaqueCandidate(state.candidate); err != nil {
		return Result{}, stateError(ErrorInvalidState, err)
	}
	activeIdentity := deployment.Identity{ProjectCommit: prepare.FromCommit, Release: prepare.FromRelease}
	active, err := deployment.RequireActive(manager.StateDir, activeIdentity)
	if err != nil {
		return Result{}, deploymentUpgradeError(err)
	}
	activeDevice, err := manager.device()(manager.DataDir)
	if err != nil {
		return Result{}, stateError(ErrorConflict, fmt.Errorf("inspect active filesystem device: %w", err))
	}
	if activeDevice != ticket.ActiveDevice {
		return Result{}, stateError(ErrorConflict, fmt.Errorf("active filesystem device changed"))
	}
	rollbackExists, err := pathExists(state.rollback)
	if err != nil {
		return Result{}, stateError(ErrorInvalidState, err)
	}
	if !rollbackExists {
		return Result{}, stateError(ErrorInvalidState, fmt.Errorf("rollback tree is missing before finalization"))
	}
	rollbackDevice, err := manager.device()(state.rollback)
	if err != nil || rollbackDevice != ticket.RollbackDevice {
		return Result{}, stateError(ErrorConflict, fmt.Errorf("rollback filesystem device changed"))
	}
	rollbackID, rollbackErr := rollbackTreeID(ctx, state.rollback)
	if !decisionExists {
		if rollbackErr != nil || rollbackID != ticket.RollbackTreeID {
			return Result{}, stateError(ErrorVerification, fmt.Errorf("frozen A data is not in the rollback position"))
		}
		decidedAt, err := decisionTimeAfter(active, manager.now())
		if err != nil {
			return Result{}, err
		}
		decision = Decision{
			SchemaVersion:       SchemaVersion,
			UpgradeID:           prepare.UpgradeID,
			Decision:            DecisionRollback,
			ActiveProjectCommit: prepare.FromCommit,
			ActiveRelease:       prepare.FromRelease,
			DecidedAt:           formatTimestamp(decidedAt),
		}
		if err := decision.validate(prepare); err != nil {
			return Result{}, stateError(ErrorInvalidState, err)
		}
		if err := writeNewJSON(state.decision, decision); err != nil {
			return Result{}, stateError(ErrorConflict, err)
		}
	}
	exchanged := rollbackErr != nil || rollbackID != ticket.RollbackTreeID
	activeID := ""
	if exchanged {
		activeID, err = rollbackTreeID(ctx, manager.DataDir)
		if err != nil || activeID != ticket.RollbackTreeID {
			return Result{}, stateError(ErrorVerification, fmt.Errorf("neither rollback position contains frozen A data"))
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
	decidedAt, err := time.Parse(timestampLayout, decision.DecidedAt)
	if err != nil {
		return Result{}, stateError(ErrorInvalidState, fmt.Errorf("rollback decision timestamp is invalid"))
	}
	if err := manager.activate(activeIdentity, activeIdentity, decidedAt); err != nil {
		return Result{}, deploymentUpgradeError(err)
	}
	return result(PhaseRolledBack, prepare, nil), nil
}

func (manager Manager) Finalize(ctx context.Context) (Result, error) {
	if err := manager.validate(); err != nil {
		return Result{}, err
	}
	state, err := validateStateLayout(manager.StateDir)
	if err != nil {
		return Result{}, err
	}
	if err := cleanupCurrentWriteTransients(state.current); err != nil {
		return Result{}, stateError(ErrorInvalidState, err)
	}
	if err := validateCurrentEntries(state.current); err != nil {
		return Result{}, stateError(ErrorInvalidState, err)
	}
	prepare, err := readPrepare(state, manager.Deployment)
	if err != nil {
		return Result{}, err
	}
	ticket, err := readTicket(state, prepare)
	if err != nil {
		return Result{}, err
	}
	decision, err := readDecision(state, prepare)
	if err != nil {
		return Result{}, err
	}
	target := deployment.Identity{ProjectCommit: decision.ActiveProjectCommit, Release: decision.ActiveRelease}
	if manager.ProjectCommit != target.ProjectCommit || manager.Release != target.Release {
		return Result{}, stateError(ErrorConflict, fmt.Errorf("finalize is owned by the terminal release"))
	}
	active, err := deployment.RequireActive(manager.StateDir, target)
	if err != nil {
		return Result{}, deploymentUpgradeError(err)
	}
	if err := requireDecisionActivation(active, decision); err != nil {
		return Result{}, err
	}
	if err := manager.requireDrillRemoved(); err != nil {
		return Result{}, err
	}

	historyPath := filepath.Join(state.history, prepare.UpgradeID)
	quarantinePath := filepath.Join(state.quarantine, prepare.UpgradeID)
	for _, destination := range []string{historyPath, quarantinePath} {
		if exists, err := pathExists(destination); err != nil {
			return Result{}, stateError(ErrorInvalidState, err)
		} else if exists {
			return Result{}, stateError(ErrorConflict, fmt.Errorf("upgrade archive destination already exists"))
		}
	}

	destination := historyPath
	phase := PhaseAccepted
	if decision.Decision == DecisionAccept {
		candidate, err := readCandidate(state, prepare)
		if err != nil {
			return Result{}, err
		}
		if candidate.Phase != PhaseVerified {
			return Result{}, stateError(ErrorConflict, fmt.Errorf("accepted candidate is not verified"))
		}
		if err := manager.validateFrozenTree(ctx, state.rollback, ticket); err != nil {
			return Result{}, err
		}
	} else {
		if _, err := validateOpaqueCandidate(state.candidate); err != nil {
			return Result{}, stateError(ErrorInvalidState, err)
		}
		activeDevice, err := manager.device()(manager.DataDir)
		if err != nil || activeDevice != ticket.ActiveDevice {
			return Result{}, stateError(ErrorConflict, fmt.Errorf("active filesystem device changed"))
		}
		rollbackDevice, err := manager.device()(state.rollback)
		if err != nil || rollbackDevice != ticket.RollbackDevice {
			return Result{}, stateError(ErrorConflict, fmt.Errorf("rollback filesystem device changed"))
		}
		destination = quarantinePath
		phase = PhaseRolledBack
	}
	if err := privatefs.RenameNoReplace(state.current, destination); err != nil {
		return Result{}, stateError(ErrorConflict, err)
	}
	if err := privatefs.SyncDirectory(filepath.Dir(destination)); err != nil {
		return Result{}, stateError(ErrorInvalidState, err)
	}
	if err := privatefs.SyncDirectory(state.upgrade); err != nil {
		return Result{}, stateError(ErrorInvalidState, err)
	}
	return result(phase, prepare, nil), nil
}

func (manager Manager) CleanupDrill(ctx context.Context) (Result, error) {
	if err := manager.validate(); err != nil {
		return Result{}, err
	}
	state, err := validateStateLayout(manager.StateDir)
	if err != nil {
		return Result{}, err
	}
	if err := cleanupCurrentWriteTransients(state.current); err != nil {
		return Result{}, stateError(ErrorInvalidState, err)
	}
	if err := validateCurrentEntries(state.current); err != nil {
		return Result{}, stateError(ErrorInvalidState, err)
	}
	prepare, err := readPrepare(state, manager.Deployment)
	if err != nil {
		return Result{}, err
	}
	decision, err := readDecision(state, prepare)
	if err != nil {
		return Result{}, err
	}
	target := deployment.Identity{ProjectCommit: decision.ActiveProjectCommit, Release: decision.ActiveRelease}
	if manager.ProjectCommit != target.ProjectCommit || manager.Release != target.Release {
		return Result{}, stateError(ErrorConflict, fmt.Errorf("drill cleanup is owned by the terminal release"))
	}
	if _, err := deployment.RequireActive(manager.StateDir, target); err != nil {
		return Result{}, deploymentUpgradeError(err)
	}
	if manager.RestoreRoot == "" || !filepath.IsAbs(manager.RestoreRoot) || filepath.Clean(manager.RestoreRoot) != manager.RestoreRoot {
		return Result{}, stateError(ErrorInvalidState, fmt.Errorf("restore root is invalid"))
	}
	if err := privatefs.ValidatePrivateDirectory(manager.RestoreRoot); err != nil {
		return Result{}, stateError(ErrorInvalidState, fmt.Errorf("validate restore root: %w", err))
	}
	drill := filepath.Join(manager.RestoreRoot, "drill")
	exists, err := pathExists(drill)
	if err != nil {
		return Result{}, stateError(ErrorInvalidState, err)
	}
	if exists {
		if manager.RemoveDrill == nil {
			return Result{}, stateError(ErrorVerification, fmt.Errorf("drill cleanup is unavailable"))
		}
		if err := manager.RemoveDrill(ctx, drill, prepare.ToCommit); err != nil {
			return Result{}, stateError(ErrorVerification, err)
		}
	}
	phase := PhaseAccepted
	if decision.Decision == DecisionRollback {
		phase = PhaseRolledBack
	}
	return result(phase, prepare, nil), nil
}

func ServingAllowed(
	ctx context.Context,
	stateDir, dataDir string,
	identity deployment.Identity,
	configured config.Deployment,
) error {
	return servingAllowed(ctx, stateDir, dataDir, identity, configured, true)
}

// StateOnlyServingAllowed validates a non-data-opening role without granting
// it a data mount. Roles that can open data must use ServingAllowed.
func StateOnlyServingAllowed(
	ctx context.Context,
	stateDir string,
	identity deployment.Identity,
	configured config.Deployment,
) error {
	return servingAllowed(ctx, stateDir, config.DataDir, identity, configured, false)
}

func servingAllowed(
	ctx context.Context,
	stateDir, dataDir string,
	identity deployment.Identity,
	configured config.Deployment,
	validateActiveData bool,
) error {
	if err := identity.Validate(); err != nil {
		return stateError(ErrorInvalidState, fmt.Errorf("serving identity is invalid"))
	}
	if _, err := deployment.RequireActive(stateDir, identity); err != nil {
		return deploymentUpgradeError(err)
	}
	state := paths(stateDir)
	currentExists, err := pathExists(state.current)
	if err != nil {
		return stateError(ErrorInvalidState, err)
	}
	if !currentExists {
		return nil
	}
	state, err = validateStateLayout(stateDir)
	if err != nil {
		return err
	}
	if err := validateCurrentEntries(state.current); err != nil {
		return stateError(ErrorInvalidState, err)
	}
	prepare, err := readPrepare(state, configured)
	if err != nil {
		return err
	}
	ticketExists, err := pathExists(state.ticket)
	if err != nil {
		return stateError(ErrorInvalidState, err)
	}
	if !ticketExists {
		for _, path := range []string{state.candidate, state.decision, state.rollback} {
			exists, pathErr := pathExists(path)
			if pathErr != nil {
				return stateError(ErrorInvalidState, pathErr)
			}
			if exists {
				return stateError(ErrorInvalidState, fmt.Errorf("prepared upgrade contains later-phase state"))
			}
		}
		if identity.ProjectCommit != prepare.FromCommit || identity.Release != prepare.FromRelease {
			return stateError(ErrorConflict, fmt.Errorf("prepared upgrade is owned by active A"))
		}
		return nil
	}
	return terminalServingAllowed(ctx, stateDir, dataDir, identity, configured, "", validateActiveData)
}

func RollbackServingAllowed(
	ctx context.Context,
	stateDir, dataDir string,
	identity deployment.Identity,
	configured config.Deployment,
) error {
	return terminalServingAllowed(ctx, stateDir, dataDir, identity, configured, DecisionRollback, true)
}

func terminalServingAllowed(
	ctx context.Context,
	stateDir, dataDir string,
	identity deployment.Identity,
	configured config.Deployment,
	requiredDecision string,
	validateActiveData bool,
) error {
	if err := identity.Validate(); err != nil {
		return stateError(ErrorInvalidState, fmt.Errorf("serving identity is invalid"))
	}
	active, err := deployment.RequireActive(stateDir, identity)
	if err != nil {
		return deploymentUpgradeError(err)
	}
	manager := Manager{
		StateDir: stateDir, DataDir: dataDir, Deployment: configured,
		ProjectCommit: identity.ProjectCommit, Release: identity.Release,
	}
	if err := manager.validate(); err != nil {
		return err
	}
	state, err := validateStateLayout(stateDir)
	if err != nil {
		return err
	}
	if err := validateCurrentEntries(state.current); err != nil {
		return stateError(ErrorInvalidState, err)
	}
	prepare, err := readPrepare(state, configured)
	if err != nil {
		return err
	}
	ticket, err := readTicket(state, prepare)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return stateError(ErrorConflict, fmt.Errorf("upgrade is not terminal"))
		}
		return err
	}
	decision, err := readDecision(state, prepare)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return stateError(ErrorConflict, fmt.Errorf("upgrade has no terminal decision"))
		}
		return err
	}
	if requiredDecision != "" && decision.Decision != requiredDecision {
		return stateError(ErrorConflict, fmt.Errorf("startup mode does not match the terminal decision"))
	}
	if identity.ProjectCommit != decision.ActiveProjectCommit || identity.Release != decision.ActiveRelease {
		return stateError(ErrorConflict, fmt.Errorf("serving identity does not match the terminal decision"))
	}
	if err := requireDecisionActivation(active, decision); err != nil {
		return err
	}
	if decision.Decision == DecisionAccept {
		candidate, err := readCandidate(state, prepare)
		if err != nil {
			return err
		}
		if candidate.Phase != PhaseVerified {
			return stateError(ErrorConflict, fmt.Errorf("accepted candidate is not verified"))
		}
		var validationErr error
		if validateActiveData {
			validationErr = manager.validateFrozenTree(ctx, state.rollback, ticket)
		} else {
			validationErr = manager.validateFrozenRollback(ctx, state.rollback, ticket)
		}
		if validationErr != nil {
			return validationErr
		}
		return nil
	}
	if _, err := validateOpaqueCandidate(state.candidate); err != nil {
		return stateError(ErrorInvalidState, err)
	}
	rollbackDevice, err := manager.device()(state.rollback)
	if err != nil || rollbackDevice != ticket.RollbackDevice {
		return stateError(ErrorConflict, fmt.Errorf("rollback filesystem device changed"))
	}
	return nil
}

// CandidateServingAllowed permits only immutable B while active identity still
// names A and the current upgrade has durable frozen rollback evidence. It is
// intentionally narrower than the normal serving gate.
func CandidateServingAllowed(
	stateDir, projectCommit, release, activeProjectCommit, activeRelease string,
	deployment config.Deployment,
) error {
	if !ValidCommit(projectCommit) || !ValidRelease(release, projectCommit) ||
		!ValidCommit(activeProjectCommit) || !ValidRelease(activeRelease, activeProjectCommit) {
		return stateError(ErrorInvalidState, fmt.Errorf("candidate serving identity is invalid"))
	}
	state, err := validateStateLayout(stateDir)
	if err != nil {
		return err
	}
	if err := validateCurrentEntries(state.current); err != nil {
		return stateError(ErrorInvalidState, err)
	}
	prepare, err := readPrepare(state, deployment)
	if err != nil {
		return err
	}
	if projectCommit != prepare.ToCommit || release != prepare.ToRelease ||
		activeProjectCommit != prepare.FromCommit || activeRelease != prepare.FromRelease {
		return stateError(ErrorConflict, fmt.Errorf("candidate and active identities do not match current upgrade"))
	}
	if _, err := readTicket(state, prepare); err != nil {
		return err
	}
	if err := requireDecisionAbsent(state); err != nil {
		return err
	}
	rollbackExists, err := pathExists(state.rollback)
	if err != nil || !rollbackExists {
		return stateError(ErrorInvalidState, fmt.Errorf("frozen rollback data is missing"))
	}
	candidateExists, err := pathExists(state.candidate)
	if err != nil {
		return stateError(ErrorInvalidState, err)
	}
	if candidateExists {
		candidate, readErr := readCandidate(state, prepare)
		if readErr != nil {
			return readErr
		}
		if candidate.Phase != PhaseMigrated && candidate.Phase != PhaseVerified {
			return stateError(ErrorConflict, fmt.Errorf("candidate phase is terminal"))
		}
	}
	return nil
}

// DrillServingAllowed accepts an ordinary verified restore when no upgrade is
// current. During an upgrade it additionally requires exact B verification
// evidence for the restored content and archive identities.
func DrillServingAllowed(
	stateDir, projectCommit, release, contentID, archiveID string,
	deployment config.Deployment,
) error {
	if !ValidCommit(projectCommit) || !ValidRelease(release, projectCommit) || !validDigest(contentID) || !validDigest(archiveID) {
		return stateError(ErrorInvalidState, fmt.Errorf("drill identity is invalid"))
	}
	state := paths(stateDir)
	currentExists, err := pathExists(state.current)
	if err != nil {
		return stateError(ErrorInvalidState, err)
	}
	if !currentExists {
		return nil
	}
	state, err = validateStateLayout(stateDir)
	if err != nil {
		return err
	}
	if err := validateCurrentEntries(state.current); err != nil {
		return stateError(ErrorInvalidState, err)
	}
	prepare, err := readPrepare(state, deployment)
	if err != nil {
		return err
	}
	if projectCommit != prepare.ToCommit || release != prepare.ToRelease {
		return stateError(ErrorConflict, fmt.Errorf("upgrade drill is not owned by candidate B"))
	}
	if _, err := readTicket(state, prepare); err != nil {
		return err
	}
	if err := requireDecisionAbsent(state); err != nil {
		return err
	}
	candidate, err := readCandidate(state, prepare)
	if err != nil {
		return err
	}
	if candidate.Phase != PhaseVerified {
		return stateError(ErrorConflict, fmt.Errorf("upgrade drill lacks verified evidence"))
	}
	for _, evidence := range candidate.Targets {
		if evidence.ContentID == contentID && evidence.ArchiveID == archiveID {
			return nil
		}
	}
	return stateError(ErrorVerification, fmt.Errorf("upgrade drill identity does not match verification evidence"))
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
	if err := cleanupCurrentWriteTransients(state.current); err != nil {
		return state, Prepare{}, stateError(ErrorInvalidState, err)
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
	return manager.validateFrozenRollback(ctx, rollback, ticket)
}

func (manager Manager) validateFrozenRollback(ctx context.Context, rollback string, ticket RollbackTicket) error {
	rollbackDevice, err := manager.device()(rollback)
	if err != nil || rollbackDevice != ticket.RollbackDevice {
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

func requireDecisionAbsent(state statePaths) error {
	exists, err := pathExists(state.decision)
	if err != nil {
		return stateError(ErrorInvalidState, err)
	}
	if exists {
		return stateError(ErrorConflict, fmt.Errorf("upgrade already has a terminal decision"))
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

func (manager Manager) now() time.Time {
	if manager.Now != nil {
		return manager.Now().UTC()
	}
	return time.Now().UTC()
}

func (manager Manager) activate(expected, target deployment.Identity, notBefore time.Time) error {
	if manager.Activate != nil {
		return manager.Activate(expected, target, notBefore)
	}
	deploymentManager := deployment.Manager{
		StateDir: manager.StateDir, DataDir: manager.DataDir,
		ProjectCommit: target.ProjectCommit, Release: target.Release,
		Now: manager.Now,
	}
	if notBefore.IsZero() {
		_, err := deploymentManager.Activate(expected)
		return err
	}
	_, err := deploymentManager.ActivateAtOrAfter(expected, notBefore)
	return err
}

func requireDecisionActivation(active deployment.Active, decision Decision) error {
	activatedAt, activeErr := time.Parse(timestampLayout, active.ActivatedAt)
	decidedAt, decisionErr := time.Parse(timestampLayout, decision.DecidedAt)
	if activeErr != nil || decisionErr != nil || activatedAt.Before(decidedAt) {
		return stateError(ErrorConflict, fmt.Errorf("active identity predates the terminal data transition"))
	}
	return nil
}

func decisionTimeAfter(active deployment.Active, now time.Time) (time.Time, error) {
	activatedAt, err := time.Parse(timestampLayout, active.ActivatedAt)
	if err != nil {
		return time.Time{}, stateError(ErrorInvalidState, fmt.Errorf("active timestamp is invalid"))
	}
	decidedAt := now.UTC().Truncate(time.Millisecond)
	if !decidedAt.After(activatedAt) {
		decidedAt = activatedAt.Add(time.Millisecond)
	}
	return decidedAt, nil
}

func (manager Manager) requireDrillRemoved() error {
	if manager.RestoreRoot == "" || !filepath.IsAbs(manager.RestoreRoot) || filepath.Clean(manager.RestoreRoot) != manager.RestoreRoot {
		return stateError(ErrorInvalidState, fmt.Errorf("restore root is invalid"))
	}
	if err := privatefs.ValidatePrivateDirectory(manager.RestoreRoot); err != nil {
		return stateError(ErrorInvalidState, fmt.Errorf("validate restore root: %w", err))
	}
	exists, err := pathExists(filepath.Join(manager.RestoreRoot, "drill"))
	if err != nil {
		return stateError(ErrorInvalidState, err)
	}
	if exists {
		return stateError(ErrorConflict, fmt.Errorf("verified drill cleanup is incomplete"))
	}
	return nil
}

func deploymentUpgradeError(err error) error {
	if deployment.IsKind(err, deployment.ErrorConflict) {
		return stateError(ErrorConflict, err)
	}
	return stateError(ErrorInvalidState, err)
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
