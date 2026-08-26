package cli

import (
	"context"
	"strconv"
	"strings"

	"github.com/hellodword/vaultwarden-less/internal/config"
	"github.com/hellodword/vaultwarden-less/internal/restic"
	"github.com/hellodword/vaultwarden-less/internal/version"
	"github.com/spf13/cobra"
)

type repoOperation func(context.Context, repository, config.Target) (any, error)

type operationResult struct {
	SchemaVersion int    `json:"schema_version"`
	Operation     string `json:"operation"`
	Target        string `json:"target"`
	Success       bool   `json:"success"`
}

type snapshotsResult struct {
	SchemaVersion int               `json:"schema_version"`
	Target        string            `json:"target"`
	Snapshots     []restic.Snapshot `json:"snapshots"`
}

type pruneResult struct {
	SchemaVersion int                 `json:"schema_version"`
	Operation     string              `json:"operation"`
	Target        string              `json:"target"`
	Applied       bool                `json:"applied"`
	Groups        []restic.PruneGroup `json:"groups"`
}

func repoCommand(configPath *string, environment commandEnvironment) *cobra.Command {
	var targetName string
	command := &cobra.Command{
		Use:   "repo",
		Short: "Run explicit thin Restic repository operations",
		Args:  noArguments,
		RunE: func(_ *cobra.Command, _ []string) error {
			return usageError("a repo subcommand is required")
		},
	}
	command.PersistentFlags().StringVar(&targetName, "target", "", "configured target name")
	operation := func(name, description string, action repoOperation) *cobra.Command {
		return &cobra.Command{
			Use: name, Short: description, Args: noArguments,
			RunE: func(cmd *cobra.Command, _ []string) error {
				if targetName == "" {
					return usageError("--target is required")
				}
				cfg, err := environment.backup(*configPath)
				if err != nil {
					return err
				}
				target, err := findTarget(cfg, targetName)
				if err != nil {
					return err
				}
				repository, err := environment.repositoryFor(cfg)
				if err != nil {
					return err
				}
				if err := repository.VerifyVersion(cmd.Context()); err != nil {
					return err
				}
				ctx, cancel := context.WithTimeout(cmd.Context(), config.MaintenanceTimeout)
				defer cancel()
				result, err := action(ctx, repository, target)
				if err != nil {
					return err
				}
				return writeJSON(cmd.OutOrStdout(), result)
			},
		}
	}

	initCommand := operation("init", "Initialize the configured Restic repository", func(ctx context.Context, repository repository, target config.Target) (any, error) {
		if err := repository.Init(ctx, target); err != nil {
			return nil, err
		}
		return successfulOperation("init", target.Name), nil
	})
	snapshotsCommand := operation("snapshots", "List new-product snapshots from newest to oldest", func(ctx context.Context, repository repository, target config.Target) (any, error) {
		snapshots, err := repository.Snapshots(ctx, target)
		if err != nil {
			return nil, err
		}
		if snapshots == nil {
			snapshots = []restic.Snapshot{}
		}
		return snapshotsResult{SchemaVersion: version.SchemaVersion, Target: target.Name, Snapshots: snapshots}, nil
	})
	var subset string
	checkCommand := operation("check", "Check repository metadata and optional one-twelfth data subset", func(ctx context.Context, repository repository, target config.Target) (any, error) {
		if err := repository.Check(ctx, target, subset); err != nil {
			return nil, err
		}
		return successfulOperation("check", target.Name), nil
	})
	checkCommand.Flags().StringVar(&subset, "read-data-subset", "", "optional n/12 data subset")
	checkCommand.PreRunE = func(_ *cobra.Command, _ []string) error {
		if !validSubset(subset) {
			return usageError("--read-data-subset must be n/12 where n is 1 through 12")
		}
		return nil
	}
	unlockCommand := operation("unlock", "Remove stale Restic repository locks", func(ctx context.Context, repository repository, target config.Target) (any, error) {
		if err := repository.Unlock(ctx, target); err != nil {
			return nil, err
		}
		return successfulOperation("unlock", target.Name), nil
	})
	var apply bool
	pruneCommand := operation("prune", "Apply fixed retention, using dry-run by default", func(ctx context.Context, repository repository, target config.Target) (any, error) {
		result, err := repository.Prune(ctx, target, apply)
		if err != nil {
			return nil, err
		}
		if result.Groups == nil {
			result.Groups = []restic.PruneGroup{}
		}
		return pruneResult{
			SchemaVersion: version.SchemaVersion,
			Operation:     "prune",
			Target:        target.Name,
			Applied:       result.Applied,
			Groups:        result.Groups,
		}, nil
	})
	pruneCommand.Flags().BoolVar(&apply, "apply", false, "perform deletion; default is Restic dry-run")
	command.AddCommand(initCommand, snapshotsCommand, checkCommand, unlockCommand, pruneCommand)
	return command
}

func successfulOperation(operation, target string) operationResult {
	return operationResult{SchemaVersion: version.SchemaVersion, Operation: operation, Target: target, Success: true}
}

func validSubset(value string) bool {
	if value == "" {
		return true
	}
	left, right, ok := strings.Cut(value, "/")
	if !ok || right != "12" || strings.Contains(left, "+") {
		return false
	}
	part, err := strconv.Atoi(left)
	return err == nil && part >= 1 && part <= 12 && strconv.Itoa(part) == left
}
