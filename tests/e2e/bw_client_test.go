package e2e_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

type bitwardenCLI struct {
	t            *testing.T
	path         string
	appdata      string
	home         string
	temp         string
	passwordFile string
	server       string
	caFile       string
	email        string
	password     string
	session      string
}

func newBitwardenCLI(t *testing.T, appdata, server, caFile, email string) *bitwardenCLI {
	return newBitwardenCLIWithPassword(t, appdata, server, caFile, email, testPassword)
}

func newBitwardenCLIWithPassword(t *testing.T, appdata, server, caFile, email, password string) *bitwardenCLI {
	t.Helper()
	path, err := exec.LookPath("bw")
	if err != nil {
		t.Fatal(err)
	}
	root := appdata
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	appdata = filepath.Join(root, "appdata")
	home := filepath.Join(root, "home")
	temp := filepath.Join(root, "tmp")
	for _, directory := range []string{appdata, home, temp} {
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	passwordFile := filepath.Join(root, "master-password")
	file, err := os.OpenFile(passwordFile, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(password + "\n"); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(caFile); err != nil || !info.Mode().IsRegular() {
		t.Fatal("Bitwarden CLI test CA must be a regular file")
	}
	return &bitwardenCLI{
		t: t, path: path, appdata: appdata, home: home, temp: temp,
		passwordFile: passwordFile, server: server, caFile: caFile,
		email: email, password: password,
	}
}

func (client *bitwardenCLI) login(ctx context.Context) {
	client.run(ctx, nil, "config", "server", client.server)
	client.run(ctx, nil, "login", "--passwordfile", client.passwordFile, client.email)
	output := client.run(ctx, nil, "--raw", "unlock", "--passwordfile", client.passwordFile)
	client.session = strings.TrimSpace(string(output))
	if client.session == "" {
		client.t.Fatal("Bitwarden CLI returned an empty session")
	}
	client.run(ctx, nil, "sync", "--force")
}

func (client *bitwardenCLI) sync(ctx context.Context) {
	client.run(ctx, nil, "sync", "--force")
}

func (client *bitwardenCLI) run(ctx context.Context, input []byte, arguments ...string) []byte {
	client.t.Helper()
	output, diagnostics, err := client.tryRun(ctx, input, arguments...)
	if err != nil {
		client.t.Fatalf("Bitwarden CLI command failed: %v: %s", err, client.redact(diagnostics))
	}
	return output
}

func (client *bitwardenCLI) tryRun(ctx context.Context, input []byte, arguments ...string) ([]byte, string, error) {
	command := exec.CommandContext(ctx, client.path, append([]string{"--nointeraction"}, arguments...)...)
	command.Env = cliEnvironment(client.home, client.temp, client.appdata, client.caFile, client.session)
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
		client.t.Fatal("pre-restore authenticated CLI session remained valid after RSA regeneration")
	} else if containsSecret(diagnostics, client.password, client.session) {
		client.t.Fatal("CLI diagnostics exposed a password")
	}
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

func (client *bitwardenCLI) createFolder(ctx context.Context, name string) string {
	return client.createEncodedObject(ctx, "folder", map[string]any{"name": name})
}

func (client *bitwardenCLI) getFolder(ctx context.Context, id string) map[string]any {
	return decodeJSONObject(client.t, client.run(ctx, nil, "--raw", "get", "folder", id), "folder")
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

func integerValue(value any) int {
	number, ok := value.(float64)
	if !ok || number < 0 || number != float64(int(number)) {
		return -1
	}
	return int(number)
}

func cliEnvironment(home, temp, appdata, caFile, session string) []string {
	environment := []string{
		"HOME=" + home,
		"TMPDIR=" + temp,
		"BITWARDENCLI_APPDATA_DIR=" + appdata,
		"NODE_EXTRA_CA_CERTS=" + caFile,
	}
	if session != "" {
		environment = append(environment, "BW_SESSION="+session)
	}
	return environment
}

func (client *bitwardenCLI) redact(value string) string {
	return redactDiagnostics(value, client.password, client.session)
}

func TestCLIEnvironmentIsExact(t *testing.T) {
	t.Parallel()
	want := []string{
		"HOME=/isolated/home",
		"TMPDIR=/isolated/tmp",
		"BITWARDENCLI_APPDATA_DIR=/isolated/appdata",
		"NODE_EXTRA_CA_CERTS=/isolated/ca.pem",
		"BW_SESSION=session",
	}
	got := cliEnvironment(
		"/isolated/home", "/isolated/tmp", "/isolated/appdata",
		"/isolated/ca.pem", "session",
	)
	if !slices.Equal(got, want) {
		t.Fatalf("CLI environment = %#v", got)
	}
}

func privateTestPath(t *testing.T, parent, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(parent, name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
