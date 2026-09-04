package cli

import (
	"context"
	"path/filepath"
	"regexp"
	"strings"

	restoretool "github.com/hellodword/vaultwarden-less/internal/restore"
	"github.com/spf13/cobra"
)

func restoreCommand(configPath *string, environment commandEnvironment) *cobra.Command {
	var targetName, snapshotID, output string
	command := &cobra.Command{
		Use:   "restore",
		Short: "Restore and verify the latest or one explicit recovery snapshot",
		Args:  noArguments,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if targetName == "" || output == "" {
				return usageError("--target and --output are required")
			}
			if snapshotID != "latest" && !validSnapshotID(snapshotID) {
				return usageError("--snapshot must be a full 64-character lowercase hexadecimal ID")
			}
			if !outputNamePattern.MatchString(output) {
				return usageError("--output must be a valid absent output name")
			}
			resolvedOutput := filepath.Join(environment.restoreRoot, output)
			if !validOutputArgument(environment.restoreRoot, resolvedOutput) {
				return commandFailure(codeUnsafePath, "The restore output path is unsafe", nil)
			}
			cfg, err := environment.backup(*configPath)
			if err != nil {
				return commandFailure(codeInvalidConfig, "The backup configuration is invalid", err)
			}
			target, err := findTarget(cfg, targetName)
			if err != nil {
				return err
			}
			repository, err := environment.repositoryFor(cfg)
			if err != nil {
				return commandFailure(codeInvalidCredentials, "Maintenance credentials are invalid", err)
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), maintenanceTimeout)
			defer cancel()
			if err := repository.VerifyVersion(ctx); err != nil {
				return commandFailure(codeRepositoryFailure, "Restic version verification failed", err)
			}
			requestedSnapshot := snapshotID
			if requestedSnapshot == "latest" {
				requestedSnapshot = ""
			}
			result, err := environment.restore(ctx, repository, restoretool.Request{
				Target: target, SnapshotID: requestedSnapshot, Output: resolvedOutput, RestoreRoot: environment.restoreRoot,
			})
			if err != nil {
				if classified := classifyError(err); classified.Code == codeTimeout || classified.Code == codeRepositoryFailure {
					return classified
				}
				return commandFailure(codeVerification, "Restore verification failed", err)
			}
			return writeJSON(cmd.OutOrStdout(), result)
		},
	}
	command.Flags().StringVar(&targetName, "target", "", "configured target name")
	command.Flags().StringVar(&snapshotID, "snapshot", "latest", "latest or one full snapshot ID")
	command.Flags().StringVar(&output, "output", "", "absent output name below the restore root")
	return command
}

var outputNamePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)

func validOutputArgument(root, output string) bool {
	if root == "" || output == "" || !filepath.IsAbs(root) || !filepath.IsAbs(output) || filepath.Clean(root) != root || filepath.Clean(output) != output {
		return false
	}
	relative, err := filepath.Rel(root, output)
	return err == nil && relative != "." && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}
