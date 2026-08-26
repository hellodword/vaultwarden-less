package cli

import (
	"errors"
	"fmt"
	"io"

	"github.com/hellodword/vaultwarden-less/internal/buildinfo"
	"github.com/hellodword/vaultwarden-less/internal/config"
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
	if errors.As(err, &usage) {
		return 2
	}
	return 1
}

func New(stdout, stderr io.Writer) *cobra.Command {
	return newCommand(stdout, stderr, defaultEnvironment(stderr))
}

// Execute classifies Cobra's otherwise untyped command-resolution failures at
// the boundary, before any command action can run.
func Execute(command *cobra.Command, arguments []string) error {
	command.SetArgs(arguments)
	if _, _, err := command.Find(arguments); err != nil {
		return &UsageError{Err: err}
	}
	return command.Execute()
}

func newCommand(stdout, stderr io.Writer, environment commandEnvironment) *cobra.Command {
	configPath := config.DefaultBackupConfigPath
	root := &cobra.Command{
		Use:           "vaultwarden-less-backup",
		Short:         "Create and restore vaultwarden-less recovery points",
		Version:       buildinfo.Version,
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
