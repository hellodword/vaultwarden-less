package e2e_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type bitwardenSendReceiver struct {
	t       *testing.T
	path    string
	appdata string
	home    string
	temp    string
	caFile  string
}

func (client *bitwardenCLI) createFileSend(ctx context.Context, path, name string) string {
	return client.createFileSendWithLimit(ctx, path, name, 0)
}

func (client *bitwardenCLI) createFileSendWithLimit(ctx context.Context, path, name string, maximumAccesses int) string {
	arguments := []string{"--raw", "send", "--file", path, "--name", name, "--fullObject"}
	if maximumAccesses > 0 {
		arguments = append(arguments, "--maxAccessCount", fmt.Sprintf("%d", maximumAccesses))
	}
	object := decodeJSONObject(client.t, client.run(ctx, nil, arguments...), "file Send")
	if maximumAccesses > 0 && integerValue(object["maxAccessCount"]) != maximumAccesses {
		client.t.Fatalf("created file Send maxAccessCount = %#v", object["maxAccessCount"])
	}
	id := stringValue(object["id"])
	if id == "" {
		client.t.Fatal("created file Send has no ID")
	}
	return id
}

func (client *bitwardenCLI) waitFileSendAccessCount(
	ctx context.Context, id string, want int, timeout time.Duration,
) {
	client.t.Helper()
	deadline := time.Now().Add(timeout)
	var observed int
	for time.Now().Before(deadline) {
		client.sync(ctx)
		object := decodeJSONObject(
			client.t,
			client.run(ctx, nil, "--raw", "send", "get", id),
			"file Send access count",
		)
		observed = integerValue(object["accessCount"])
		if observed == want {
			return
		}
		select {
		case <-ctx.Done():
			client.t.Fatal(ctx.Err())
		case <-time.After(500 * time.Millisecond):
		}
	}
	client.t.Fatalf("file Send accessCount = %d, want %d", observed, want)
}

func (client *bitwardenCLI) getFileSend(ctx context.Context, id, output string) {
	sendURL := client.fileSendURL(ctx, id)
	receiver := newBitwardenSendReceiver(client.t, ctx, client.server, client.caFile)
	stdout, diagnostics, err := receiver.tryReceive(ctx, sendURL, output, false)
	if err != nil {
		diagnostics = string(stdout) + "\n" + diagnostics
		diagnostics = strings.ReplaceAll(diagnostics, sendURL, "<redacted-send-url>")
		client.t.Fatalf("Bitwarden CLI file Send receive failed: %v: %s", err, client.redact(diagnostics))
	}
}

func (client *bitwardenCLI) expectFileSendAccessFailure(ctx context.Context, id string) {
	client.t.Helper()
	sendURL := client.fileSendURL(ctx, id)
	receiver := newBitwardenSendReceiver(client.t, ctx, client.server, client.caFile)
	output := filepath.Join(receiver.temp, "unexpected-send-output")
	stdout, diagnostics, err := receiver.tryReceive(ctx, sendURL, output, true)
	if err == nil {
		client.t.Fatal("a fresh recipient downloaded the access-limited Send after its limit")
	}
	if _, statError := os.Lstat(output); !os.IsNotExist(statError) {
		client.t.Fatalf("failed access published an output file: %v", statError)
	}
	allDiagnostics := string(stdout) + "\n" + diagnostics
	if containsSecret(allDiagnostics, client.password, client.session) {
		client.t.Fatal("Send failure diagnostics exposed a password")
	}
	var response struct {
		Success bool   `json:"success"`
		Message string `json:"message"`
	}
	if json.Unmarshal(bytes.TrimSpace(stdout), &response) != nil ||
		response.Success || response.Message != "Not found." {
		allDiagnostics = strings.ReplaceAll(
			client.redact(allDiagnostics), sendURL, "<redacted-send-url>",
		)
		client.t.Fatalf("fresh recipient failed for an unexpected reason: %s", allDiagnostics)
	}
}

func newBitwardenSendReceiver(
	t *testing.T, ctx context.Context, server, caFile string,
) *bitwardenSendReceiver {
	t.Helper()
	path, err := exec.LookPath("bw")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	receiver := &bitwardenSendReceiver{
		t:       t,
		path:    path,
		appdata: filepath.Join(root, "appdata"),
		home:    filepath.Join(root, "home"),
		temp:    filepath.Join(root, "tmp"),
		caFile:  caFile,
	}
	for _, directory := range []string{receiver.appdata, receiver.home, receiver.temp} {
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	command := exec.CommandContext(
		ctx,
		receiver.path,
		"--nointeraction",
		"config",
		"server",
		server,
	)
	command.Env = cliEnvironment(
		receiver.home,
		receiver.temp,
		receiver.appdata,
		receiver.caFile,
		"",
	)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf(
			"configure isolated Send recipient: %v: %s",
			err,
			redactDiagnostics(string(output), server),
		)
	}
	return receiver
}

func (receiver *bitwardenSendReceiver) tryReceive(
	ctx context.Context, sendURL, output string, structuredResponse bool,
) ([]byte, string, error) {
	receiver.t.Helper()
	arguments := []string{"--nointeraction"}
	if structuredResponse {
		arguments = append(arguments, "--response")
	}
	arguments = append(arguments, "send", "receive", sendURL, "--output", output)
	command := exec.CommandContext(
		ctx,
		receiver.path,
		arguments...,
	)
	command.Env = cliEnvironment(
		receiver.home,
		receiver.temp,
		receiver.appdata,
		receiver.caFile,
		"",
	)
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err := command.Run()
	return stdout.Bytes(), stderr.String(), err
}

func (client *bitwardenCLI) fileSendURL(ctx context.Context, id string) string {
	client.t.Helper()
	var send struct {
		AccessURL string         `json:"accessUrl"`
		File      map[string]any `json:"file"`
	}
	if err := json.Unmarshal(client.run(ctx, nil, "--raw", "send", "get", id), &send); err != nil || send.AccessURL == "" || send.File == nil {
		client.t.Fatal("Bitwarden CLI returned an invalid file Send")
	}
	accessURL, err := url.Parse(send.AccessURL)
	serverURL, serverErr := url.Parse(client.server)
	if err != nil || serverErr != nil || accessURL.Scheme != serverURL.Scheme || accessURL.Host != serverURL.Host || accessURL.User != nil || accessURL.Fragment == "" {
		client.t.Fatal("Bitwarden CLI returned an unsafe file Send URL")
	}
	return send.AccessURL
}
