package importer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/hellodword/vaultwarden-less/internal/config"
	"github.com/hellodword/vaultwarden-less/internal/deployment"
	"github.com/hellodword/vaultwarden-less/internal/privatefs"
)

type CandidateVerifier func(context.Context, string, string, string) (CandidateEvidence, error)

type Manager struct {
	RootDir       string
	StateDir      string
	DataDir       string
	ImportRoot    string
	ProjectCommit string
	Release       string
	Now           func() time.Time

	Exchange        func(string, string) error
	Device          func(string) (uint64, error)
	OnlineBackup    func(context.Context, string, string) error
	VerifyCandidate CandidateVerifier
	Activate        func(deployment.Identity, time.Time) error
}

func (manager Manager) Prepare(ctx context.Context) (Result, error) {
	if err := manager.validate(); err != nil {
		return Result{}, err
	}
	state, err := ensureStateLayout(manager.StateDir)
	if err != nil {
		return Result{}, err
	}
	currentExists, err := pathExists(state.current)
	if err != nil {
		return Result{}, stateError(ErrorInvalidState, err)
	}
	if currentExists {
		return manager.resumePrepare(ctx)
	}
	if historyExists, err := pathExists(state.historyEntry); err != nil {
		return Result{}, stateError(ErrorInvalidState, err)
	} else if historyExists {
		return Result{}, stateError(ErrorConflict, fmt.Errorf("initial import already completed"))
	}
	if err := manager.requireInitialPreconditions(); err != nil {
		return Result{}, err
	}
	if err := manager.cleanupCandidateTransient(); err != nil {
		return Result{}, stateError(ErrorInvalidState, err)
	}
	if err := manager.cleanupVerificationTransient(); err != nil {
		return Result{}, stateError(ErrorInvalidState, err)
	}
	if err := cleanupCurrentTransient(state); err != nil {
		return Result{}, stateError(ErrorInvalidState, err)
	}
	candidatePath := manager.candidatePath()
	if exists, err := pathExists(candidatePath); err != nil {
		return Result{}, stateError(ErrorInvalidState, err)
	} else if exists {
		return Result{}, stateError(ErrorConflict, fmt.Errorf("import candidate already exists without current state"))
	}

	sourceTreeID, err := treeID(ctx, manager.sourcePath())
	if err != nil {
		return Result{}, stateError(ErrorSourceInconsistent, err)
	}
	sourceDevice, err := manager.device()(manager.sourcePath())
	if err != nil {
		return Result{}, stateError(ErrorSourceInconsistent, err)
	}
	sourceRSAKeyID, err := fileID(ctx, manager.sourcePath(), "rsa_key.pem")
	if err != nil {
		return Result{}, stateError(ErrorSourceInconsistent, err)
	}
	temporary := manager.candidateTransientPath()
	if err := os.Mkdir(temporary, 0o700); err != nil {
		return Result{}, stateError(ErrorInvalidState, err)
	}
	if err := os.Chmod(temporary, 0o700); err != nil {
		_ = os.RemoveAll(temporary)
		return Result{}, stateError(ErrorInvalidState, err)
	}
	statePublished := false
	defer func() {
		if !statePublished {
			_ = os.RemoveAll(temporary)
		}
	}()
	if err := buildCandidate(ctx, manager.sourcePath(), temporary, manager.onlineBackup()); err != nil {
		return Result{}, stateError(ErrorSourceInconsistent, err)
	}
	candidateTreeID, err := treeID(ctx, temporary)
	if err != nil {
		return Result{}, stateError(ErrorVerification, err)
	}
	candidateDevice, err := manager.device()(temporary)
	if err != nil {
		return Result{}, stateError(ErrorInvalidState, err)
	}
	afterSourceTreeID, err := treeID(ctx, manager.sourcePath())
	if err != nil {
		return Result{}, stateError(ErrorSourceInconsistent, err)
	}
	afterSourceDevice, err := manager.device()(manager.sourcePath())
	if err != nil || afterSourceTreeID != sourceTreeID || afterSourceDevice != sourceDevice {
		return Result{}, stateError(ErrorSourceInconsistent, fmt.Errorf("import source changed while preparing candidate"))
	}
	afterRSAKeyID, err := fileID(ctx, manager.sourcePath(), "rsa_key.pem")
	if err != nil || afterRSAKeyID != sourceRSAKeyID {
		return Result{}, stateError(ErrorSourceInconsistent, fmt.Errorf("import source RSA identity changed"))
	}
	dataDevice, err := manager.device()(manager.DataDir)
	if err != nil || dataDevice == 0 || sourceDevice != dataDevice || candidateDevice != dataDevice {
		return Result{}, stateError(ErrorConflict, fmt.Errorf("import paths are on different filesystems"))
	}
	if err := manager.verifyExchangeSupport(); err != nil {
		return Result{}, stateError(ErrorConflict, err)
	}
	prepare := Prepare{
		ImportID:         ImportIDInitial,
		Phase:            PhasePrepared,
		ProjectCommit:    manager.ProjectCommit,
		Release:          manager.Release,
		SourcePath:       sourceRelative,
		CandidatePath:    candidateRelative,
		DataPath:         dataRelative,
		FilesystemDevice: dataDevice,
		SourceTreeID:     sourceTreeID,
		CandidateTreeID:  candidateTreeID,
		SourceRSAKeyID:   sourceRSAKeyID,
		PreparedAt:       formatTimestamp(manager.now()),
	}
	if err := prepare.validate(); err != nil {
		return Result{}, stateError(ErrorInvalidState, err)
	}
	currentTemporary := state.transient
	if err := os.Mkdir(currentTemporary, 0o700); err != nil {
		return Result{}, stateError(ErrorInvalidState, err)
	}
	currentPublished := false
	defer func() {
		if !currentPublished {
			_ = os.RemoveAll(currentTemporary)
		}
	}()
	if err := os.Chmod(currentTemporary, 0o700); err != nil {
		return Result{}, stateError(ErrorInvalidState, err)
	}
	if err := writeNewJSON(filepath.Join(currentTemporary, prepareName), prepare); err != nil {
		return Result{}, stateError(ErrorInvalidState, err)
	}
	if err := privatefs.SyncDirectory(currentTemporary); err != nil {
		return Result{}, stateError(ErrorInvalidState, err)
	}
	if err := privatefs.RenameNoReplace(currentTemporary, state.current); err != nil {
		return Result{}, stateError(ErrorConflict, err)
	}
	currentPublished = true
	statePublished = true
	if err := privatefs.SyncDirectory(state.importRoot); err != nil {
		return Result{}, stateError(ErrorInvalidState, err)
	}
	if err := privatefs.RenameNoReplace(temporary, candidatePath); err != nil {
		return Result{}, stateError(ErrorInvalidState, fmt.Errorf("publish import candidate: %w", err))
	}
	if err := privatefs.SyncDirectory(manager.ImportRoot); err != nil {
		return Result{}, stateError(ErrorInvalidState, err)
	}
	return result(PhasePrepared, prepare, candidateTreeID, false), nil
}

