package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hellodword/vaultwarden-less/internal/buildinfo"
	"github.com/hellodword/vaultwarden-less/internal/config"
	"github.com/hellodword/vaultwarden-less/internal/privatefs"
	"github.com/hellodword/vaultwarden-less/internal/restic"
	restoretool "github.com/hellodword/vaultwarden-less/internal/restore"
	"github.com/hellodword/vaultwarden-less/internal/snapshot"
	"github.com/hellodword/vaultwarden-less/internal/upgrade"
	"github.com/spf13/cobra"
)

const upgradeDrillURL = "http://drill-vaultwarden/alive"

func upgradeCommand(configPath *string, environment commandEnvironment) *cobra.Command {
	command := &cobra.Command{
		Use: "upgrade", Short: "Run the explicit A/B upgrade state machine", Args: noArguments,
		RunE: func(_ *cobra.Command, _ []string) error { return usageError("An upgrade subcommand is required") },
	}
	var toCommit, toRelease string
	prepare := &cobra.Command{
		Use: "prepare", Short: "Create immutable A-owned upgrade identity state", Args: noArguments,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if toCommit == "" || toRelease == "" {
				return usageError("--to-commit and --to-release are required")
			}
			if !upgrade.ValidCommit(toCommit) || !upgrade.ValidRelease(toRelease, toCommit) {
				return usageError("--to-commit and --to-release must be one matching immutable clean identity")
			}
			manager, err := upgradeManager(environment)
			if err != nil {
				return err
			}
			result, err := manager.Prepare(toCommit, toRelease)
			if err != nil {
				return classifyUpgradeFailure(err)
			}
			return writeJSON(cmd.OutOrStdout(), result)
		},
	}
	prepare.Flags().StringVar(&toCommit, "to-commit", "", "candidate full commit")
	prepare.Flags().StringVar(&toRelease, "to-release", "", "candidate immutable release")

	freeze := &cobra.Command{
		Use: "freeze", Short: "Freeze A evidence and the same-filesystem rollback copy", Args: noArguments,
		RunE: func(cmd *cobra.Command, _ []string) error {
			manager, cfg, collector, err := upgradeEvidenceEnvironment(*configPath, environment, false)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), config.MaintenanceTimeout)
			defer cancel()
			result, err := manager.Freeze(ctx, cfg.UpgradeVerificationTargets, collector)
			if err != nil {
				return classifyUpgradeFailure(err)
			}
			return writeJSON(cmd.OutOrStdout(), result)
		},
	}

	migrateCheck := &cobra.Command{
		Use: "migrate-check", Short: "Record B-owned audited migration and schema evidence", Args: noArguments,
		RunE: func(cmd *cobra.Command, _ []string) error {
			manager, err := upgradeManager(environment)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), config.MaintenanceTimeout)
			defer cancel()
			result, err := manager.MigrateCheck(ctx)
			if err != nil {
				return classifyUpgradeFailure(err)
			}
			return writeJSON(cmd.OutOrStdout(), result)
		},
	}

	verify := &cobra.Command{
		Use: "verify", Short: "Create and really restore B verification-target evidence", Args: noArguments,
		RunE: func(cmd *cobra.Command, _ []string) error {
			manager, cfg, collector, err := upgradeEvidenceEnvironment(*configPath, environment, true)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), config.MaintenanceTimeout)
			defer cancel()
			result, err := manager.Verify(ctx, cfg.UpgradeVerificationTargets, collector)
			if err != nil {
				return classifyUpgradeFailure(err)
			}
			return writeJSON(cmd.OutOrStdout(), result)
		},
	}

	accept := &cobra.Command{
		Use: "accept", Short: "Accept verified B only while its restored drill is healthy", Args: noArguments,
		RunE: func(cmd *cobra.Command, _ []string) error {
			manager, err := upgradeManager(environment)
			if err != nil {
				return err
			}
			if environment.drillCheck == nil {
				return commandFailure(codeInternal, "Upgrade drill verification is unavailable", nil)
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), config.MaintenanceTimeout)
			defer cancel()
			result, err := manager.Accept(ctx, environment.drillCheck)
			if err != nil {
				return classifyUpgradeFailure(err)
			}
			return writeJSON(cmd.OutOrStdout(), result)
		},
	}
	cleanupDrill := &cobra.Command{
		Use: "cleanup-drill", Short: "Remove only the verified terminal upgrade drill", Args: noArguments,
		RunE: func(cmd *cobra.Command, _ []string) error {
			manager, err := upgradeManager(environment)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), config.MaintenanceTimeout)
			defer cancel()
			result, err := manager.CleanupDrill(ctx)
			if err != nil {
				return classifyUpgradeFailure(err)
			}
			return writeJSON(cmd.OutOrStdout(), result)
		},
	}
	finalize := &cobra.Command{
		Use: "finalize", Short: "Archive a decided upgrade after service and drill cleanup checks", Args: noArguments,
		RunE: func(cmd *cobra.Command, _ []string) error {
			manager, err := upgradeManager(environment)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), config.MaintenanceTimeout)
			defer cancel()
			result, err := manager.Finalize(ctx)
			if err != nil {
				return classifyUpgradeFailure(err)
			}
			return writeJSON(cmd.OutOrStdout(), result)
		},
	}
	command.AddCommand(prepare, freeze, migrateCheck, verify, accept, cleanupDrill, finalize)
	return command
}

