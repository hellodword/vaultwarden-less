package cli

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/hellodword/vaultwarden-less/internal/config"
	"github.com/hellodword/vaultwarden-less/internal/notify"
	"github.com/hellodword/vaultwarden-less/internal/restic"
	restoretool "github.com/hellodword/vaultwarden-less/internal/restore"
	"github.com/hellodword/vaultwarden-less/internal/snapshot"
	"github.com/hellodword/vaultwarden-less/internal/state"
	"github.com/hellodword/vaultwarden-less/internal/version"
	"github.com/hellodword/vaultwarden-less/internal/worker"
	"github.com/spf13/cobra"
)

type UsageError struct{ Err error }

func (e *UsageError) Error() string { return e.Err.Error() }
func (e *UsageError) Unwrap() error { return e.Err }

func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	var usage *UsageError
	if errors.As(err, &usage) || strings.Contains(err.Error(), "unknown command") || strings.Contains(err.Error(), "required flag") {
		return 2
	}
	return 1
}

func New(stdout, stderr io.Writer) *cobra.Command {
	var configPath string
	root := &cobra.Command{
		Use:           "vaultwarden-less-backup",
		Short:         "Create and restore vaultwarden-less v1 recovery points",
		Version:       version.Product,
		SilenceUsage:  true,
		SilenceErrors: true,
		Args:          usageNoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return &UsageError{Err: fmt.Errorf("a subcommand is required")}
		},
	}
	root.SetOut(stdout)
	root.SetErr(stderr)
	root.PersistentFlags().StringVar(&configPath, "config", config.DefaultBackupConfigPath, "path to backup.yaml")
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return &UsageError{Err: err} })
	root.AddCommand(
		serveCommand(&configPath), runCommand(&configPath), statusCommand(&configPath),
		configCommand(&configPath), repoCommand(&configPath), restoreCommand(&configPath), healthcheckCommand(),
	)
	root.CompletionOptions.DisableDefaultCmd = true
	return root
}

func serveCommand(configPath *string) *cobra.Command {
	return &cobra.Command{
		Use: "serve", Short: "Run the event endpoint, scheduler, backup targets, and notification delivery", Args: usageNoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.LoadBackup(*configPath)
			if err != nil {
				return err
			}
			lock, err := state.AcquireLock(cfg.StateDir)
			if err != nil {
				return err
			}
			defer func() { _ = lock.Close() }()
			log := slog.New(slog.NewJSONHandler(cmd.ErrOrStderr(), nil))
			runner := newWorkerRunner(cfg)
			runner.Logger = log
			logConfigWarnings(log, cfg)
			store, err := state.Open(cfg.StateDir, targetNames(cfg))
			if err != nil {
				return err
			}
			if store.Rebuilt() {
				log.Warn("runtime state was rebuilt", "error_code", "runtime_state_rebuilt")
			}
			notifiers, err := notify.NewManager(cfg.StateDir, cfg.Notifiers, log)
			if err != nil {
				log.Error("notification persistence is unavailable; backup continues", "error_code", "notification_state_failed")
				notifiers = notify.NewUnavailableManager(log)
			}
			if notifiers.Rebuilt() {
				log.Warn("notification state was rebuilt", "error_code", "notification_state_rebuilt")
			}
			ctx, cancel := context.WithCancel(cmd.Context())
			defer cancel()
			notifiers.Start(ctx)
			coordinator := worker.NewCoordinator(cfg, store, runner, notifiers, log)
			events := worker.NewEventServer(cfg, coordinator)
			serverErr := make(chan error, 1)
			go func() { serverErr <- events.ListenAndServe() }()
			runErr := make(chan error, 1)
			go func() { runErr <- coordinator.Run(ctx) }()
			log.Info("backup worker started", "listen_address", cfg.ListenAddress)
			var result error
			coordinatorDone := false
			select {
			case <-cmd.Context().Done():
			case err := <-serverErr:
				if err != nil {
					result = fmt.Errorf("event server failed: %w", err)
				}
				cancel()
			case err := <-runErr:
				coordinatorDone = true
				if err != nil {
					result = err
				}
				cancel()
			}
			cancel()
			shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout.Duration)
			defer shutdownCancel()
			if err := events.Shutdown(shutdownCtx); err != nil && result == nil {
				result = err
			}
			if !coordinatorDone {
				select {
				case err := <-runErr:
					if err != nil && result == nil {
						result = err
					}
				case <-shutdownCtx.Done():
					if result == nil {
						result = fmt.Errorf("backup worker shutdown timed out")
					}
				}
			}
			notifiers.Wait()
			log.Info("backup worker stopped")
			return result
		},
	}
}