func (manager Manager) resumePrepare(ctx context.Context) (Result, error) {
	state, prepare, err := manager.current()
	if err != nil {
		return Result{}, err
	}
	if exists, err := pathExists(state.verification); err != nil {
		return Result{}, stateError(ErrorInvalidState, err)
	} else if exists {
		return Result{}, stateError(ErrorConflict, fmt.Errorf("import verification already exists"))
	}
	if exists, err := pathExists(state.decision); err != nil {
		return Result{}, stateError(ErrorInvalidState, err)
	} else if exists {
		return Result{}, stateError(ErrorConflict, fmt.Errorf("import decision already exists"))
	}
	if err := manager.requireInitialPreconditions(); err != nil {
		return Result{}, err
	}
	if err := manager.requireSource(ctx, prepare); err != nil {
		return Result{}, err
	}
	if err := manager.requireDevice(prepare.FilesystemDevice); err != nil {
		return Result{}, err
	}
	candidate := manager.candidatePath()
	if exists, err := pathExists(candidate); err != nil {
		return Result{}, stateError(ErrorInvalidState, err)
	} else if exists {
		identity, err := treeID(ctx, candidate)
		if err != nil || identity != prepare.CandidateTreeID {
			return Result{}, stateError(ErrorVerification, fmt.Errorf("prepared import candidate changed"))
		}
		return result(PhasePrepared, prepare, identity, false), nil
	}
	matching, err := manager.findMatchingTransient(ctx, prepare.CandidateTreeID)
	if err != nil {
		return Result{}, stateError(ErrorVerification, err)
	}
	if matching == "" {
		matching = manager.candidateTransientPath()
		if err := os.Mkdir(matching, 0o700); err != nil {
			return Result{}, stateError(ErrorInvalidState, err)
		}
		if err := os.Chmod(matching, 0o700); err != nil {
			_ = os.RemoveAll(matching)
			return Result{}, stateError(ErrorInvalidState, err)
		}
		if err := buildCandidate(ctx, manager.sourcePath(), matching, manager.onlineBackup()); err != nil {
			_ = os.RemoveAll(matching)
			return Result{}, stateError(ErrorSourceInconsistent, err)
		}
		identity, identityErr := treeID(ctx, matching)
		if identityErr != nil || identity != prepare.CandidateTreeID {
			_ = os.RemoveAll(matching)
			return Result{}, stateError(ErrorVerification, fmt.Errorf("resumed candidate differs from immutable preparation"))
		}
	}
	if err := privatefs.RenameNoReplace(matching, candidate); err != nil {
		return Result{}, stateError(ErrorInvalidState, err)
	}
	if err := privatefs.SyncDirectory(manager.ImportRoot); err != nil {
		return Result{}, stateError(ErrorInvalidState, err)
	}
	return result(PhasePrepared, prepare, prepare.CandidateTreeID, false), nil
}

