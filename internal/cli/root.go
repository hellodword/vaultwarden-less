package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/hellodword/vaultwarden-less/internal/buildinfo"
	"github.com/hellodword/vaultwarden-less/internal/config"
	"github.com/hellodword/vaultwarden-less/internal/restic"
	"github.com/spf13/cobra"
)

const (
	codeUsageError         = "usage_error"
	codeInvalidConfig      = "invalid_config"
	codeInvalidCredentials = "invalid_credentials"
	codeUnsafePath         = "unsafe_path"
	codeNotReady           = "not_ready"
	codeConflict           = "conflict"
	codeSourceInconsistent = "source_inconsistent"
	codeUnsupportedSchema  = "unsupported_schema"
	codeRepositoryFailure  = "repository_failure"
	codeVerification       = "verification_failure"
	codeTimeout            = "timeout"
	codeUpgradeState       = "upgrade_state_invalid"
	codeDeploymentState    = "deployment_state_invalid"
	codeInternal           = "internal_error"
)

var stableErrorCodes = map[string]struct{}{
	codeUsageError: {}, codeInvalidConfig: {}, codeInvalidCredentials: {}, codeUnsafePath: {}, codeNotReady: {},
	codeConflict: {}, codeSourceInconsistent: {}, codeUnsupportedSchema: {}, codeRepositoryFailure: {},
	codeVerification: {}, codeTimeout: {}, codeUpgradeState: {}, codeDeploymentState: {}, codeInternal: {},
}

type CommandError struct {
	Code    string
	Message string
	Usage   bool
	Cause   error
}

func (err *CommandError) Error() string { return err.Message }
func (err *CommandError) Unwrap() error { return err.Cause }

type errorDocument struct {
	SchemaVersion int `json:"schema_version"`
	Error         struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	classified := classifyError(err)
	if classified.Usage {
		return 2
	}
	return 1
}

func WriteError(writer io.Writer, err error) {
	if err == nil {
		return
	}
	classified := classifyError(err)
	document := errorDocument{SchemaVersion: ResultSchemaVersion}
	document.Error.Code = classified.Code
	document.Error.Message = classified.Message
	_ = json.NewEncoder(writer).Encode(document)
}

func classifyError(err error) *CommandError {
	var commandError *CommandError
	if errors.As(err, &commandError) {
		if _, valid := stableErrorCodes[commandError.Code]; valid && commandError.Message != "" {
			return commandError
		}
		return commandFailure(codeInternal, "The command failed internally", err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return commandFailure(codeTimeout, "The operation timed out", err)
	}
	var operationError *restic.OperationError
	if errors.As(err, &operationError) {
		if operationError.Code == "timeout" {
			return commandFailure(codeTimeout, "The repository operation timed out", err)
		}
		return commandFailure(codeRepositoryFailure, "The repository operation failed", err)
	}
	return commandFailure(codeInternal, "The command failed internally", err)
}

func commandFailure(code, message string, cause error) *CommandError {
	return &CommandError{Code: code, Message: message, Cause: cause}
}

func New(stdout, stderr io.Writer) *cobra.Command {
	return newCommand(stdout, stderr, defaultEnvironment(stderr))
}

// Execute classifies Cobra's otherwise untyped command-resolution failures at
// the boundary, before any command action can run.
func Execute(command *cobra.Command, arguments []string) error {
	command.SetArgs(arguments)
	if _, _, err := command.Find(arguments); err != nil {
		return usageError("Invalid command or arguments")
	}
	return command.Execute()
}

func newCommand(stdout, stderr io.Writer, environment commandEnvironment) *cobra.Command {
	configPath := config.DefaultBackupConfigPath
	root := &cobra.Command{
		Use:           "vaultwarden-less-backup",
		Short:         "Create and restore vaultwarden-less recovery points",
		SilenceUsage:  true,
		SilenceErrors: true,
		Args:          noArguments,
		RunE: func(_ *cobra.Command, _ []string) error {
			return usageError("A subcommand is required")
		},
		PersistentPreRunE: func(command *cobra.Command, _ []string) error {
			if dirtyAllowed(command) || environment.isReady() {
				return nil
			}
			return commandFailure(codeNotReady, "A clean release identity is required for this command", nil)
		},
	}
	root.SetOut(stdout)
	root.SetErr(stderr)
	root.PersistentFlags().StringVar(&configPath, "config", config.DefaultBackupConfigPath, "path to private backup.yaml")
	root.SetFlagErrorFunc(func(_ *cobra.Command, _ error) error { return usageError("Invalid command flags") })
	root.CompletionOptions.DisableDefaultCmd = true
	root.AddCommand(
		serveCommand(&configPath, environment),
		deploymentCommand(environment),
		requestBackupCommand(environment),
		statusCommand(environment),
		configCommand(&configPath, environment),
		repoCommand(&configPath, environment),
		restoreCommand(&configPath, environment),
		upgradeCommand(&configPath, environment),
		rollbackCommand(environment),
		healthcheckCommand(environment),
		versionCommand(),
	)
	return root
}

func dirtyAllowed(command *cobra.Command) bool {
	path := command.CommandPath()
	return path == "vaultwarden-less-backup version" || path == "vaultwarden-less-backup healthcheck" ||
		path == "vaultwarden-less-backup config validate"
}

type versionResult struct {
	SchemaVersion int     `json:"schema_version"`
	ProjectCommit *string `json:"project_commit"`
	Release       string  `json:"release"`
	Clean         bool    `json:"clean"`
}

func versionCommand() *cobra.Command {
	return &cobra.Command{
		Use: "version", Short: "Print the build recovery identity", Args: noArguments,
		RunE: func(command *cobra.Command, _ []string) error {
			result := versionResult{SchemaVersion: ResultSchemaVersion, Release: "development", Clean: false}
			if buildinfo.Ready() {
				commit := buildinfo.ProjectCommit
				result.ProjectCommit = &commit
				result.Release = buildinfo.Release
				result.Clean = true
			}
			return writeJSON(command.OutOrStdout(), result)
		},
	}
}

func noArguments(_ *cobra.Command, arguments []string) error {
	if len(arguments) != 0 {
		return usageError("Unexpected positional arguments")
	}
	return nil
}

func usageError(message string, arguments ...any) error {
	return &CommandError{Code: codeUsageError, Message: fmt.Sprintf(message, arguments...), Usage: true}
}
