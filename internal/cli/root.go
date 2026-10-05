package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/hellodword/vaultwarden-less/internal/buildinfo"
	"github.com/hellodword/vaultwarden-less/internal/config"
	"github.com/spf13/cobra"
)

type commandError struct {
	message string
	input   bool
}

func (e *commandError) Error() string { return e.message }
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	var e *commandError
	if errors.As(err, &e) && e.input {
		return 2
	}
	return 1
}
func WriteError(out io.Writer, err error) {
	if err == nil {
		return
	}
	message := "The operation failed"
	var e *commandError
	if errors.As(err, &e) {
		message = e.message
	}
	json.NewEncoder(out).Encode(map[string]any{"error": map[string]string{"message": message, "code": map[bool]string{true: "input_error", false: "operation_failed"}[ExitCode(err) == 2]}})
}
func inputError(message string) error { return &commandError{message: message, input: true} }
func noArguments(_ *cobra.Command, args []string) error {
	if len(args) > 0 {
		return inputError("Unexpected positional arguments")
	}
	return nil
}

type environment struct {
	endpoint string
	client   *http.Client
	serve    func(context.Context, string, io.Writer) error
}

func New(stdout, stderr io.Writer) *cobra.Command {
	return newCommand(stdout, stderr, environment{endpoint: "http://127.0.0.1:9090", client: &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, serve: serve})
}
func Execute(cmd *cobra.Command, args []string) error {
	cmd.SetArgs(args)
	if _, _, err := cmd.Find(args); err != nil {
		return inputError("Unknown command")
	}
	return cmd.Execute()
}

func newCommand(stdout, stderr io.Writer, env environment) *cobra.Command {
	root := &cobra.Command{Use: "vaultwarden-less-backup", Short: "Check and back up native Vaultwarden data", SilenceErrors: true, SilenceUsage: true, Args: noArguments, RunE: func(*cobra.Command, []string) error { return inputError("A subcommand is required") }}
	root.SetOut(stdout)
	root.SetErr(stderr)
	root.SetFlagErrorFunc(func(*cobra.Command, error) error { return inputError("Invalid command flags") })
	root.CompletionOptions.DisableDefaultCmd = true
	servePath := config.DefaultBackupConfigPath
	serveCmd := &cobra.Command{Use: "serve", Args: noArguments, RunE: func(cmd *cobra.Command, _ []string) error { return env.serve(cmd.Context(), servePath, stderr) }}
	serveCmd.Flags().StringVar(&servePath, "config", servePath, "backup configuration path")
	validatePath := config.DefaultBackupConfigPath
	validate := &cobra.Command{Use: "validate", Args: noArguments, RunE: func(cmd *cobra.Command, _ []string) error {
		cfg, err := config.LoadBackup(validatePath)
		if err != nil {
			return inputError("Invalid local configuration")
		}
		if err := json.NewEncoder(stdout).Encode(cfg); err != nil {
			return err
		}
		if !cfg.Valid() {
			return inputError("One or more configuration entries are invalid")
		}
		return nil
	}}
	validate.Flags().StringVar(&validatePath, "config", validatePath, "backup configuration path")
	full := false
	check := &cobra.Command{Use: "check", Args: noArguments, RunE: func(cmd *cobra.Command, _ []string) error {
		return call(cmd.Context(), env, http.MethodPost, "/checks", map[string]bool{"full": full}, http.StatusAccepted, stdout)
	}}
	check.Flags().BoolVar(&full, "full", false, "compare all database content")
	status := &cobra.Command{Use: "status", Args: noArguments, RunE: func(cmd *cobra.Command, _ []string) error {
		return call(cmd.Context(), env, http.MethodGet, "/status", nil, http.StatusOK, stdout)
	}}
	health := &cobra.Command{Use: "healthcheck", Args: noArguments, RunE: func(cmd *cobra.Command, _ []string) error {
		if err := call(cmd.Context(), env, http.MethodGet, "/healthz", nil, http.StatusNoContent, io.Discard); err != nil {
			return err
		}
		return json.NewEncoder(stdout).Encode(map[string]bool{"healthy": true})
	}}
	version := &cobra.Command{Use: "version", Args: noArguments, RunE: func(*cobra.Command, []string) error { return json.NewEncoder(stdout).Encode(buildinfo.Current()) }}
	root.AddCommand(serveCmd, validate, check, status, health, version)
	return root
}

func call(ctx context.Context, env environment, method, path string, body any, status int, out io.Writer) error {
	var input io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		input = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, env.endpoint+path, input)
	if err != nil {
		return &commandError{message: "Invalid worker address"}
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	response, err := env.client.Do(req)
	if err != nil {
		return &commandError{message: "Backup process unavailable"}
	}
	defer response.Body.Close()
	if response.StatusCode != status {
		return &commandError{message: "Backup process did not accept the request"}
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil || len(data) > 1<<20 {
		return &commandError{message: "Invalid worker response"}
	}
	if status != http.StatusNoContent && !json.Valid(data) {
		return &commandError{message: "Invalid worker response"}
	}
	_, err = out.Write(data)
	return err
}