func (manager Manager) Verify(ctx context.Context, confirmClientLogin bool) (Result, error) {
	state, prepare, err := manager.current()
	if err != nil {
		return Result{}, err
	}
	if !confirmClientLogin {
		return Result{}, stateError(ErrorVerification, fmt.Errorf("real client login confirmation is required"))
	}
	if exists, err := pathExists(state.decision); err != nil {
		return Result{}, stateError(ErrorInvalidState, err)
	} else if exists {
		return Result{}, stateError(ErrorConflict, fmt.Errorf("decided import cannot be verified again"))
	}
	if err := manager.requireSource(ctx, prepare); err != nil {
		return Result{}, err
	}
	if exists, err := pathExists(state.verification); err != nil {
		return Result{}, stateError(ErrorInvalidState, err)
	} else if exists {
		verification, err := readVerification(state, prepare)
		if err != nil {
			return Result{}, err
		}
		candidateTreeID, err := treeID(ctx, manager.candidatePath())
		if err != nil || candidateTreeID != verification.CandidateTreeID {
			return Result{}, stateError(ErrorVerification, fmt.Errorf("verified candidate changed"))
		}
		if err := manager.requireRotatedRSA(ctx, prepare); err != nil {
			return Result{}, err
		}
		return result(PhaseVerified, prepare, candidateTreeID, true), nil
	}
	if err := normalizePrivateTree(manager.candidatePath()); err != nil {
		return Result{}, stateError(ErrorVerification, err)
	}
	if err := manager.requireRotatedRSA(ctx, prepare); err != nil {
		return Result{}, err
	}
	verifier := manager.candidateVerifier()
	if verifier == nil {
		return Result{}, stateError(ErrorVerification, fmt.Errorf("candidate verification is unavailable"))
	}
	staging := manager.verificationTransientPath()
	if err := os.Mkdir(staging, 0o700); err != nil {
		return Result{}, stateError(ErrorVerification, err)
	}
	if err := os.Chmod(staging, 0o700); err != nil {
		_ = manager.cleanupVerificationTransient()
		return Result{}, stateError(ErrorVerification, err)
	}
	evidence, verifyErr := verifier(ctx, manager.candidatePath(), staging, manager.ProjectCommit)
	cleanupErr := manager.cleanupVerificationTransient()
	if verifyErr != nil {
		return Result{}, stateError(ErrorVerification, verifyErr)
	}
	if cleanupErr != nil {
		return Result{}, stateError(ErrorVerification, cleanupErr)
	}
	if err := evidence.validate(); err != nil {
		return Result{}, stateError(ErrorVerification, err)
	}
	if err := normalizePrivateTree(manager.candidatePath()); err != nil {
		return Result{}, stateError(ErrorVerification, err)
	}
	candidateTreeID, err := treeID(ctx, manager.candidatePath())
	if err != nil {
		return Result{}, stateError(ErrorVerification, err)
	}
	if err := manager.requireSource(ctx, prepare); err != nil {
		return Result{}, err
	}
	verification := Verification{
		ImportID:                  ImportIDInitial,
		Phase:                     PhaseVerified,
		ProjectCommit:             prepare.ProjectCommit,
		Release:                   prepare.Release,
		SourceTreeID:              prepare.SourceTreeID,
		CandidateTreeID:           candidateTreeID,
		DatabaseMigration:         evidence.DatabaseMigration,
		DatabaseSchemaFingerprint: evidence.DatabaseSchemaFingerprint,
		ContentID:                 evidence.ContentID,
		ArchiveID:                 evidence.ArchiveID,
		ClientLoginConfirmed:      true,
		VerifiedAt:                formatTimestamp(manager.now()),
	}
	if err := verification.validate(prepare); err != nil {
		return Result{}, stateError(ErrorInvalidState, err)
	}
	if err := writeNewJSON(state.verification, verification); err != nil {
		return Result{}, stateError(ErrorInvalidState, err)
	}
	return result(PhaseVerified, prepare, candidateTreeID, true), nil
}

