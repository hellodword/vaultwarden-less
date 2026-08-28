package cli

import (
	"context"
	"errors"
	"path/filepath"

	"github.com/hellodword/vaultwarden-less/internal/buildinfo"
	"github.com/hellodword/vaultwarden-less/internal/deployment"
	"github.com/hellodword/vaultwarden-less/internal/importer"
	"github.com/hellodword/vaultwarden-less/internal/snapshot"
	"github.com/spf13/cobra"
)

func deploymentCommand(environment commandEnvironment) *cobra.Command {
	command := &cobra.Command{
		Use:   "deployment",
		Short: "Manage the durable active release identity",
		Args:  noArguments,
		RunE: func(_ *cobra.Command, _ []string) error {
			return usageError("A deployment subcommand is required")
		},
	}
	initialize := &cobra.Command{
		Use:   "init",
		Short: "Initialize an empty deployment with this immutable release",
		Args:  noArguments,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if environment.loadDeployment == nil {
				return commandFailure(codeInternal, "Deployment validation is unavailable", nil)
			}
			if _, err := environment.loadDeployment(environment.deploymentPath); err != nil {
				return commandFailure(codeInvalidConfig, "The deployment environment is invalid", err)
			}
			result, err := (deployment.Manager{
				StateDir:      environment.stateDir,
				DataDir:       environment.dataDir,
				ProjectCommit: buildinfo.ProjectCommit,
				Release:       buildinfo.Release,
				Now:           environment.now,
			}).Init()
			if err != nil {
				return classifyDeploymentFailure(err)
			}
			return writeJSON(cmd.OutOrStdout(), result)
		},
	}
	command.AddCommand(initialize, importWorkflowCommand(environment))
	return command
}