func runCommand(configPath *string) *cobra.Command {
	return &cobra.Command{
		Use: "run", Short: "Create one recovery point and attempt every due target once", Args: usageNoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.LoadBackup(*configPath)
			if err != nil {
				return err
			}
			lock, err := state.AcquireLock(cfg.StateDir)
			if err != nil {
				return err
			}
			defer func() { _ = lock.Close() }()
			log := slog.New(slog.NewJSONHandler(cmd.ErrOrStderr(), nil))
			runner := newWorkerRunner(cfg)
			runner.Logger = log
			logConfigWarnings(log, cfg)
			store, err := state.Open(cfg.StateDir, targetNames(cfg))
			if err != nil {
				return err
			}
			if store.Rebuilt() {
				log.Warn("runtime state was rebuilt", "error_code", "runtime_state_rebuilt")
			}
			notifiers, err := notify.NewManager(cfg.StateDir, cfg.Notifiers, log)
			if err != nil {
				log.Error("notification persistence is unavailable; backup continues", "error_code", "notification_state_failed")
				notifiers = notify.NewUnavailableManager(log)
			}
			if notifiers.Rebuilt() {
				log.Warn("notification state was rebuilt", "error_code", "notification_state_rebuilt")
			}
			ctx, cancel := context.WithCancel(cmd.Context())
			notifiers.Start(ctx)
			coordinator := worker.NewCoordinator(cfg, store, runner, notifiers, log)
			if len(cfg.Targets) != 0 {
				err = coordinator.Reconcile(ctx)
				if err == nil {
					err = coordinator.MarkDirty(time.Now())
				}
				if err == nil {
					err = store.Update(func(next *state.State) error {
						next.Source.NextRetryAt = ""
						for name, target := range next.Targets {
							target.NextRetryAt = ""
							next.Targets[name] = target
						}
						return nil
					})
				}
				if err == nil {
					err = coordinator.Sample(ctx)
				}
			}
			cancel()
			notifiers.Wait()
			statusErr := writeJSON(cmd.OutOrStdout(), store.Snapshot().PublicStatus(targetNames(cfg)))
			if statusErr != nil {
				return statusErr
			}
			if err == nil && store.Snapshot().Current != nil {
				err = fmt.Errorf("one or more backup targets are degraded")
			}
			return err
		},
	}
}

func statusCommand(configPath *string) *cobra.Command {
	return &cobra.Command{
		Use: "status", Short: "Print stable v1 worker status JSON", Args: usageNoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.LoadBackup(*configPath)
			if err != nil {
				return err
			}
			currentStore, err := state.Open(cfg.StateDir, targetNames(cfg))
			if err != nil {
				return err
			}
			return writeJSON(cmd.OutOrStdout(), currentStore.Snapshot().PublicStatus(targetNames(cfg)))
		},
	}
}