func (manager Manager) Accept(ctx context.Context) (Result, error) {
	state, prepare, err := manager.currentOrCompleted()
	if err != nil {
		return Result{}, err
	}
	if state.current == state.historyEntry {
		verification, err := readVerification(state, prepare)
		if err != nil {
			return Result{}, err
		}
		return result(PhaseAccepted, prepare, verification.CandidateTreeID, true), nil
	}
	verification, err := readVerification(state, prepare)
	if err != nil {
		return Result{}, err
	}
	if err := manager.requireSource(ctx, prepare); err != nil {
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
	} else {
		if err := manager.requireActiveAbsent(); err != nil {
			return Result{}, err
		}
		if err := manager.requirePreExchangeTrees(ctx, prepare, verification); err != nil {
			return Result{}, err
		}
		decision = Decision{
			ImportID:            ImportIDInitial,
			Decision:            DecisionAccept,
			ActiveProjectCommit: prepare.ProjectCommit,
			ActiveRelease:       prepare.Release,
			DecidedAt:           formatTimestamp(manager.now()),
		}
		if err := decision.validate(prepare); err != nil {
			return Result{}, stateError(ErrorInvalidState, err)
		}
		if err := writeNewJSON(state.decision, decision); err != nil {
			return Result{}, stateError(ErrorInvalidState, err)
		}
	}
	active, activeExists, err := readOptionalActive(manager.StateDir)
	if err != nil {
		return Result{}, err
	}
	if activeExists {
		if active.Identity() != prepare.Identity() || requireDecisionActivation(active, decision) != nil {
			return Result{}, stateError(ErrorConflict, fmt.Errorf("active identity does not prove completed import exchange"))
		}
		if err := manager.requirePostExchangeCandidate(); err != nil {
			return Result{}, err
		}
		return result(PhaseAccepted, prepare, verification.CandidateTreeID, true), nil
	}
	if err := manager.requireDevice(prepare.FilesystemDevice); err != nil {
		return Result{}, err
	}
	candidate := manager.candidatePath()
	candidateTreeID, err := treeID(ctx, candidate)
	if err != nil {
		return Result{}, stateError(ErrorVerification, err)
	}
	dataTreeID, err := treeID(ctx, manager.DataDir)
	if err != nil {
		return Result{}, stateError(ErrorVerification, err)
	}
	dataEmpty, err := emptyPrivateDirectory(manager.DataDir)
	if err != nil {
		return Result{}, stateError(ErrorInvalidState, err)
	}
	candidateEmpty, err := emptyPrivateDirectory(candidate)
	if err != nil {
		return Result{}, stateError(ErrorInvalidState, err)
	}
	switch {
	case dataEmpty && candidateTreeID == verification.CandidateTreeID:
		if err := manager.exchange()(manager.DataDir, candidate); err != nil {
			return Result{}, stateError(ErrorConflict, err)
		}
		if err := privatefs.SyncDirectory(manager.RootDir); err != nil {
			return Result{}, stateError(ErrorInvalidState, err)
		}
		if err := privatefs.SyncDirectory(manager.ImportRoot); err != nil {
			return Result{}, stateError(ErrorInvalidState, err)
		}
	case dataTreeID == verification.CandidateTreeID && candidateEmpty:
		// A prior attempt completed the exchange and stopped before publication.
	default:
		return Result{}, stateError(ErrorConflict, fmt.Errorf("import tree identities do not match either exchange boundary"))
	}
	dataTreeID, err = treeID(ctx, manager.DataDir)
	if err != nil || dataTreeID != verification.CandidateTreeID {
		return Result{}, stateError(ErrorVerification, fmt.Errorf("accepted data does not match verified candidate"))
	}
	if empty, err := emptyPrivateDirectory(candidate); err != nil || !empty {
		return Result{}, stateError(ErrorVerification, fmt.Errorf("exchanged candidate is not the exact empty data tree"))
	}
	if err := manager.requireSource(ctx, prepare); err != nil {
		return Result{}, err
	}
	decidedAt, err := time.Parse(timestampLayout, decision.DecidedAt)
	if err != nil {
		return Result{}, stateError(ErrorInvalidState, err)
	}
	if err := manager.activate(prepare.Identity(), decidedAt); err != nil {
		return Result{}, err
	}
	active, activeExists, err = readOptionalActive(manager.StateDir)
	if err != nil || !activeExists || active.Identity() != prepare.Identity() || requireDecisionActivation(active, decision) != nil {
		return Result{}, stateError(ErrorConflict, fmt.Errorf("import activation was not durably published"))
	}
	return result(PhaseAccepted, prepare, verification.CandidateTreeID, true), nil
}

