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
	"slices"
	"strings"
	"testing"
	"time"
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

type bitwardenSendReceiver struct {
	t       *testing.T
	path    string
	appdata string
	home    string
	temp    string
	caFile  string
}

func newBitwardenCLI(t *testing.T, appdata, server, caFile, email string) *bitwardenCLI {
	t.Helper()
	password := testPassword
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

func (client *bitwardenCLI) createOrganizationItem(ctx context.Context, name, password, organizationID, collectionID string) string {
	item := map[string]any{
		"organizationId": organizationID,
		"collectionIds":  []string{collectionID},
		"folderId":       nil,
		"type":           1,
		"reprompt":       0,
		"name":           name,
		"notes":          "organization recovery E2E note",
		"favorite":       false,
		"login": map[string]any{
			"username": "organization@example.com",
			"password": password,
			"uris":     []any{},
		},
		"fields": []any{}, "secureNote": nil, "card": nil,
	}
	return client.createEncodedObject(ctx, "item", item)
}

func (client *bitwardenCLI) getOrganization(ctx context.Context, id string) map[string]any {
	return decodeJSONObject(client.t, client.run(ctx, nil, "--raw", "get", "organization", id), "organization")
}

func (client *bitwardenCLI) organizationMembers(ctx context.Context, organizationID string) []map[string]any {
	client.t.Helper()
	var members []map[string]any
	data := client.run(ctx, nil, "--raw", "list", "org-members", "--organizationid", organizationID)
	if err := json.Unmarshal(data, &members); err != nil || members == nil {
		client.t.Fatal("Bitwarden CLI returned an invalid organization member list")
	}
	return members
}

func (client *bitwardenCLI) collectionByName(ctx context.Context, organizationID, name string) map[string]any {
	client.t.Helper()
	var collections []map[string]any
	data := client.run(ctx, nil, "--raw", "list", "org-collections", "--organizationid", organizationID)
	if err := json.Unmarshal(data, &collections); err != nil {
		client.t.Fatal("Bitwarden CLI returned an invalid organization collection list")
	}
	for _, collection := range collections {
		if stringValue(collection["name"]) == name && stringValue(collection["id"]) != "" {
			return collection
		}
	}
	client.t.Fatalf("organization collection %q was not found", name)
	return nil
}

func (client *bitwardenCLI) createOrganizationCollection(ctx context.Context, organizationID, name string) string {
	object := map[string]any{
		"organizationId": organizationID,
		"name":           name,
		"externalId":     nil,
		"groups":         []any{},
		"users":          []any{},
	}
	data, err := json.Marshal(object)
	if err != nil {
		client.t.Fatal(err)
	}
	encoded := strings.TrimSpace(string(client.run(ctx, data, "encode")))
	created := decodeJSONObject(
		client.t,
		client.run(ctx, nil, "--raw", "create", "org-collection", encoded, "--organizationid", organizationID),
		"organization collection",
	)
	id := stringValue(created["id"])
	if id == "" {
		client.t.Fatal("created organization collection has no ID")
	}
	return id
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
