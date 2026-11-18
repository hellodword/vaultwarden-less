package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCommandsExitCodesAndLocalValidation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.WriteHeader(204)
			return
		}
		io.WriteString(w, `{"source":"failed"}`)
	}))
	defer server.Close()
	served := false
	env := environment{rulesDir: "../../rules", endpoint: server.URL, client: server.Client(), serve: func(context.Context, string, io.Writer) error { served = true; return nil }}
	for _, args := range [][]string{{"version"}, {"healthcheck"}, {"status"}, {"serve"}} {
		var out, diagnostic bytes.Buffer
		if err := execute(t.Context(), args, &out, &diagnostic, env); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
	}
	if !served {
		t.Fatal("serve did not run")
	}
	for _, args := range [][]string{{}, {"unknown"}, {"backup", "extra"}, {"backup", "--unknown"}, {"backup", "--force=invalid"}, {"serve", "--config"}, {"status", "--config=x"}, {"help", "unknown"}, {"help", "backup", "extra"}, {"version", "--", "extra"}} {
		var out, diagnostic bytes.Buffer
		err := execute(t.Context(), args, &out, &diagnostic, env)
		if code := ExitCode(err); code != 2 {
			t.Fatalf("input %v exit=%d", args, code)
		}
		WriteError(&diagnostic, err)
		if out.Len() != 0 || !json.Valid(diagnostic.Bytes()) || !strings.Contains(diagnostic.String(), `"code":"input_error"`) {
			t.Fatalf("input %v: stdout=%q stderr=%q", args, &out, &diagnostic)
		}
	}
	path := filepath.Join(t.TempDir(), "backup.yaml")
	if err := os.WriteFile(path, []byte("targets: []\nnotifications: {channels: []}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	server.Close()
	if err := execute(t.Context(), []string{"validate", "--config", path}, io.Discard, io.Discard, env); err != nil {
		t.Fatal("local validate contacted network")
	}
	if code := ExitCode(execute(t.Context(), []string{"status"}, io.Discard, io.Discard, env)); code != 1 {
		t.Fatal("runtime failure exit code")
	}
}

func TestHelpDoesNotRunCommands(t *testing.T) {
	for _, args := range [][]string{{"help"}, {"-h"}, {"--help"}, {"help", "serve"}, {"validate", "--help"}, {"status", "--help"}, {"healthcheck", "--help"}, {"version", "--help"}} {
		var out, diagnostic bytes.Buffer
		if err := execute(t.Context(), args, &out, &diagnostic, environment{}); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		if !strings.Contains(out.String(), "Usage:") || diagnostic.Len() != 0 {
			t.Fatalf("%v: stdout=%q stderr=%q", args, &out, &diagnostic)
		}
	}
}

func TestServePreservesContextPathAndDiagnostics(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	var diagnostic bytes.Buffer
	env := environment{serve: func(got context.Context, path string, out io.Writer) error {
		if got != ctx || path != "/config/path with spaces.yaml" || out != &diagnostic {
			t.Fatalf("serve received wrong context, path or diagnostic stream: %q", path)
		}
		return got.Err()
	}}
	err := execute(ctx, []string{"serve", "--config=/config/path with spaces.yaml"}, io.Discard, &diagnostic, env)
	if !errors.Is(err, context.Canceled) || ExitCode(err) != 130 {
		t.Fatalf("cancellation: %v", err)
	}
	WriteError(&diagnostic, err)
	if diagnostic.String() != "{\"error\":{\"code\":\"operation_failed\",\"message\":\"The operation failed\"}}\n" {
		t.Fatalf("unexpected diagnostic: %q", &diagnostic)
	}
}
