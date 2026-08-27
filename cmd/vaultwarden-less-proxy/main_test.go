package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hellodword/vaultwarden-less/internal/buildinfo"
)

func TestProxyCLIHelpVersionAndUsageGolden(t *testing.T) {
	setProxyBuildIdentity(t, "", "development", "false")
	environment := proxyEnvironment{ready: func() bool { return true }, healthURL: "http://127.0.0.1:1", serve: func(context.Context, io.Writer) error { return nil }}
	for _, test := range []struct {
		arguments []string
		code      int
		stdout    string
		errorCode string
	}{
		{[]string{"--help"}, 0, "serve|healthcheck|version", ""},
		{[]string{"serve", "--help"}, 0, "vaultwarden-less-proxy serve", ""},
		{[]string{"healthcheck", "--help"}, 0, "vaultwarden-less-proxy healthcheck", ""},
		{[]string{"version"}, 0, "{\"schema_version\":1,\"project_commit\":null,\"release\":\"development\",\"clean\":false}\n", ""},
		{nil, 2, "", "usage_error"},
		{[]string{"unknown"}, 2, "", "usage_error"},
		{[]string{"serve", "extra"}, 2, "", "usage_error"},
		{[]string{"healthcheck", "extra"}, 2, "", "usage_error"},
		{[]string{"--version"}, 2, "", "usage_error"},
	} {
		var stdout, stderr bytes.Buffer
		code := executeWithEnvironment(context.Background(), test.arguments, &stdout, &stderr, environment)
		if code != test.code {
			t.Errorf("%#v code = %d, want %d", test.arguments, code, test.code)
		}
		if test.stdout != "" && !strings.Contains(stdout.String(), test.stdout) || test.stdout == "" && stdout.Len() != 0 {
			t.Errorf("%#v stdout = %q", test.arguments, stdout.String())
		}
		if test.errorCode == "" && stderr.Len() != 0 {
			t.Errorf("%#v stderr = %q", test.arguments, stderr.String())
		}
		if test.errorCode != "" {
			assertProxyError(t, stderr.String(), test.errorCode)
		}
	}
}

func TestProxyCLICleanVersionGolden(t *testing.T) {
	commit := strings.Repeat("b", 40)
	setProxyBuildIdentity(t, commit, "RELEASE.2026-08-27T12-34-56Z-gbbbbbbbbbbbb", "true")
	var stdout, stderr bytes.Buffer
	code := executeWithEnvironment(context.Background(), []string{"version"}, &stdout, &stderr, proxyEnvironment{})
	want := `{"schema_version":1,"project_commit":"` + commit + `","release":"RELEASE.2026-08-27T12-34-56Z-gbbbbbbbbbbbb","clean":true}` + "\n"
	if code != 0 || stdout.String() != want || stderr.Len() != 0 {
		t.Fatalf("version = %d/%q/%q", code, stdout.String(), stderr.String())
	}
}

func TestProxyCLIHealthcheckGoldenAndFailure(t *testing.T) {
	for _, status := range []int{http.StatusNoContent, http.StatusServiceUnavailable} {
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			writer.WriteHeader(status)
		}))
		var stdout, stderr bytes.Buffer
		code := executeWithEnvironment(context.Background(), []string{"healthcheck"}, &stdout, &stderr, proxyEnvironment{healthURL: server.URL})
		server.Close()
		if status == http.StatusNoContent {
			if code != 0 || stdout.String() != "{\"schema_version\":1,\"healthy\":true}\n" || stderr.Len() != 0 {
				t.Fatalf("healthcheck success = %d/%q/%q", code, stdout.String(), stderr.String())
			}
		} else {
			if code != 1 || stdout.Len() != 0 {
				t.Fatalf("healthcheck failure = %d/%q/%q", code, stdout.String(), stderr.String())
			}
			assertProxyError(t, stderr.String(), "not_ready")
		}
	}
}

func TestProxyDirtyServeRefusesBeforeSideEffects(t *testing.T) {
	called := false
	environment := proxyEnvironment{
		ready: func() bool { return false },
		serve: func(context.Context, io.Writer) error { called = true; return nil },
	}
	var stdout, stderr bytes.Buffer
	code := executeWithEnvironment(context.Background(), []string{"serve"}, &stdout, &stderr, environment)
	if code != 1 || stdout.Len() != 0 || called {
		t.Fatalf("dirty serve = %d/%q/%q/called=%t", code, stdout.String(), stderr.String(), called)
	}
	assertProxyError(t, stderr.String(), "not_ready")
}

func TestProxyUpgradeGateRefusesBeforeServing(t *testing.T) {
	called := false
	environment := proxyEnvironment{
		ready:      func() bool { return true },
		allowServe: func() error { return errors.New("pending acceptance") },
		serve:      func(context.Context, io.Writer) error { called = true; return nil },
	}
	var stdout, stderr bytes.Buffer
	code := executeWithEnvironment(context.Background(), []string{"serve"}, &stdout, &stderr, environment)
	if code != 1 || stdout.Len() != 0 || called {
		t.Fatalf("upgrade-gated serve = %d/%q/%q/called=%t", code, stdout.String(), stderr.String(), called)
	}
	assertProxyError(t, stderr.String(), "not_ready")
}

func assertProxyError(t *testing.T, output, code string) {
	t.Helper()
	decoder := json.NewDecoder(strings.NewReader(output))
	var document struct {
		SchemaVersion int `json:"schema_version"`
		Error         struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := decoder.Decode(&document); err != nil || document.SchemaVersion != 1 || document.Error.Code != code || document.Error.Message == "" {
		t.Fatalf("error document = %#v/%v", document, err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		t.Fatalf("second error document in %q", output)
	}
}

func setProxyBuildIdentity(t *testing.T, commit, release, clean string) {
	t.Helper()
	oldCommit, oldRelease, oldClean := buildinfo.ProjectCommit, buildinfo.Release, buildinfo.Clean
	buildinfo.ProjectCommit, buildinfo.Release, buildinfo.Clean = commit, release, clean
	t.Cleanup(func() { buildinfo.ProjectCommit, buildinfo.Release, buildinfo.Clean = oldCommit, oldRelease, oldClean })
}
