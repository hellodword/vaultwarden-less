//go:build systemtest

package e2e_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const cliPasswordEnvironment = "VWLESS_BW_TEST_PASSWORD"

type bitwardenCLI struct {
	t        *testing.T
	path     string
	appdata  string
	server   string
	email    string
	password string
	session  string
}

func newBitwardenCLI(t *testing.T, appdata, server, email, password string) *bitwardenCLI {
	t.Helper()
	path, err := exec.LookPath("bw")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(appdata, 0o700); err != nil {
		t.Fatal(err)
	}
	return &bitwardenCLI{t: t, path: path, appdata: appdata, server: server, email: email, password: password}
}

func (client *bitwardenCLI) login(ctx context.Context) {
	client.run(ctx, nil, "config", "server", client.server)
	client.run(ctx, nil, "login", "--passwordenv", cliPasswordEnvironment, client.email)
	output := client.run(ctx, nil, "--raw", "unlock", "--passwordenv", cliPasswordEnvironment)
	client.session = strings.TrimSpace(string(output))
	if client.session == "" {
		client.t.Fatal("Bitwarden CLI returned an empty session")
	}
	client.run(ctx, nil, "sync", "--force")
}

func (client *bitwardenCLI) run(ctx context.Context, input []byte, arguments ...string) []byte {
	client.t.Helper()
	output, diagnostics, err := client.tryRun(ctx, input, arguments...)
	if err != nil {
		client.t.Fatalf("Bitwarden CLI command failed: %v: %s", err, client.sanitize(diagnostics))
	}
	return output
}

func (client *bitwardenCLI) tryRun(ctx context.Context, input []byte, arguments ...string) ([]byte, string, error) {
	command := exec.CommandContext(ctx, client.path, append([]string{"--nointeraction"}, arguments...)...)
	command.Env = cliEnvironment(client.appdata, client.password, client.session)
	if input != nil {
		command.Stdin = bytes.NewReader(input)
	}
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err := command.Run()
	return stdout.Bytes(), stderr.String(), err
}

func (client *bitwardenCLI) expectSyncFailure(ctx context.Context) {
	client.t.Helper()
	if _, diagnostics, err := client.tryRun(ctx, nil, "sync", "--force"); err == nil {
		client.t.Fatal("old authenticated CLI session remained valid after RSA regeneration")
	} else if strings.Contains(client.sanitize(diagnostics), client.password) {
		client.t.Fatal("CLI diagnostics exposed a password")
	}
}

func (client *bitwardenCLI) createFolder(ctx context.Context, name string) string {
	object := map[string]any{"name": name}
	return client.createEncodedObject(ctx, "folder", object)
}

func (client *bitwardenCLI) getFolder(ctx context.Context, id string) map[string]any {
	return decodeJSONObject(client.t, client.run(ctx, nil, "--raw", "get", "folder", id), "folder")
}

func (client *bitwardenCLI) createItem(ctx context.Context, name, password, folderID string) string {
	item := map[string]any{
		"organizationId": nil,
		"folderId":       folderID,
		"type":           1,
		"reprompt":       0,
		"name":           name,
		"notes":          "stable E2E note",
		"favorite":       false,
		"login": map[string]any{
			"username": "vaultwarden-less@example.com",
			"password": password,
			"uris":     []any{},
		},
		"fields": []any{}, "secureNote": nil, "card": nil,
	}
	return client.createEncodedObject(ctx, "item", item)
}

func (client *bitwardenCLI) createEncodedObject(ctx context.Context, objectType string, object map[string]any) string {
	client.t.Helper()
	data, err := json.Marshal(object)
	if err != nil {
		client.t.Fatal(err)
	}
	encoded := strings.TrimSpace(string(client.run(ctx, data, "encode")))
	created := decodeJSONObject(client.t, client.run(ctx, nil, "--raw", "create", objectType, encoded), objectType)
	id := stringValue(created["id"])
	if id == "" {
		client.t.Fatalf("created %s has no ID", objectType)
	}
	return id
}