func (manager Manager) Cancel(ctx context.Context) (Result, error) {
	state, prepare, err := manager.current()
	if err != nil {
		return Result{}, err
	}
	if exists, err := pathExists(state.decision); err != nil {
		return Result{}, stateError(ErrorInvalidState, err)
	} else if exists {
		return Result{}, stateError(ErrorConflict, fmt.Errorf("decided import cannot be cancelled"))
	}
	if err := manager.requireInitialPreconditions(); err != nil {
		return Result{}, err
	}
	candidate := manager.candidatePath()
	if exists, err := pathExists(candidate); err != nil {
		return Result{}, stateError(ErrorInvalidState, err)
	} else if exists {
		if err := safeRemoveTree(candidate); err != nil {
			return Result{}, stateError(ErrorVerification, err)
		}
		if err := privatefs.SyncDirectory(manager.ImportRoot); err != nil {
			return Result{}, stateError(ErrorInvalidState, err)
		}
	}
	if err := manager.cleanupCandidateTransient(); err != nil {
		return Result{}, stateError(ErrorInvalidState, err)
	}
	if err := os.RemoveAll(state.current); err != nil {
		return Result{}, stateError(ErrorInvalidState, err)
	}
	if err := privatefs.SyncDirectory(state.importRoot); err != nil {
		return Result{}, stateError(ErrorInvalidState, err)
	}
	return result(PhaseCancelled, prepare, prepare.CandidateTreeID, false), nil
}

func (manager Manager) Finalize(ctx context.Context) (Result, error) {
	state, prepare, err := manager.currentOrCompleted()
	if err != nil {
		return Result{}, err
	}
	if state.current == state.historyEntry {
		verification, err := readVerification(state, prepare)
		if err != nil {
			return Result{}, err
		}
		return result(PhaseFinalized, prepare, verification.CandidateTreeID, true), nil
	}
	verification, err := readVerification(state, prepare)
	if err != nil {
		return Result{}, err
	}
	decision, err := readDecision(state, prepare)
	if err != nil {
		return Result{}, err
	}
	active, err := deployment.RequireActive(manager.StateDir, prepare.Identity())
	if err != nil || requireDecisionActivation(active, decision) != nil {
		return Result{}, stateError(ErrorConflict, fmt.Errorf("active identity does not prove completed import exchange"))
	}
	if err := manager.requireSource(ctx, prepare); err != nil {
		return Result{}, err
	}
	candidate := manager.candidatePath()
	if exists, err := pathExists(candidate); err != nil {
		return Result{}, stateError(ErrorInvalidState, err)
	} else if exists {
		empty, err := emptyPrivateDirectory(candidate)
		if err != nil || !empty {
			return Result{}, stateError(ErrorConflict, fmt.Errorf("post-exchange candidate is not empty"))
		}
		if err := os.Remove(candidate); err != nil {
			return Result{}, stateError(ErrorInvalidState, err)
		}
		if err := privatefs.SyncDirectory(manager.ImportRoot); err != nil {
			return Result{}, stateError(ErrorInvalidState, err)
		}
	}
	if exists, err := pathExists(state.historyEntry); err != nil {
		return Result{}, stateError(ErrorInvalidState, err)
	} else if exists {
		return Result{}, stateError(ErrorConflict, fmt.Errorf("import history destination already exists"))
	}
	if err := privatefs.RenameNoReplace(state.current, state.historyEntry); err != nil {
		return Result{}, stateError(ErrorConflict, err)
	}
	if err := privatefs.SyncDirectory(state.history); err != nil {
		return Result{}, stateError(ErrorInvalidState, err)
	}
	if err := privatefs.SyncDirectory(state.importRoot); err != nil {
		return Result{}, stateError(ErrorInvalidState, err)
	}
	return result(PhaseFinalized, prepare, verification.CandidateTreeID, true), nil
}

