package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hellodword/vaultwarden-less/internal/version"
)

func TestProxyCLIHelpVersionAndUsage(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		arguments []string
		code      int
		stdout    string
		stderr    bool
	}{
		{[]string{"--help"}, 0, "serve|healthcheck", false},
		{[]string{"serve", "--help"}, 0, "vaultwarden-less-proxy serve", false},
		{[]string{"healthcheck", "--help"}, 0, "vaultwarden-less-proxy healthcheck", false},
		{[]string{"--version"}, 0, "vaultwarden-less-proxy " + version.Product + "\n", false},
		{nil, 2, "", true},
		{[]string{"unknown"}, 2, "", true},
		{[]string{"serve", "extra"}, 2, "", true},
		{[]string{"healthcheck", "extra"}, 2, "", true},
	} {
		var stdout, stderr bytes.Buffer
		code := execute(context.Background(), test.arguments, &stdout, &stderr)
		if code != test.code {
			t.Errorf("%#v code = %d, want %d", test.arguments, code, test.code)
		}
		if test.stdout != "" && !strings.Contains(stdout.String(), test.stdout) || test.stdout == "" && stdout.Len() != 0 {
			t.Errorf("%#v stdout = %q", test.arguments, stdout.String())
		}
		if test.stderr != (stderr.Len() > 0) {
			t.Errorf("%#v stderr = %q", test.arguments, stderr.String())
		}
	}
}

func TestProxyCLIHealthcheck(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	var stdout, stderr bytes.Buffer
	code := execute(context.Background(), []string{"healthcheck", "--url", server.URL}, &stdout, &stderr)
	if code != 0 || stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("healthcheck = %d/%q/%q", code, stdout.String(), stderr.String())
	}
}

func TestProxyCLIHealthcheckFailureHasNoStdout(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	var stdout, stderr bytes.Buffer
	code := execute(context.Background(), []string{"healthcheck", "--url", server.URL}, &stdout, &stderr)
	if code != 1 || stdout.Len() != 0 || stderr.Len() == 0 {
		t.Fatalf("healthcheck failure = %d/%q/%q", code, stdout.String(), stderr.String())
	}
}
