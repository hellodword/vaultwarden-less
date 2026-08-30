package cli

import (
	"fmt"
	"path/filepath"

	"github.com/hellodword/vaultwarden-less/internal/notify"
	"github.com/spf13/cobra"
)

type configValidationResult struct {
	Valid   bool     `json:"valid"`
	Role    string   `json:"role"`
	Targets []string `json:"targets"`
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
			if _, err := environment.loadDeployment(); err != nil {
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
			if environment.validateFile == nil {
				return commandFailure(codeInternal, "Private file validation is unavailable", nil)
			}
			if err := environment.validateFile(environment.vaultwardenPath); err != nil {
				return commandFailure(codeUnsafePath, "The Vaultwarden environment file is unsafe", err)
			}
			if environment.validateDirectory == nil {
				return commandFailure(codeInternal, "Private directory validation is unavailable", nil)
			}
			for _, path := range []string{environment.deploymentRoot, environment.dataDir, environment.stagingDir, environment.cacheDir, environment.restoreRoot, environment.stateDir} {
				if err := environment.validateDirectory(path); err != nil {
					return commandFailure(codeUnsafePath, fmt.Sprintf("A required private directory is unsafe: %s", path), err)
				}
			}
			if environment.validateMount == nil {
				return commandFailure(codeInternal, "Deployment mount validation is unavailable", nil)
			}
			mounts := [][2]string{
				{filepath.Join(environment.deploymentRoot, "config", "backup.yaml"), *configPath},
				{filepath.Join(environment.deploymentRoot, "config", "vaultwarden.env"), environment.vaultwardenPath},
				{filepath.Join(environment.deploymentRoot, "credentials", role+".yaml"), environment.credentialsPath},
				{filepath.Join(environment.deploymentRoot, "credentials", "restic"), environment.passwordRoot},
				{filepath.Join(environment.deploymentRoot, "cache"), environment.cacheDir},
				{filepath.Join(environment.deploymentRoot, "data"), environment.dataDir},
				{filepath.Join(environment.deploymentRoot, "staging"), environment.stagingDir},
				{filepath.Join(environment.deploymentRoot, "restore"), environment.restoreRoot},
				{filepath.Join(environment.deploymentRoot, "state"), environment.stateDir},
			}
			if role == "backup" {
				mounts = append(mounts, [2]string{
					filepath.Join(environment.deploymentRoot, "credentials", "notifiers.yaml"), environment.notifierPath,
				})
			}
			for _, mount := range mounts {
				if err := environment.validateMount(mount[0], mount[1]); err != nil {
					return commandFailure(codeUnsafePath, "A private bind does not match the deployment root", err)
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
				Valid:   true,
				Role:    role,
				Targets: targets,
			})
		},
	}
	validate.Flags().StringVar(&role, "role", "", "credential role: backup or maintenance")
	command.AddCommand(validate)
	return command
}