func CandidateServingAllowed(
	ctx context.Context,
	stateDir, sourceDir, candidateDir string,
	identity deployment.Identity,
	configured config.Deployment,
) error {
	if err := identity.Validate(); err != nil ||
		configured.RuntimeUID != uint32(os.Geteuid()) || configured.RuntimeGID != uint32(os.Getegid()) {
		return stateError(ErrorInvalidState, fmt.Errorf("import serving identity is invalid"))
	}
	state, err := validateStateLayout(stateDir)
	if err != nil {
		return err
	}
	prepare, err := readPrepare(state)
	if err != nil {
		return err
	}
	if prepare.Identity() != identity {
		return stateError(ErrorConflict, fmt.Errorf("import candidate belongs to another release"))
	}
	if _, exists, err := readOptionalActive(stateDir); err != nil {
		return err
	} else if exists {
		return stateError(ErrorConflict, fmt.Errorf("an active deployment cannot run initial import"))
	}
	for _, path := range []string{state.verification, state.decision} {
		if exists, err := pathExists(path); err != nil {
			return stateError(ErrorInvalidState, err)
		} else if exists {
			return stateError(ErrorConflict, fmt.Errorf("verified or decided import candidate cannot restart"))
		}
	}
	if exists, err := pathExists(filepath.Join(stateDir, "upgrade", "current")); err != nil {
		return stateError(ErrorInvalidState, err)
	} else if exists {
		return stateError(ErrorConflict, fmt.Errorf("upgrade and import cannot be current together"))
	}
	sourceTreeID, err := treeID(ctx, sourceDir)
	if err != nil || sourceTreeID != prepare.SourceTreeID {
		return stateError(ErrorSourceInconsistent, fmt.Errorf("import source changed"))
	}
	sourceDevice, err := deviceID(sourceDir)
	if err != nil || sourceDevice != prepare.FilesystemDevice {
		return stateError(ErrorSourceInconsistent, fmt.Errorf("import source filesystem changed"))
	}
	_, candidateDevice, err := inspectPrivateTree(candidateDir, true)
	if err != nil || candidateDevice != prepare.FilesystemDevice {
		return stateError(ErrorVerification, fmt.Errorf("import candidate tree is unsafe"))
	}
	if _, err := os.Lstat(filepath.Join(candidateDir, "db.sqlite3")); err != nil {
		return stateError(ErrorVerification, fmt.Errorf("import candidate database is unavailable"))
	}
	return nil
}

func NormalServingAllowed(stateDir string, identity deployment.Identity) error {
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
	prepare, err := readPrepare(state)
	if err != nil {
		return err
	}
	if prepare.Identity() != identity {
		return stateError(ErrorConflict, fmt.Errorf("active release does not own current import"))
	}
	if _, err := readVerification(state, prepare); err != nil {
		return err
	}
	decision, err := readDecision(state, prepare)
	if err != nil {
		return err
	}
	active, err := deployment.RequireActive(stateDir, identity)
	if err != nil || requireDecisionActivation(active, decision) != nil {
		return stateError(ErrorConflict, fmt.Errorf("active identity does not prove completed import exchange"))
	}
	return nil
}

func (manager Manager) current() (statePaths, Prepare, error) {
	if err := manager.validate(); err != nil {
		return statePaths{}, Prepare{}, err
	}
	if err := manager.cleanupVerificationTransient(); err != nil {
		return paths(manager.StateDir), Prepare{}, stateError(ErrorInvalidState, err)
	}
	if err := cleanupCurrentWriteTransients(paths(manager.StateDir).current); err != nil {
		return paths(manager.StateDir), Prepare{}, stateError(ErrorInvalidState, err)
	}
	state, err := validateStateLayout(manager.StateDir)
	if err != nil {
		return state, Prepare{}, err
	}
	prepare, err := readPrepare(state)
	if err != nil {
		return state, Prepare{}, err
	}
	if prepare.Identity() != (deployment.Identity{ProjectCommit: manager.ProjectCommit, Release: manager.Release}) {
		return state, Prepare{}, stateError(ErrorConflict, fmt.Errorf("import command belongs to another release"))
	}
	return state, prepare, nil
}

func (manager Manager) currentOrCompleted() (statePaths, Prepare, error) {
	state, prepare, err := manager.current()
	if err == nil {
		return state, prepare, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return state, Prepare{}, err
	}
	archived := paths(manager.StateDir)
	if privatefs.ValidatePrivateDirectory(archived.historyEntry) != nil {
		return archived, Prepare{}, err
	}
	archived.current = archived.historyEntry
	archived.prepare = filepath.Join(archived.current, prepareName)
	archived.verification = filepath.Join(archived.current, verificationName)
	archived.decision = filepath.Join(archived.current, decisionName)
	if err := validateCurrentEntries(archived.current); err != nil {
		return archived, Prepare{}, stateError(ErrorInvalidState, err)
	}
	prepare, err = readPrepare(archived)
	if err != nil || prepare.Identity() != (deployment.Identity{ProjectCommit: manager.ProjectCommit, Release: manager.Release}) {
		return archived, Prepare{}, stateError(ErrorConflict, fmt.Errorf("completed import belongs to another release"))
	}
	if _, err := readVerification(archived, prepare); err != nil {
		return archived, Prepare{}, err
	}
	decision, err := readDecision(archived, prepare)
	if err != nil {
		return archived, Prepare{}, err
	}
	active, err := deployment.RequireActive(manager.StateDir, prepare.Identity())
	if err != nil || requireDecisionActivation(active, decision) != nil {
		return archived, Prepare{}, stateError(ErrorConflict, fmt.Errorf("completed import active identity is invalid"))
	}
	return archived, prepare, nil
}

