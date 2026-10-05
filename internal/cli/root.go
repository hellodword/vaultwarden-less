package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
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
	endpoint string
	client   *http.Client
	serve    func(context.Context, string, io.Writer) error
}

func Execute(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	return execute(ctx, args, stdout, stderr, environment{endpoint: "http://127.0.0.1:9090", client: &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, serve: serve})
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
	var help, full bool
	path := config.DefaultBackupConfigPath
	flags.BoolVar(&help, "help", false, "show command help")
	flags.BoolVar(&help, "h", false, "show command help")
	switch command {
	case "serve", "validate":
		flags.StringVar(&path, "config", path, "backup configuration path")
	case "check":
		flags.BoolVar(&full, "full", false, "compare all database content")
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
	case "check":
		return call(ctx, env, http.MethodPost, "/checks", map[string]bool{"full": full}, http.StatusAccepted, stdout)
	case "status":
		return call(ctx, env, http.MethodGet, "/status", nil, http.StatusOK, stdout)
	case "healthcheck":
		if err := call(ctx, env, http.MethodGet, "/healthz", nil, http.StatusNoContent, io.Discard); err != nil {
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
	case "serve", "validate", "check", "status", "healthcheck", "version":
		return true
	}
	return false
}

func writeHelp(out io.Writer, command string) error {
	if command == "" {
		_, err := io.WriteString(out, `Check and back up native Vaultwarden data.

Usage: vaultwarden-less-backup <command> [flags]

Commands:
  serve        Run the backup worker
  validate     Validate local configuration without contacting the worker
  check        Request a backup check
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
	case "check":
		usage = "  --full         Compare all database content (default false)\n"
	}
	_, err := fmt.Fprintf(out, "Usage: vaultwarden-less-backup %s [flags]\n\nFlags:\n%s  -h, --help     Show command help\n", command, usage)
	return err
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