func (client *bitwardenCLI) getItem(ctx context.Context, id string) map[string]any {
	return decodeJSONObject(client.t, client.run(ctx, nil, "--raw", "get", "item", id), "item")
}

func (client *bitwardenCLI) editItem(ctx context.Context, id string, item map[string]any) {
	data, err := json.Marshal(item)
	if err != nil {
		client.t.Fatal(err)
	}
	encoded := strings.TrimSpace(string(client.run(ctx, data, "encode")))
	client.run(ctx, nil, "--raw", "edit", "item", id, encoded)
}

func (client *bitwardenCLI) createAttachment(ctx context.Context, itemID, path string) string {
	object := decodeJSONObject(client.t, client.run(ctx, nil, "--raw", "create", "attachment", "--file", path, "--itemid", itemID), "attachment result")
	attachments, ok := object["attachments"].([]any)
	if !ok {
		client.t.Fatal("created item has no attachment list")
	}
	for _, candidate := range attachments {
		attachment, ok := candidate.(map[string]any)
		if ok && stringValue(attachment["id"]) != "" {
			return stringValue(attachment["id"])
		}
	}
	client.t.Fatal("created attachment has no ID")
	return ""
}

func (client *bitwardenCLI) getAttachment(ctx context.Context, attachmentID, itemID, output string) {
	client.run(ctx, nil, "--raw", "get", "attachment", attachmentID, "--itemid", itemID, "--output", output)
}

func (client *bitwardenCLI) createFileSend(ctx context.Context, path, name string) string {
	object := decodeJSONObject(client.t, client.run(ctx, nil, "--raw", "send", "--file", path, "--name", name, "--fullObject"), "file Send")
	id := stringValue(object["id"])
	if id == "" {
		client.t.Fatal("created file Send has no ID")
	}
	return id
}

func (client *bitwardenCLI) getFileSend(ctx context.Context, id, output string) {
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
	_, diagnostics, err := client.tryRun(ctx, nil, "send", "receive", send.AccessURL, "--output", output)
	if err != nil {
		diagnostics = strings.ReplaceAll(diagnostics, send.AccessURL, "<redacted-send-url>")
		client.t.Fatalf("Bitwarden CLI file Send receive failed: %v: %s", err, client.sanitize(diagnostics))
	}
}

func decodeJSONObject(t *testing.T, data []byte, label string) map[string]any {
	t.Helper()
	var object map[string]any
	if err := json.Unmarshal(data, &object); err != nil || object == nil {
		t.Fatalf("decode %s: invalid JSON object", label)
	}
	return object
}

func setItemPassword(t *testing.T, item map[string]any, password string) {
	t.Helper()
	login, ok := item["login"].(map[string]any)
	if !ok {
		t.Fatal("item has no login object")
	}
	login["password"] = password
}

func itemPassword(t *testing.T, item map[string]any) string {
	t.Helper()
	login, ok := item["login"].(map[string]any)
	if !ok {
		t.Fatal("item has no login object")
	}
	return stringValue(login["password"])
}

func stringValue(value any) string {
	result, _ := value.(string)
	return result
}

func cliEnvironment(appdata, password, session string) []string {
	environment := make([]string, 0, len(os.Environ())+4)
	for _, value := range os.Environ() {
		name, _, ok := strings.Cut(value, "=")
		if ok && name != "BITWARDENCLI_APPDATA_DIR" && name != "BW_SESSION" && name != cliPasswordEnvironment && name != "NODE_TLS_REJECT_UNAUTHORIZED" {
			environment = append(environment, value)
		}
	}
	environment = append(environment,
		"BITWARDENCLI_APPDATA_DIR="+appdata,
		cliPasswordEnvironment+"="+password,
		"NODE_TLS_REJECT_UNAUTHORIZED=0",
	)
	if session != "" {
		environment = append(environment, "BW_SESSION="+session)
	}
	return environment
}

func (client *bitwardenCLI) sanitize(value string) string {
	for _, secret := range []string{client.password, client.session} {
		if secret != "" {
			value = strings.ReplaceAll(value, secret, "<redacted>")
		}
	}
	return sanitize(value)
}

func privateTestPath(t *testing.T, parent, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(parent, name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