func rollbackCommand(environment commandEnvironment) *cobra.Command {
	command := &cobra.Command{
		Use: "rollback", Short: "Apply an explicit pre-accept rollback", Args: noArguments,
		RunE: func(_ *cobra.Command, _ []string) error { return usageError("A rollback subcommand is required") },
	}
	command.AddCommand(&cobra.Command{
		Use: "apply", Short: "Atomically restore A and quarantine failed B data", Args: noArguments,
		RunE: func(cmd *cobra.Command, _ []string) error {
			manager, err := rollbackManager(environment)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), config.MaintenanceTimeout)
			defer cancel()
			result, err := manager.Rollback(ctx)
			if err != nil {
				return classifyUpgradeFailure(err)
			}
			return writeJSON(cmd.OutOrStdout(), result)
		},
	})
	return command
}

func rollbackManager(environment commandEnvironment) (upgrade.Manager, error) {
	return upgradeManager(environment)
}

func upgradeManager(environment commandEnvironment) (upgrade.Manager, error) {
	if environment.loadDeployment == nil {
		return upgrade.Manager{}, commandFailure(codeInternal, "Deployment validation is unavailable", nil)
	}
	deployment, err := environment.loadDeployment(environment.deploymentPath)
	if err != nil {
		return upgrade.Manager{}, commandFailure(codeInvalidConfig, "The deployment environment is invalid", err)
	}
	return upgrade.Manager{
		StateDir:      environment.stateDir,
		DataDir:       environment.dataDir,
		RestoreRoot:   environment.restoreRoot,
		Deployment:    deployment,
		ProjectCommit: buildinfo.ProjectCommit,
		Release:       buildinfo.Release,
		Now:           environment.now,
		Database: func(ctx context.Context, path string) (upgrade.DatabaseContract, error) {
			contract, err := snapshot.ValidateDatabaseContract(ctx, path)
			return upgrade.DatabaseContract(contract), err
		},
		PrepareFreeze: func(ctx context.Context, dataDir string) error {
			return snapshot.PrepareStoppedDatabase(ctx, filepath.Join(dataDir, "db.sqlite3"))
		},
		RemoveDrill: func(ctx context.Context, output, projectCommit string) error {
			if output != filepath.Join(environment.restoreRoot, "drill") {
				return fmt.Errorf("upgrade drill cleanup path is not exact")
			}
			manifest, err := snapshot.Verify(ctx, output)
			if err != nil {
				return err
			}
			if manifest.ProjectCommit != projectCommit {
				return fmt.Errorf("upgrade drill belongs to another project commit")
			}
			if err := os.RemoveAll(output); err != nil {
				return err
			}
			return privatefs.SyncDirectory(environment.restoreRoot)
		},
	}, nil
}

func upgradeEvidenceEnvironment(
	configPath string,
	environment commandEnvironment,
	retainDrill bool,
) (upgrade.Manager, *config.Backup, upgrade.EvidenceCollector, error) {
	manager, err := upgradeManager(environment)
	if err != nil {
		return upgrade.Manager{}, nil, nil, err
	}
	cfg, err := environment.backup(configPath)
	if err != nil {
		return upgrade.Manager{}, nil, nil, commandFailure(codeInvalidConfig, "The backup configuration is invalid", err)
	}
	repository, err := environment.repositoryFor(cfg)
	if err != nil {
		return upgrade.Manager{}, nil, nil, commandFailure(codeInvalidCredentials, "Maintenance credentials are invalid", err)
	}
	collector := func(ctx context.Context, projectCommit string, targetNames []string) ([]upgrade.TargetEvidence, error) {
		return collectUpgradeEvidence(ctx, environment, cfg, repository, projectCommit, targetNames, retainDrill)
	}
	return manager, cfg, collector, nil
}

