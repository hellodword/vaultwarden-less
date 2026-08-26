package cli

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/hellodword/vaultwarden-less/internal/config"
	"github.com/hellodword/vaultwarden-less/internal/version"
	"github.com/spf13/cobra"
)

type UsageError struct{ Err error }

func (err *UsageError) Error() string { return err.Err.Error() }
func (err *UsageError) Unwrap() error { return err.Err }

func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	var usage *UsageError
	if errors.As(err, &usage) || strings.Contains(err.Error(), "unknown command") || strings.Contains(err.Error(), "unknown flag") {
		return 2
	}
	return 1
}

func New(stdout, stderr io.Writer) *cobra.Command {
	return newCommand(stdout, stderr, defaultEnvironment())
}

func newCommand(stdout, stderr io.Writer, environment commandEnvironment) *cobra.Command {
	configPath := config.DefaultBackupConfigPath
	root := &cobra.Command{
		Use:           "vaultwarden-less-backup",
		Short:         "Create and restore vaultwarden-less 0.1 recovery points",
		Version:       version.Product,
		SilenceUsage:  true,
		SilenceErrors: true,
		Args:          noArguments,
		RunE: func(_ *cobra.Command, _ []string) error {
			return usageError("a subcommand is required")
		},
	}
	root.SetOut(stdout)
	root.SetErr(stderr)
	root.SetVersionTemplate("vaultwarden-less-backup {{.Version}}\n")
	root.PersistentFlags().StringVar(&configPath, "config", config.DefaultBackupConfigPath, "path to private backup.yaml")
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return &UsageError{Err: err} })
	root.CompletionOptions.DisableDefaultCmd = true
	root.AddCommand(
		serveCommand(&configPath, environment),
		requestBackupCommand(environment),
		statusCommand(environment),
		configCommand(&configPath, environment),
		repoCommand(&configPath, environment),
		restoreCommand(&configPath, environment),
		healthcheckCommand(environment),
	)
	return root
}

func noArguments(_ *cobra.Command, arguments []string) error {
	if len(arguments) != 0 {
		return usageError("unexpected positional arguments")
	}
	return nil
}

func usageError(message string, arguments ...any) error {
	return &UsageError{Err: fmt.Errorf(message, arguments...)}
}