func (manager Manager) validate() error {
	identity := deployment.Identity{ProjectCommit: manager.ProjectCommit, Release: manager.Release}
	if err := identity.Validate(); err != nil {
		return stateError(ErrorInvalidState, fmt.Errorf("import manager identity is invalid"))
	}
	for _, path := range []string{manager.RootDir, manager.StateDir, manager.DataDir, manager.ImportRoot} {
		if path == "" || path == string(filepath.Separator) || !filepath.IsAbs(path) || filepath.Clean(path) != path {
			return stateError(ErrorInvalidState, fmt.Errorf("import manager paths are invalid"))
		}
	}
	if manager.StateDir != filepath.Join(manager.RootDir, "state") || manager.DataDir != filepath.Join(manager.RootDir, "data") ||
		manager.ImportRoot != filepath.Join(manager.RootDir, "import") {
		return stateError(ErrorInvalidState, fmt.Errorf("import paths do not use the fixed deployment layout"))
	}
	return nil
}

func (manager Manager) requireInitialPreconditions() error {
	if err := manager.requireActiveAbsent(); err != nil {
		return err
	}
	for _, directory := range []string{manager.DataDir, manager.ImportRoot, manager.sourcePath()} {
		if err := privatefs.ValidatePrivateDirectory(directory); err != nil {
			return stateError(ErrorInvalidState, err)
		}
	}
	empty, err := emptyPrivateDirectory(manager.DataDir)
	if err != nil {
		return stateError(ErrorInvalidState, err)
	}
	if !empty {
		return stateError(ErrorConflict, fmt.Errorf("initial import requires empty data"))
	}
	if exists, err := pathExists(filepath.Join(manager.StateDir, "upgrade", "current")); err != nil {
		return stateError(ErrorInvalidState, err)
	} else if exists {
		return stateError(ErrorConflict, fmt.Errorf("upgrade and import cannot be current together"))
	}
	return nil
}

func (manager Manager) requireActiveAbsent() error {
	_, exists, err := readOptionalActive(manager.StateDir)
	if err != nil {
		return err
	}
	if exists {
		return stateError(ErrorConflict, fmt.Errorf("initial import is unavailable after activation"))
	}
	return nil
}

func readOptionalActive(stateDir string) (deployment.Active, bool, error) {
	active, err := deployment.ReadActive(stateDir)
	if err == nil {
		return active, true, nil
	}
	if deployment.IsKind(err, deployment.ErrorNotInitialized) {
		return deployment.Active{}, false, nil
	}
	return deployment.Active{}, false, stateError(ErrorInvalidState, err)
}

func (manager Manager) requireSource(ctx context.Context, prepare Prepare) error {
	identity, err := treeID(ctx, manager.sourcePath())
	if err != nil {
		return stateError(ErrorSourceInconsistent, fmt.Errorf("import source tree changed"))
	}
	device, err := manager.device()(manager.sourcePath())
	if err != nil || identity != prepare.SourceTreeID || device != prepare.FilesystemDevice {
		return stateError(ErrorSourceInconsistent, fmt.Errorf("import source tree changed"))
	}
	rsa, err := fileID(ctx, manager.sourcePath(), "rsa_key.pem")
	if err != nil || rsa != prepare.SourceRSAKeyID {
		return stateError(ErrorSourceInconsistent, fmt.Errorf("import source RSA identity changed"))
	}
	return nil
}

func (manager Manager) requireRotatedRSA(ctx context.Context, prepare Prepare) error {
	candidateRSA, err := fileID(ctx, manager.candidatePath(), "rsa_key.pem")
	if err != nil || candidateRSA == prepare.SourceRSAKeyID {
		return stateError(ErrorVerification, fmt.Errorf("candidate RSA identity was not rotated"))
	}
	return nil
}

func (manager Manager) requireDevice(expected uint64) error {
	for _, path := range []string{manager.DataDir, manager.ImportRoot, manager.sourcePath()} {
		device, err := manager.device()(path)
		if err != nil || device != expected {
			return stateError(ErrorConflict, fmt.Errorf("import filesystem device changed"))
		}
	}
	if exists, err := pathExists(manager.candidatePath()); err != nil {
		return stateError(ErrorInvalidState, err)
	} else if exists {
		device, err := manager.device()(manager.candidatePath())
		if err != nil || device != expected {
			return stateError(ErrorConflict, fmt.Errorf("candidate filesystem device changed"))
		}
	}
	return nil
}

func (manager Manager) requirePreExchangeTrees(ctx context.Context, prepare Prepare, verification Verification) error {
	if err := manager.requireDevice(prepare.FilesystemDevice); err != nil {
		return err
	}
	empty, err := emptyPrivateDirectory(manager.DataDir)
	if err != nil || !empty {
		return stateError(ErrorConflict, fmt.Errorf("initial import data is not empty"))
	}
	identity, err := treeID(ctx, manager.candidatePath())
	if err != nil || identity != verification.CandidateTreeID {
		return stateError(ErrorVerification, fmt.Errorf("candidate changed after verification"))
	}
	return nil
}