func collectUpgradeEvidence(
	ctx context.Context,
	environment commandEnvironment,
	cfg *config.Backup,
	repository repository,
	projectCommit string,
	targetNames []string,
	retainDrill bool,
) ([]upgrade.TargetEvidence, error) {
	if err := repository.VerifyVersion(ctx); err != nil {
		return nil, commandFailure(codeRepositoryFailure, "Restic version verification failed", err)
	}
	manifest, err := (snapshot.Builder{
		DataDir: environment.dataDir, StagingDir: environment.stagingDir, ProjectCommit: projectCommit,
	}).Build(ctx)
	if err != nil {
		return nil, commandFailure(codeSourceInconsistent, "Upgrade recovery source is inconsistent", err)
	}
	evidence := make([]upgrade.TargetEvidence, 0, len(targetNames))
	for index, name := range targetNames {
		target, exists := cfg.Target(name)
		if !exists {
			return nil, commandFailure(codeInvalidConfig, "Upgrade verification target is not configured", nil)
		}
		synchronized, err := repository.EnsureExact(ctx, target, manifest)
		if err != nil {
			return nil, commandFailure(codeRepositoryFailure, "Upgrade target snapshot failed", err)
		}
		if !validUpgradeSync(manifest, synchronized) {
			return nil, commandFailure(codeVerification, "Upgrade target returned invalid snapshot evidence", nil)
		}
		output, retained, err := upgradeRestoreOutput(ctx, environment.restoreRoot, retainDrill && index == 0, projectCommit, synchronized)
		if err != nil {
			return nil, commandFailure(codeVerification, "Upgrade restore output is unsafe", err)
		}
		restored, err := environment.restore(ctx, repository, restoretool.Request{
			Target: target, SnapshotID: synchronized.SnapshotID, Output: output, RestoreRoot: environment.restoreRoot,
		})
		if err != nil {
			return nil, commandFailure(codeVerification, "Upgrade target restore verification failed", err)
		}
		if restored.SchemaVersion != restoretool.ResultSchemaVersion || restored.Target != target.Name ||
			restored.SnapshotID != synchronized.SnapshotID || restored.ProjectCommit != projectCommit ||
			restored.ContentID != synchronized.ContentID || restored.ArchiveID != synchronized.ArchiveID || restored.Output != output {
			return nil, commandFailure(codeVerification, "Upgrade restored evidence does not match the target snapshot", nil)
		}
		if !retained {
			if err := removeVerifiedUpgradeRestore(ctx, environment.restoreRoot, output); err != nil {
				return nil, commandFailure(codeVerification, "Upgrade restore cleanup failed", err)
			}
		}
		evidence = append(evidence, upgrade.TargetEvidence{
			Target: target.Name, SnapshotID: synchronized.SnapshotID,
			ContentID: synchronized.ContentID, ArchiveID: synchronized.ArchiveID,
		})
	}
	return evidence, nil
}

func validUpgradeSync(manifest snapshot.Manifest, synchronized restic.SyncResult) bool {
	return validSnapshotID(synchronized.SnapshotID) && synchronized.ContentID == manifest.ContentID &&
		synchronized.ArchiveID == manifest.ArchiveID
}

func upgradeRestoreOutput(
	ctx context.Context,
	restoreRoot string,
	retain bool,
	projectCommit string,
	synchronized restic.SyncResult,
) (string, bool, error) {
	if retain {
		output := filepath.Join(restoreRoot, "drill")
		if _, err := os.Lstat(output); err == nil {
			manifest, verifyErr := snapshot.Verify(ctx, output)
			if verifyErr != nil || manifest.ProjectCommit != projectCommit || manifest.ContentID != synchronized.ContentID ||
				manifest.ArchiveID != synchronized.ArchiveID {
				return "", false, fmt.Errorf("existing drill recovery point is not reusable")
			}
			if err := os.RemoveAll(output); err != nil {
				return "", false, err
			}
			if err := privatefs.SyncDirectory(restoreRoot); err != nil {
				return "", false, err
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", false, err
		}
		return output, true, nil
	}
	temporary, err := os.MkdirTemp(restoreRoot, ".upgrade-evidence-")
	if err != nil {
		return "", false, err
	}
	if err := os.Remove(temporary); err != nil {
		return "", false, err
	}
	return temporary, false, nil
}

func removeVerifiedUpgradeRestore(ctx context.Context, root, output string) error {
	relative, err := filepath.Rel(root, output)
	if err != nil || relative == "." || relative == ".." || filepath.IsAbs(relative) ||
		strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return fmt.Errorf("restore cleanup path escapes root")
	}
	if _, err := snapshot.Verify(ctx, output); err != nil {
		return err
	}
	if err := os.RemoveAll(output); err != nil {
		return err
	}
	return privatefs.SyncDirectory(root)
}

func classifyUpgradeFailure(err error) error {
	var commandError *CommandError
	if errors.As(err, &commandError) {
		return commandError
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return classifyError(err)
	}
	switch {
	case upgrade.IsKind(err, upgrade.ErrorConflict):
		return commandFailure(codeConflict, "The requested upgrade phase conflicts with durable state", err)
	case upgrade.IsKind(err, upgrade.ErrorInvalidState):
		return commandFailure(codeUpgradeState, "Upgrade state is missing, corrupt, or unsafe", err)
	case upgrade.IsKind(err, upgrade.ErrorSourceInconsistent):
		return commandFailure(codeSourceInconsistent, "The active upgrade source is inconsistent", err)
	case upgrade.IsKind(err, upgrade.ErrorVerification):
		return commandFailure(codeVerification, "Upgrade evidence verification failed", err)
	default:
		return classifyError(err)
	}
}

func checkUpgradeDrill(ctx context.Context) error {
	client := &http.Client{
		Timeout: 5 * time.Second,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, upgradeDrillURL, nil)
	if err != nil {
		return err
	}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("upgrade drill is not healthy")
	}
	return nil
}