func configCommand(configPath *string) *cobra.Command {
	var checkRepositoryCredentials, checkNotifierMaterial bool
	command := &cobra.Command{
		Use: "config", Short: "Validate configuration and print a secret-free v1 summary", Args: usageNoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.LoadBackup(*configPath)
			if err != nil {
				return err
			}
			if checkRepositoryCredentials && len(cfg.Targets) != 0 {
				runner, err := newRunner(cfg)
				if err != nil {
					return err
				}
				for _, target := range cfg.Targets {
					if err := runner.ValidateTarget(target); err != nil {
						return fmt.Errorf("target %s repository credentials: %w", target.Name, err)
					}
				}
			}
			if checkNotifierMaterial {
				if err := notify.ValidateConfigs(cfg.Notifiers); err != nil {
					return err
				}
			}
			type targetSummary struct {
				Name    string `json:"name"`
				Backend string `json:"backend"`
			}
			type notifierSummary struct {
				Name string `json:"name"`
				Type string `json:"type"`
			}
			summary := struct {
				SchemaVersion                int               `json:"schema_version"`
				ProductVersion               string            `json:"product_version"`
				VaultwardenVersion           string            `json:"vaultwarden_version"`
				ResticVersion                string            `json:"restic_version"`
				RepositoryCredentialsChecked bool              `json:"repository_credentials_checked"`
				NotifierMaterialChecked      bool              `json:"notifier_material_checked"`
				Warnings                     []string          `json:"warnings"`
				Targets                      []targetSummary   `json:"targets"`
				Notifiers                    []notifierSummary `json:"notifiers"`
			}{
				SchemaVersion: version.SchemaVersion, ProductVersion: version.Product,
				VaultwardenVersion: version.Vaultwarden, ResticVersion: version.Restic,
				RepositoryCredentialsChecked: checkRepositoryCredentials, NotifierMaterialChecked: checkNotifierMaterial,
				Warnings: make([]string, 0), Targets: make([]targetSummary, 0, len(cfg.Targets)), Notifiers: make([]notifierSummary, 0, len(cfg.Notifiers)),
			}
			for _, target := range cfg.Targets {
				summary.Targets = append(summary.Targets, targetSummary{target.Name, target.Backend()})
				summary.Warnings = append(summary.Warnings, target.Warnings()...)
			}
			for _, notifier := range cfg.Notifiers {
				summary.Notifiers = append(summary.Notifiers, notifierSummary{notifier.Name, notifier.Type})
			}
			return writeJSON(cmd.OutOrStdout(), summary)
		},
	}
	command.Flags().BoolVar(&checkRepositoryCredentials, "check-repository-credentials", false, "validate the mounted repository credentials")
	command.Flags().BoolVar(&checkNotifierMaterial, "check-notifier-material", false, "validate notification material")
	return command
}

func repoCommand(configPath *string) *cobra.Command {
	var targetName string
	repo := &cobra.Command{Use: "repo", Short: "Run explicit Restic repository maintenance", Args: usageNoArgs, RunE: func(_ *cobra.Command, _ []string) error {
		return &UsageError{Err: fmt.Errorf("a repo subcommand is required")}
	}}
	repo.PersistentFlags().StringVar(&targetName, "target", "", "configured target name")
	operation := func(name string, action func(context.Context, *config.Backup, restic.Runner, config.Target) (any, error)) *cobra.Command {
		return &cobra.Command{Use: name, Args: usageNoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
			if targetName == "" {
				return &UsageError{Err: fmt.Errorf("--target is required")}
			}
			cfg, err := config.LoadBackup(*configPath)
			if err != nil {
				return err
			}
			target, err := findTarget(cfg, targetName)
			if err != nil {
				return &UsageError{Err: err}
			}
			runner, err := newRunner(cfg)
			if err != nil {
				return err
			}
			if err := runner.VerifyVersion(cmd.Context()); err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), cfg.MaintenanceTimeout.Duration)
			defer cancel()
			result, err := action(ctx, cfg, runner, target)
			if err != nil {
				return err
			}
			return writeJSON(cmd.OutOrStdout(), result)
		}}
	}
	var snapshotStatus string
	snapshotsCommand := operation("snapshots", func(ctx context.Context, _ *config.Backup, runner restic.Runner, target config.Target) (any, error) {
		status := snapshot.Status(snapshotStatus)
		if snapshotStatus == "all" {
			status = ""
		}
		report, err := runner.ListSnapshots(ctx, target)
		items := report.Snapshots
		if status != "" {
			filtered := make([]restic.SnapshotInfo, 0, len(items))
			for _, item := range items {
				if item.Status == status {
					filtered = append(filtered, item)
				}
			}
			items = filtered
		}
		summaries := make([]snapshotSummary, 0, len(items))
		for _, item := range items {
			summaries = append(summaries, snapshotSummary{ID: item.ID, Time: item.Time, Status: item.Status, ContentID: item.ContentID})
		}
		return struct {
			SchemaVersion int                      `json:"schema_version"`
			Target        string                   `json:"target"`
			Snapshots     []snapshotSummary        `json:"snapshots"`
			Anomalies     []restic.SnapshotAnomaly `json:"anomalies"`
		}{version.SchemaVersion, target.Name, summaries, report.Anomalies}, err
	})
	snapshotsCommand.Flags().StringVar(&snapshotStatus, "status", "all", "all, complete, or incomplete")
	snapshotsCommand.PreRunE = func(_ *cobra.Command, _ []string) error {
		if snapshotStatus != "all" && snapshotStatus != "complete" && snapshotStatus != "incomplete" {
			return &UsageError{Err: fmt.Errorf("invalid --status")}
		}
		return nil
	}
	initCommand := operation("init", func(ctx context.Context, _ *config.Backup, runner restic.Runner, target config.Target) (any, error) {
		err := runner.Init(ctx, target)
		return operationResult("init", target.Name, false), err
	})
	unlockCommand := operation("unlock", func(ctx context.Context, _ *config.Backup, runner restic.Runner, target config.Target) (any, error) {
		err := runner.Unlock(ctx, target)
		return operationResult("unlock", target.Name, false), err
	})
	var subset string
	checkCommand := operation("check", func(ctx context.Context, _ *config.Backup, runner restic.Runner, target config.Target) (any, error) {
		err := runner.Check(ctx, target, subset)
		return operationResult("check", target.Name, false), err
	})
	checkCommand.Flags().StringVar(&subset, "read-data-subset", "", "optional n/12 repository data subset")
	checkCommand.PreRunE = func(_ *cobra.Command, _ []string) error {
		if subset != "" && !regexp.MustCompile(`^(?:[1-9]|1[0-2])/12$`).MatchString(subset) {
			return &UsageError{Err: fmt.Errorf("invalid --read-data-subset")}
		}
		return nil
	}
	var apply bool
	pruneCommand := operation("prune", func(ctx context.Context, _ *config.Backup, runner restic.Runner, target config.Target) (any, error) {
		reports, err := runner.Prune(ctx, target, apply)
		return struct {
			SchemaVersion int                  `json:"schema_version"`
			Operation     string               `json:"operation"`
			Target        string               `json:"target"`
			Applied       bool                 `json:"applied"`
			Reports       []restic.PruneResult `json:"reports"`
		}{version.SchemaVersion, "prune", target.Name, apply, reports}, err
	})
	pruneCommand.Flags().BoolVar(&apply, "apply", false, "apply deletion; default is Restic dry-run")
	repo.AddCommand(snapshotsCommand, initCommand, checkCommand, unlockCommand, pruneCommand)
	return repo
}