func (manager Manager) requirePostExchangeCandidate() error {
	candidate := manager.candidatePath()
	exists, err := pathExists(candidate)
	if err != nil {
		return stateError(ErrorInvalidState, err)
	}
	if !exists {
		return nil
	}
	empty, err := emptyPrivateDirectory(candidate)
	if err != nil || !empty {
		return stateError(ErrorConflict, fmt.Errorf("post-exchange candidate is not empty"))
	}
	return nil
}

func (manager Manager) verifyExchangeSupport() error {
	left, err := os.MkdirTemp(manager.ImportRoot, ".exchange-left-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(left) }()
	right, err := os.MkdirTemp(manager.ImportRoot, ".exchange-right-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(right) }()
	for path, marker := range map[string]string{left: "left", right: "right"} {
		if err := os.Chmod(path, 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(path, "identity"), []byte(marker), 0o600); err != nil {
			return err
		}
	}
	if err := manager.exchange()(left, right); err != nil {
		return fmt.Errorf("filesystem does not support atomic directory exchange: %w", err)
	}
	leftMarker, leftErr := os.ReadFile(filepath.Join(left, "identity"))
	rightMarker, rightErr := os.ReadFile(filepath.Join(right, "identity"))
	if leftErr != nil || rightErr != nil || string(leftMarker) != "right" || string(rightMarker) != "left" {
		return fmt.Errorf("filesystem directory exchange did not swap both trees")
	}
	return nil
}

func (manager Manager) cleanupCandidateTransient() error {
	path := manager.candidateTransientPath()
	exists, err := pathExists(path)
	if err != nil {
		return err
	}
	if exists {
		if err := safeRemoveTree(path); err != nil {
			return err
		}
	}
	return privatefs.SyncDirectory(manager.ImportRoot)
}

func (manager Manager) cleanupVerificationTransient() error {
	path := manager.verificationTransientPath()
	exists, err := pathExists(path)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	if err := safeRemoveTree(path); err != nil {
		return err
	}
	return privatefs.SyncDirectory(manager.ImportRoot)
}

func cleanupCurrentTransient(state statePaths) error {
	exists, err := pathExists(state.transient)
	if err != nil {
		return err
	}
	if exists {
		if err := safeRemoveTree(state.transient); err != nil {
			return err
		}
	}
	return privatefs.SyncDirectory(state.importRoot)
}

func (manager Manager) findMatchingTransient(ctx context.Context, expected string) (string, error) {
	path := manager.candidateTransientPath()
	exists, err := pathExists(path)
	if err != nil {
		return "", err
	}
	if !exists {
		return "", nil
	}
	identity, err := treeID(ctx, path)
	if err != nil || identity != expected {
		return "", fmt.Errorf("candidate transient differs from immutable preparation")
	}
	return path, nil
}

func (manager Manager) sourcePath() string    { return filepath.Join(manager.ImportRoot, "source") }
func (manager Manager) candidatePath() string { return filepath.Join(manager.ImportRoot, "candidate") }
func (manager Manager) candidateTransientPath() string {
	return filepath.Join(manager.ImportRoot, candidateTransientName)
}
func (manager Manager) verificationTransientPath() string {
	return filepath.Join(manager.ImportRoot, verifyTransient)
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

func (manager Manager) onlineBackup() func(context.Context, string, string) error {
	return manager.OnlineBackup
}

func (manager Manager) candidateVerifier() CandidateVerifier {
	return manager.VerifyCandidate
}

func (manager Manager) activate(identity deployment.Identity, notBefore time.Time) error {
	if manager.Activate != nil {
		return manager.Activate(identity, notBefore)
	}
	_, err := (deployment.Manager{
		StateDir:      manager.StateDir,
		DataDir:       manager.DataDir,
		ProjectCommit: identity.ProjectCommit,
		Release:       identity.Release,
		Now:           manager.Now,
	}).PublishInitialAtOrAfter(notBefore)
	if err != nil {
		if deployment.IsKind(err, deployment.ErrorConflict) {
			return stateError(ErrorConflict, err)
		}
		return stateError(ErrorInvalidState, err)
	}
	return nil
}

func requireDecisionActivation(active deployment.Active, decision Decision) error {
	activatedAt, activeErr := time.Parse(timestampLayout, active.ActivatedAt)
	decidedAt, decisionErr := time.Parse(timestampLayout, decision.DecidedAt)
	if activeErr != nil || decisionErr != nil || activatedAt.Before(decidedAt) {
		return fmt.Errorf("active identity predates import data exchange")
	}
	return nil
}