func importWorkflowCommand(environment commandEnvironment) *cobra.Command {
	command := &cobra.Command{
		Use:   "import",
		Short: "Run the one-time external SQLite import state machine",
		Args:  noArguments,
		RunE: func(_ *cobra.Command, _ []string) error {
			return usageError("An import subcommand is required")
		},
	}
	prepare := &cobra.Command{
		Use: "prepare", Short: "Create a private candidate from the stopped external source", Args: noArguments,
		RunE: func(cmd *cobra.Command, _ []string) error {
			manager, err := importManager(environment)
			if err != nil {
				return err
			}
			result, err := manager.Prepare(cmd.Context())
			if err != nil {
				return classifyImportFailure(err)
			}
			return writeJSON(cmd.OutOrStdout(), result)
		},
	}
	var confirmClientLogin bool
	verify := &cobra.Command{
		Use: "verify", Short: "Verify the stopped migrated candidate and real client login", Args: noArguments,
		PreRunE: func(_ *cobra.Command, _ []string) error {
			if !confirmClientLogin {
				return usageError("--confirm-client-login is required")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			manager, err := importManager(environment)
			if err != nil {
				return err
			}
			result, err := manager.Verify(cmd.Context(), confirmClientLogin)
			if err != nil {
				return classifyImportFailure(err)
			}
			return writeJSON(cmd.OutOrStdout(), result)
		},
	}
	verify.Flags().BoolVar(&confirmClientLogin, "confirm-client-login", false, "confirm a successful login through the real HTTPS client path")
	accept := &cobra.Command{
		Use: "accept", Short: "Commit the immutable import decision and atomically promote candidate data", Args: noArguments,
		RunE: func(cmd *cobra.Command, _ []string) error {
			manager, err := importManager(environment)
			if err != nil {
				return err
			}
			result, err := manager.Accept(cmd.Context())
			if err != nil {
				return classifyImportFailure(err)
			}
			return writeJSON(cmd.OutOrStdout(), result)
		},
	}
	cancel := &cobra.Command{
		Use: "cancel", Short: "Remove only the disposable undecided import candidate", Args: noArguments,
		RunE: func(cmd *cobra.Command, _ []string) error {
			manager, err := importManager(environment)
			if err != nil {
				return err
			}
			result, err := manager.Cancel(cmd.Context())
			if err != nil {
				return classifyImportFailure(err)
			}
			return writeJSON(cmd.OutOrStdout(), result)
		},
	}
	finalize := &cobra.Command{
		Use: "finalize", Short: "Archive a healthy accepted initial import", Args: noArguments,
		RunE: func(cmd *cobra.Command, _ []string) error {
			manager, err := importManager(environment)
			if err != nil {
				return err
			}
			result, err := manager.Finalize(cmd.Context())
			if err != nil {
				return classifyImportFailure(err)
			}
			return writeJSON(cmd.OutOrStdout(), result)
		},
	}
	command.AddCommand(prepare, verify, accept, cancel, finalize)
	return command
}

func importManager(environment commandEnvironment) (importer.Manager, error) {
	if environment.loadDeployment == nil {
		return importer.Manager{}, commandFailure(codeInternal, "Deployment validation is unavailable", nil)
	}
	if _, err := environment.loadDeployment(environment.deploymentPath); err != nil {
		return importer.Manager{}, commandFailure(codeInvalidConfig, "The deployment environment is invalid", err)
	}
	root := environment.deploymentRoot
	return importer.Manager{
		RootDir:         root,
		StateDir:        filepath.Join(root, "state"),
		DataDir:         filepath.Join(root, "data"),
		ImportRoot:      filepath.Join(root, "import"),
		ProjectCommit:   buildinfo.ProjectCommit,
		Release:         buildinfo.Release,
		Now:             environment.now,
		OnlineBackup:    snapshot.OnlineBackup,
		VerifyCandidate: verifyImportCandidate,
	}, nil
}

func verifyImportCandidate(ctx context.Context, candidate, staging, commit string) (importer.CandidateEvidence, error) {
	if err := snapshot.PrepareStoppedDatabase(ctx, filepath.Join(candidate, "db.sqlite3")); err != nil {
		return importer.CandidateEvidence{}, err
	}
	contract, err := snapshot.ValidateDatabaseContract(ctx, filepath.Join(candidate, "db.sqlite3"))
	if err != nil {
		return importer.CandidateEvidence{}, err
	}
	manifest, err := (snapshot.Builder{DataDir: candidate, StagingDir: staging, ProjectCommit: commit}).Build(ctx)
	if err != nil {
		return importer.CandidateEvidence{}, err
	}
	return importer.CandidateEvidence{
		DatabaseMigration:         contract.Migration,
		DatabaseSchemaFingerprint: contract.SchemaFingerprint,
		ContentID:                 manifest.ContentID,
		ArchiveID:                 manifest.ArchiveID,
	}, nil
}

func classifyImportFailure(err error) error {
	switch {
	case importer.IsKind(err, importer.ErrorConflict):
		return commandFailure(codeConflict, "The import conflicts with durable state", err)
	case importer.IsKind(err, importer.ErrorSourceInconsistent):
		return commandFailure(codeSourceInconsistent, "The external import source changed or is unsafe", err)
	case importer.IsKind(err, importer.ErrorVerification):
		return commandFailure(codeVerification, "The import candidate could not be verified", err)
	case importer.IsKind(err, importer.ErrorInvalidState):
		return commandFailure(codeImportState, "The import state is invalid", err)
	default:
		var state *importer.StateError
		if errors.As(err, &state) {
			return commandFailure(codeImportState, "The import state is invalid", err)
		}
		return commandFailure(codeInternal, "The import operation failed", err)
	}
}

func classifyDeploymentFailure(err error) error {
	switch {
	case deployment.IsKind(err, deployment.ErrorConflict):
		return commandFailure(codeConflict, "The deployment identity conflicts with durable state", err)
	case deployment.IsKind(err, deployment.ErrorInvalidState), deployment.IsKind(err, deployment.ErrorNotInitialized):
		return commandFailure(codeDeploymentState, "The deployment identity state is invalid", err)
	default:
		var state *deployment.StateError
		if errors.As(err, &state) {
			return commandFailure(codeDeploymentState, "The deployment identity state is invalid", err)
		}
		return commandFailure(codeInternal, "The deployment identity operation failed", err)
	}
}