type snapshotSummary struct {
	ID        string          `json:"id"`
	Time      string          `json:"time"`
	Status    snapshot.Status `json:"status"`
	ContentID string          `json:"content_id"`
}

func restoreCommand(configPath *string) *cobra.Command {
	var targetName, snapshotID, output string
	var allowIncomplete bool
	command := &cobra.Command{
		Use: "restore", Short: "Restore and verify a v1 recovery point into an isolated output directory", Args: usageNoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if targetName == "" || output == "" {
				return &UsageError{Err: fmt.Errorf("--target and --output are required")}
			}
			if snapshotID != "" && !validSnapshotID(snapshotID) {
				return &UsageError{Err: fmt.Errorf("--snapshot must be a full 64-character lowercase ID")}
			}
			cfg, err := config.LoadBackup(*configPath)
			if err != nil {
				return err
			}
			target, err := findTarget(cfg, targetName)
			if err != nil {
				return &UsageError{Err: err}
			}
			runner, err := newRunner(cfg)
			if err != nil {
				return err
			}
			if err := runner.VerifyVersion(cmd.Context()); err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), cfg.MaintenanceTimeout.Duration)
			defer cancel()
			result, err := restoretool.Run(ctx, cfg, runner, restoretool.Request{Target: target, SnapshotID: snapshotID, AllowIncomplete: allowIncomplete, Output: output})
			if err != nil {
				return err
			}
			return writeJSON(cmd.OutOrStdout(), result)
		},
	}
	command.Flags().StringVar(&targetName, "target", "", "configured Restic target")
	command.Flags().StringVar(&snapshotID, "snapshot", "", "full snapshot ID; defaults to latest complete")
	command.Flags().StringVar(&output, "output", "", "absolute absent or empty isolated output directory")
	command.Flags().BoolVar(&allowIncomplete, "allow-incomplete", false, "explicitly allow an incomplete recovery point")
	return command
}

