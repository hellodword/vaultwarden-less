package cli

import (
	"fmt"

	"github.com/hellodword/vaultwarden-less/internal/notify"
	"github.com/spf13/cobra"
)

type configValidationResult struct {
	SchemaVersion int      `json:"schema_version"`
	Valid         bool     `json:"valid"`
	Role          string   `json:"role"`
	Targets       []string `json:"targets"`
	Warnings      []string `json:"warnings"`
}

func configCommand(configPath *string, environment commandEnvironment) *cobra.Command {
	command := &cobra.Command{
		Use:   "config",
		Short: "Validate private deployment configuration",
		Args:  noArguments,
		RunE: func(_ *cobra.Command, _ []string) error {
			return usageError("A config subcommand is required")
		},
	}
	var role string
	validate := &cobra.Command{
		Use:   "validate",
		Short: "Validate deployment, role, password, and notifier material without repository access",
		Args:  noArguments,
		PreRunE: func(_ *cobra.Command, _ []string) error {
			if role != "backup" && role != "maintenance" {
				return usageError("--role must be backup or maintenance")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			if environment.loadDeployment == nil {
				return commandFailure(codeInternal, "Deployment validation is unavailable", nil)
			}
			if _, err := environment.loadDeployment(environment.deploymentPath); err != nil {
				return commandFailure(codeInvalidConfig, "The deployment environment is invalid", err)
			}
			cfg, err := environment.backup(*configPath)
			if err != nil {
				return commandFailure(codeInvalidConfig, "The backup configuration is invalid", err)
			}
			if environment.loadCredentials == nil {
				return commandFailure(codeInternal, "Role credential validation is unavailable", nil)
			}
			if _, err := environment.loadCredentials(environment.credentialsPath, cfg); err != nil {
				return commandFailure(codeInvalidCredentials, "The selected role credentials are invalid", err)
			}
			if environment.validatePasswords == nil {
				return commandFailure(codeInternal, "Password validation is unavailable", nil)
			}
			if err := environment.validatePasswords(environment.passwordRoot, cfg); err != nil {
				return commandFailure(codeInvalidCredentials, "The Restic password set is invalid", err)
			}
			if environment.validateDirectory == nil {
				return commandFailure(codeInternal, "Private directory validation is unavailable", nil)
			}
			for _, path := range []string{environment.dataDir, environment.stagingDir, environment.cacheDir, environment.restoreRoot, environment.stateDir} {
				if err := environment.validateDirectory(path); err != nil {
					return commandFailure(codeUnsafePath, fmt.Sprintf("A required private directory is unsafe: %s", path), err)
				}
			}
			if role == "backup" {
				notifiers, err := environment.notifiersFor(cfg)
				if err != nil {
					return commandFailure(codeInvalidCredentials, "Notifier credentials are invalid", err)
				}
				if _, err := notify.NewManager(notifiers, nil); err != nil {
					return commandFailure(codeInvalidCredentials, "Notifier credentials are invalid", err)
				}
			}
			targets := make([]string, 0, len(cfg.Targets))
			for _, target := range cfg.Targets {
				targets = append(targets, target.Name)
			}
			return writeJSON(cmd.OutOrStdout(), configValidationResult{
				SchemaVersion: ResultSchemaVersion,
				Valid:         true,
				Role:          role,
				Targets:       targets,
				Warnings:      []string{},
			})
		},
	}
	validate.Flags().StringVar(&role, "role", "", "credential role: backup or maintenance")
	command.AddCommand(validate)
	return command
}
