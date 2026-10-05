package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestCommandsExitCodesAndLocalValidation(t *testing.T) {
	var full bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/checks" {
			var body struct {
				Full bool `json:"full"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			full = body.Full
			w.WriteHeader(202)
			io.WriteString(w, `{"accepted":true}`)
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
	for _, args := range [][]string{{"version"}, {"healthcheck"}, {"status"}, {"check", "--full"}, {"serve"}} {
		var out, diagnostic bytes.Buffer
		root := newCommand(&out, &diagnostic, env)
		if err := Execute(root, args); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
	}
	if !full || !served {
		t.Fatal("commands were gated or lost full flag")
	}
	for _, args := range [][]string{{}, {"unknown"}, {"check", "extra"}, {"check", "--unknown"}} {
		root := newCommand(io.Discard, io.Discard, env)
		if code := ExitCode(Execute(root, args)); code != 2 {
			t.Fatalf("input %v exit=%d", args, code)
		}
	}
	path := filepath.Join(t.TempDir(), "backup.yaml")
	if err := os.WriteFile(path, []byte("targets: []\nnotifiers: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	server.Close()
	root := newCommand(io.Discard, io.Discard, env)
	if err := Execute(root, []string{"validate", "--config", path}); err != nil {
		t.Fatal("local validate contacted network")
	}
	root = newCommand(io.Discard, io.Discard, env)
	if code := ExitCode(Execute(root, []string{"status"})); code != 1 {
		t.Fatal("runtime failure exit code")
	}
}
