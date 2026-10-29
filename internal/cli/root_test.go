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
	var force bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/backups" {
			var body struct {
				Force bool `json:"force"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			force = body.Force
			w.WriteHeader(202)
			io.WriteString(w, `{"id":"0123456789abcdef0123456789abcdef","state":"succeeded","targets":[]}`)
			return
		}
		if r.URL.Path == "/healthz" {
			w.WriteHeader(204)
			return
		}
		io.WriteString(w, `{"source":"failed"}`)
	}))
	defer server.Close()
	served := false
	env := environment{endpoint: server.URL, client: server.Client(), serve: func(context.Context, string, io.Writer) error { served = true; return nil }}
	for _, args := range [][]string{{"version"}, {"healthcheck"}, {"status"}, {"backup", "--force"}, {"serve"}} {
		var out, diagnostic bytes.Buffer
		if err := execute(t.Context(), args, &out, &diagnostic, env); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
	}
	if !force || !served {
		t.Fatal("commands were gated or lost force flag")
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
	for _, args := range [][]string{{"help"}, {"-h"}, {"--help"}, {"help", "serve"}, {"validate", "--help"}, {"backup", "-h"}, {"status", "--help"}, {"healthcheck", "--help"}, {"version", "--help"}} {
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

func TestCheckBooleanFormsAndCancellation(t *testing.T) {
	var received []bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Force bool }
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		received = append(received, body.Force)
		w.WriteHeader(http.StatusAccepted)
		io.WriteString(w, `{"id":"0123456789abcdef0123456789abcdef","state":"succeeded","targets":[]}`)
	}))
	defer server.Close()
	env := environment{endpoint: server.URL, client: server.Client()}
	for _, args := range [][]string{{"backup"}, {"backup", "--force"}, {"backup", "--force=false"}, {"backup", "--force=true"}} {
		if err := execute(t.Context(), args, io.Discard, io.Discard, env); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := execute(ctx, []string{"backup"}, io.Discard, io.Discard, env); ExitCode(err) != 130 {
		t.Fatalf("cancelled request: %v", err)
	}
	if len(received) != 4 || received[0] || !received[1] || received[2] || !received[3] {
		t.Fatalf("requests: %v", received)
	}
}

func TestWaitForActualOperationFailureAndMissingReceipt(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(map[bool]string{false: "failed", true: "lost"}[missing], func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					w.WriteHeader(http.StatusAccepted)
					io.WriteString(w, `{"id":"0123456789abcdef0123456789abcdef","state":"queued"}`)
					return
				}
				if missing {
					w.WriteHeader(http.StatusNotFound)
					io.WriteString(w, `{"error":{"code":"operation_unknown"}}`)
					return
				}
				io.WriteString(w, `{"id":"0123456789abcdef0123456789abcdef","state":"failed","targets":[{"name":"one","state":"deferred","retry_at":"2026-10-29T00:00:00Z"}]}`)
			}))
			defer server.Close()
			var stdout, stderr bytes.Buffer
			err := execute(t.Context(), []string{"backup"}, &stdout, &stderr, environment{endpoint: server.URL, client: server.Client()})
			if ExitCode(err) != 1 {
				t.Fatal("acceptance was treated as completion")
			}
			if missing {
				if stdout.Len() != 0 || !strings.Contains(err.Error(), "cannot be confirmed") {
					t.Fatal("lost receipt was not explicit")
				}
			} else if !json.Valid(stdout.Bytes()) || !strings.Contains(stdout.String(), "deferred") {
				t.Fatal("target outcomes not on stdout")
			}
		})
	}
}
func TestInterruptOnlyStopsWaiting(t *testing.T) {
	accepted := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		io.WriteString(w, `{"id":"0123456789abcdef0123456789abcdef","state":"running"}`)
		close(accepted)
	}))
	defer server.Close()
	go func() { <-accepted; cancel() }()
	if code := ExitCode(execute(ctx, []string{"backup"}, io.Discard, io.Discard, environment{endpoint: server.URL, client: server.Client()})); code != 130 {
		t.Fatalf("interrupted wait exit %d", code)
	}
}
