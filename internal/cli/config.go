package cli

import (
	"fmt"
	"strings"

	"github.com/hellodword/vaultwarden-less/internal/version"
	"github.com/spf13/cobra"
)

type configValidationResult struct {
	SchemaVersion int              `json:"schema_version"`
	Valid         bool             `json:"valid"`
	Warnings      []string         `json:"warnings"`
	Targets       []configTarget   `json:"targets"`
	Notifiers     []configNotifier `json:"notifiers"`
}

type configTarget struct {
	Name    string `json:"name"`
	Backend string `json:"backend"`
}

type configNotifier struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

func configCommand(configPath *string, environment commandEnvironment) *cobra.Command {
	command := &cobra.Command{
		Use:   "config",
		Short: "Validate private deployment configuration",
		Args:  noArguments,
		RunE: func(_ *cobra.Command, _ []string) error {
			return usageError("a config subcommand is required")
		},
	}
	command.AddCommand(&cobra.Command{
		Use:   "validate",
		Short: "Validate backup.yaml, role credentials, password, and notifier material",
		Args:  noArguments,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := environment.backup(*configPath)
			if err != nil {
				return err
			}
			if _, err := environment.loadCredentials(environment.credentialsPath, cfg); err != nil {
				return err
			}
			password, err := environment.readSecret(environment.passwordPath)
			if err != nil {
				return fmt.Errorf("read Restic password: %w", err)
			}
			if strings.ContainsAny(password, "\r\n\x00") {
				return fmt.Errorf("restic password contains a forbidden character")
			}
			if err := environment.validateNotify(cfg.Notifiers); err != nil {
				return err
			}
			result := configValidationResult{
				SchemaVersion: version.SchemaVersion,
				Valid:         true,
				Warnings:      []string{},
				Targets:       make([]configTarget, 0, len(cfg.Targets)),
				Notifiers:     make([]configNotifier, 0, len(cfg.Notifiers)),
			}
			for _, target := range cfg.Targets {
				result.Targets = append(result.Targets, configTarget{Name: target.Name, Backend: target.Backend()})
				for _, warning := range target.Warnings() {
					result.Warnings = append(result.Warnings, target.Name+":"+warning)
				}
			}
			for _, notifier := range cfg.Notifiers {
				result.Notifiers = append(result.Notifiers, configNotifier{Name: notifier.Name, Type: notifier.Type})
			}
			return writeJSON(cmd.OutOrStdout(), result)
		},
	})
	return command
}
