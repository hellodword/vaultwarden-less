package cli

import (
	"errors"

	"github.com/hellodword/vaultwarden-less/internal/buildinfo"
	"github.com/hellodword/vaultwarden-less/internal/deployment"
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
	command.AddCommand(initialize)
	return command
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
