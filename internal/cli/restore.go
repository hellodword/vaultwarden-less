package cli

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/hellodword/vaultwarden-less/internal/config"
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
			if snapshotID != "" && !validSnapshotID(snapshotID) {
				return usageError("--snapshot must be a full 64-character lowercase hexadecimal ID")
			}
			if !validOutputArgument(environment.restoreRoot, output) {
				return usageError("--output must be a clean absolute child of the restore root")
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
			result, err := environment.restore(ctx, repository, restoretool.Request{
				Target: target, SnapshotID: snapshotID, Output: output, RestoreRoot: environment.restoreRoot,
			})
			if err != nil {
				return fmt.Errorf("restore operation failed")
			}
			return writeJSON(cmd.OutOrStdout(), result)
		},
	}
	command.Flags().StringVar(&targetName, "target", "", "configured target name")
	command.Flags().StringVar(&snapshotID, "snapshot", "", "full snapshot ID; defaults to the latest new-product snapshot")
	command.Flags().StringVar(&output, "output", "", "absolute absent output below the restore root")
	return command
}

func validOutputArgument(root, output string) bool {
	if root == "" || output == "" || !filepath.IsAbs(root) || !filepath.IsAbs(output) || filepath.Clean(root) != root || filepath.Clean(output) != output {
		return false
	}
	relative, err := filepath.Rel(root, output)
	return err == nil && relative != "." && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}