func healthcheckCommand() *cobra.Command {
	var url string
	command := &cobra.Command{Use: "healthcheck", Hidden: true, Args: usageNoArgs, RunE: func(_ *cobra.Command, _ []string) error {
		client := &http.Client{Timeout: 2 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
		request, err := http.NewRequest(http.MethodGet, url, nil)
		if err != nil {
			return &UsageError{Err: err}
		}
		response, err := client.Do(request)
		if err != nil {
			return err
		}
		defer func() { _ = response.Body.Close() }()
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		if response.StatusCode != http.StatusNoContent {
			return fmt.Errorf("health endpoint returned %d", response.StatusCode)
		}
		return nil
	}}
	command.Flags().StringVar(&url, "url", "http://127.0.0.1:9090"+worker.HealthPath, "health URL")
	return command
}

func newRunner(cfg *config.Backup) (restic.Runner, error) {
	if len(cfg.Targets) == 0 {
		return restic.Runner{Binary: cfg.ResticBinary, CacheDir: cfg.CacheDir, StagingDir: cfg.StagingDir, Disabled: true}, nil
	}
	credentials, err := config.LoadRoleCredentials(config.RoleCredentialsPath, cfg)
	if err != nil {
		return restic.Runner{}, err
	}
	runner := restic.Runner{
		Binary: cfg.ResticBinary, CacheDir: cfg.CacheDir, StagingDir: cfg.StagingDir,
		PasswordFile: config.ResticPasswordPath, Credentials: credentials, Logger: slog.Default(),
	}
	for _, target := range cfg.Targets {
		if err := runner.ValidateTarget(target); err != nil {
			return restic.Runner{}, fmt.Errorf("target %s credentials: %w", target.Name, err)
		}
	}
	return runner, nil
}

// newWorkerRunner deliberately defers repository material checks until a
// target operation. The resident worker and its health endpoint must remain
// available when a Restic binary, credential mount, password file, or remote
// repository is temporarily unavailable; the target then records a stable
// degraded error instead of preventing serve/run from starting.
func newWorkerRunner(cfg *config.Backup) restic.Runner {
	if len(cfg.Targets) == 0 {
		return restic.Runner{Binary: cfg.ResticBinary, CacheDir: cfg.CacheDir, StagingDir: cfg.StagingDir, Disabled: true}
	}
	var credentials *config.RoleCredentials
	if loaded, err := config.LoadRoleCredentials(config.RoleCredentialsPath, cfg); err == nil {
		credentials = loaded
	}
	return restic.Runner{
		Binary: cfg.ResticBinary, CacheDir: cfg.CacheDir, StagingDir: cfg.StagingDir,
		PasswordFile: config.ResticPasswordPath, Credentials: credentials,
	}
}

func logConfigWarnings(log *slog.Logger, cfg *config.Backup) {
	for _, target := range cfg.Targets {
		for _, warning := range target.Warnings() {
			log.Warn("configuration warning", "target", target.Name, "warning", warning)
		}
	}
}
func targetNames(cfg *config.Backup) []string {
	names := make([]string, 0, len(cfg.Targets))
	for _, target := range cfg.Targets {
		names = append(names, target.Name)
	}
	sort.Strings(names)
	return names
}
func findTarget(cfg *config.Backup, name string) (config.Target, error) {
	for _, target := range cfg.Targets {
		if target.Name == name {
			return target, nil
		}
	}
	return config.Target{}, fmt.Errorf("unknown target %q", name)
}
func writeJSON(writer io.Writer, value any) error {
	encoder := json.NewEncoder(writer)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}
func usageNoArgs(_ *cobra.Command, args []string) error {
	if len(args) != 0 {
		return &UsageError{Err: fmt.Errorf("unexpected positional arguments")}
	}
	return nil
}
func operationResult(operation, target string, applied bool) any {
	return struct {
		SchemaVersion int    `json:"schema_version"`
		Operation     string `json:"operation"`
		Target        string `json:"target"`
		Applied       bool   `json:"applied,omitempty"`
		Success       bool   `json:"success"`
	}{version.SchemaVersion, operation, target, applied, true}
}
func validSnapshotID(value string) bool {
	if len(value) != 64 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
