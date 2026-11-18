package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/hellodword/vaultwarden-less/internal/capture"
	"github.com/hellodword/vaultwarden-less/internal/routes"
	"io"
	"net/http"
	"path/filepath"
	"time"

	"github.com/hellodword/vaultwarden-less/internal/buildinfo"
	"github.com/hellodword/vaultwarden-less/internal/config"
)

type commandError struct {
	message string
	input   bool
}

func (e *commandError) Error() string { return e.message }
func ExitCode(err error) int {
	if errors.Is(err, context.Canceled) {
		return 130
	}
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

type environment struct {
	rulesDir string
	endpoint string
	client   *http.Client
	serve    func(context.Context, string, io.Writer) error
}

func Execute(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	return execute(ctx, args, stdout, stderr, environment{rulesDir: "/rules", endpoint: "http://127.0.0.1:9090", client: &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, serve: serve})
}

func execute(ctx context.Context, args []string, stdout, stderr io.Writer, env environment) error {
	if len(args) == 0 {
		return inputError("A subcommand is required")
	}
	command, args := args[0], args[1:]
	if command == "help" || command == "--help" || command == "-h" {
		if len(args) == 0 {
			return writeHelp(stdout, "")
		}
		if command != "help" || len(args) != 1 {
			return inputError("Unexpected positional arguments")
		}
		return writeHelp(stdout, args[0])
	}
	if !knownCommand(command) {
		return inputError("Unknown command")
	}
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var help bool
	path := config.DefaultBackupConfigPath
	flags.BoolVar(&help, "help", false, "show command help")
	flags.BoolVar(&help, "h", false, "show command help")
	switch command {
	case "serve", "validate":
		flags.StringVar(&path, "config", path, "backup configuration path")
	}
	if err := flags.Parse(args); err != nil {
		return inputError("Invalid command flags")
	}
	if flags.NArg() != 0 {
		return inputError("Unexpected positional arguments")
	}
	if help {
		return writeHelp(stdout, command)
	}
	switch command {
	case "serve":
		return env.serve(ctx, path, stderr)
	case "validate":
		if _, err := routes.Load(filepath.Join(env.rulesDir, "routes.txt")); err != nil {
			return inputError(err.Error())
		}
		if _, err := capture.LoadRule(filepath.Join(env.rulesDir, "personal-vault.sql")); err != nil {
			return inputError(err.Error())
		}
		cfg, err := config.LoadBackup(path)
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
	case "status":
		return call(ctx, env, "/status", http.StatusOK, stdout)
	case "healthcheck":
		if err := call(ctx, env, "/healthz", http.StatusNoContent, io.Discard); err != nil {
			return err
		}
		return json.NewEncoder(stdout).Encode(map[string]bool{"healthy": true})
	case "version":
		return json.NewEncoder(stdout).Encode(buildinfo.Current())
	}
	panic("unhandled command")
}

func knownCommand(command string) bool {
	switch command {
	case "serve", "validate", "status", "healthcheck", "version":
		return true
	}
	return false
}

func writeHelp(out io.Writer, command string) error {
	if command == "" {
		_, err := io.WriteString(out, `Back up a personal Vaultwarden database.

Usage: vaultwarden-less-backup <command> [flags]

Commands:
  serve        Run the backup worker
  validate     Validate local configuration without contacting the worker
  status       Show worker status
  healthcheck  Check worker liveness
  version      Show build information

Use "vaultwarden-less-backup help <command>" for command flags.
`)
		return err
	}
	if !knownCommand(command) {
		return inputError("Unknown command")
	}
	usage := ""
	switch command {
	case "serve", "validate":
		usage = fmt.Sprintf("  --config PATH  Backup configuration path (default %s)\n", config.DefaultBackupConfigPath)
	}
	_, err := fmt.Fprintf(out, "Usage: vaultwarden-less-backup %s [flags]\n\nFlags:\n%s  -h, --help     Show command help\n", command, usage)
	return err
}

func call(ctx context.Context, env environment, path string, status int, out io.Writer) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, env.endpoint+path, nil)
	if err != nil {
		return &commandError{message: "Invalid worker address"}
	}
	response, err := env.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return &commandError{message: "Backup process unavailable"}
	}
	defer response.Body.Close()
	if response.StatusCode != status {
		if response.StatusCode == http.StatusBadRequest {
			return inputError("Worker rejected the operation input")
		}
		return &commandError{message: "Worker response unavailable"}
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
