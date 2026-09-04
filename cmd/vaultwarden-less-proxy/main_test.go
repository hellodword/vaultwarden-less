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

func TestProxyCLIVersionAndUsageAreSemantic(t *testing.T) {
	setProxyBuildIdentity(t, "", "development", "false")
	environment := proxyEnvironment{ready: func() bool { return true }, healthURL: "http://127.0.0.1:1", serve: func(context.Context, io.Writer) error { return nil }}
	var stdout, stderr bytes.Buffer
	if code := executeWithEnvironment(context.Background(), []string{"version"}, &stdout, &stderr, environment); code != 0 || stderr.Len() != 0 {
		t.Fatalf("development version = %d/%q/%q", code, stdout.String(), stderr.String())
	}
	var version struct {
		ProjectCommit *string `json:"project_commit"`
		Release       string  `json:"release"`
		Clean         bool    `json:"clean"`
	}
	decodeSingleProxyJSON(t, stdout.String(), &version)
	if version.ProjectCommit != nil || version.Release != "development" || version.Clean {
		t.Fatalf("development version = %#v", version)
	}

	for _, arguments := range [][]string{nil, {"unknown"}, {"serve", "extra"}, {"healthcheck", "extra"}, {"--version"}} {
		stdout.Reset()
		stderr.Reset()
		code := executeWithEnvironment(context.Background(), arguments, &stdout, &stderr, environment)
		if code != 2 || stdout.Len() != 0 {
			t.Errorf("%#v = %d/%q/%q", arguments, code, stdout.String(), stderr.String())
		}
		assertProxyError(t, stderr.String(), "usage_error")
	}
}

func TestProxyCLICleanVersionIsSemanticJSON(t *testing.T) {
	commit := strings.Repeat("b", 40)
	setProxyBuildIdentity(t, commit, "git-"+commit, "true")
	var stdout, stderr bytes.Buffer
	code := executeWithEnvironment(context.Background(), []string{"version"}, &stdout, &stderr, proxyEnvironment{})
	if code != 0 || stderr.Len() != 0 {
		t.Fatalf("version = %d/%q/%q", code, stdout.String(), stderr.String())
	}
	var version struct {
		ProjectCommit *string `json:"project_commit"`
		Release       string  `json:"release"`
		Clean         bool    `json:"clean"`
	}
	decodeSingleProxyJSON(t, stdout.String(), &version)
	if version.ProjectCommit == nil || *version.ProjectCommit != commit || version.Release != "git-"+commit || !version.Clean {
		t.Fatalf("clean version = %#v", version)
	}
}

func TestProxyCLIHealthcheckIsSemanticAndReportsFailure(t *testing.T) {
	for _, status := range []int{http.StatusNoContent, http.StatusServiceUnavailable} {
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			writer.WriteHeader(status)
		}))
		var stdout, stderr bytes.Buffer
		code := executeWithEnvironment(context.Background(), []string{"healthcheck"}, &stdout, &stderr, proxyEnvironment{healthURL: server.URL})
		server.Close()
		if status == http.StatusNoContent {
			var result struct {
				Healthy bool `json:"healthy"`
			}
			decodeSingleProxyJSON(t, stdout.String(), &result)
			if code != 0 || !result.Healthy || stderr.Len() != 0 {
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

func assertProxyError(t *testing.T, output, code string) {
	t.Helper()
	decoder := json.NewDecoder(strings.NewReader(output))
	var document struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := decoder.Decode(&document); err != nil || document.Error.Code != code || document.Error.Message == "" {
		t.Fatalf("error document = %#v/%v", document, err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		t.Fatalf("second error document in %q", output)
	}
}

func decodeSingleProxyJSON(t *testing.T, input string, destination any) {
	t.Helper()
	decoder := json.NewDecoder(strings.NewReader(input))
	if err := decoder.Decode(destination); err != nil {
		t.Fatalf("decode JSON: %v", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		t.Fatalf("JSON contains a second value")
	}
}

func setProxyBuildIdentity(t *testing.T, commit, release, clean string) {
	t.Helper()
	oldCommit, oldRelease, oldClean := buildinfo.ProjectCommit, buildinfo.Release, buildinfo.Clean
	buildinfo.ProjectCommit, buildinfo.Release, buildinfo.Clean = commit, release, clean
	t.Cleanup(func() { buildinfo.ProjectCommit, buildinfo.Release, buildinfo.Clean = oldCommit, oldRelease, oldClean })
}
