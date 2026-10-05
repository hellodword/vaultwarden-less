package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCommandsReportExitCodesAndSeparateStreams(t *testing.T) {
	health := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	defer health.Close()
	environment := proxyEnvironment{healthURL: health.URL, serve: func(context.Context, io.Writer) error { return nil }}
	for _, test := range []struct {
		args []string
		code int
	}{
		{[]string{"version"}, 0}, {[]string{"healthcheck"}, 0}, {[]string{"serve"}, 0},
		{nil, 2}, {[]string{"unknown"}, 2}, {[]string{"serve", "--unexpected"}, 2}, {[]string{"version", "extra"}, 2},
	} {
		var out, diagnostic bytes.Buffer
		code := executeWithEnvironment(context.Background(), test.args, &out, &diagnostic, environment)
		if code != test.code {
			t.Fatalf("%v: exit %d", test.args, code)
		}
		if code == 0 && diagnostic.Len() != 0 {
			t.Fatal("successful command emitted an error")
		}
		if code != 0 && (out.Len() != 0 || !json.Valid(diagnostic.Bytes())) {
			t.Fatal("input error polluted stdout or lost structured diagnostic")
		}
	}
	environment.serve = func(context.Context, io.Writer) error { return errors.New("sensitive internal detail") }
	var out, diagnostic bytes.Buffer
	if executeWithEnvironment(context.Background(), []string{"serve"}, &out, &diagnostic, environment) != 1 {
		t.Fatal("serve failure exit code")
	}
	if out.Len() != 0 || bytes.Contains(diagnostic.Bytes(), []byte("sensitive")) {
		t.Fatal("unsafe serve diagnostic")
	}
	health.Close()
	if executeWithEnvironment(context.Background(), []string{"healthcheck"}, io.Discard, io.Discard, environment) != 1 {
		t.Fatal("unresponsive process reported healthy")
	}
}
