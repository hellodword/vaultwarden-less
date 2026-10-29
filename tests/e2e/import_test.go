package e2e_test

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNativeArchiveImportAcceptance(t *testing.T) {
	if !*runE2E {
		t.Skip("packaged Compose acceptance only")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	h := newE2EHarness(t, ctx)
	h.start()
	h.waitStatus(3*time.Minute, settled)
	email := "import@example.com"
	h.overrideVaultwarden(map[string]string{"SIGNUPS_ALLOWED": "true", "SENDS_ALLOWED": "true"})
	registerAccount(ctx, t, loopbackEndpoint(h.proxyPort), email, testPassword)
	client := newBitwardenCLI(t, filepath.Join(h.testRoot, "bw-import-source"), h.publicTLS, h.tlsCAFile, email)
	client.login(ctx)
	fixture := createVaultFixture(t, ctx, h.testRoot, client, "Imported native archive")
	h.overrideVaultwarden(map[string]string{})
	// Keep the independent in-memory object-store peers alive while stopping
	// only the product writers and capture worker for the native archive.
	h.compose("stop", "proxy", "backup", "vaultwarden")
	data := filepath.Join(h.deploymentRoot, "data")
	// Deliberately dangerous old configuration must stay inactive after import.
	writePrivateFile(t, filepath.Join(data, "config.json"), `{"signups_allowed":true,"sends_allowed":true,"disable_admin_token":true}`)
	keyBefore := fileDigest(t, filepath.Join(data, "rsa_key.pem"))
	// All product writers are stopped. This controlled SQLite fixture keeps a
	// committed WAL without concurrent writes while the native archive is taken.
	command := exec.CommandContext(ctx, "sqlite3", "-batch", filepath.Join(data, "db.sqlite3"))
	input, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { input.Close(); command.Wait() })
	fmt.Fprintln(input, "PRAGMA journal_mode=WAL; PRAGMA wal_autocheckpoint=0; CREATE TABLE import_wal_fixture(v); INSERT INTO import_wal_fixture VALUES('committed in archive WAL'); SELECT 'ready';")
	scanner := bufio.NewScanner(output)
	ready := false
	for scanner.Scan() {
		if scanner.Text() == "ready" {
			ready = true
			break
		}
	}
	if !ready {
		t.Fatal("WAL fixture failed")
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if info, err := os.Stat(filepath.Join(data, "db.sqlite3"+suffix)); err != nil || info.Size() == 0 {
			t.Fatal("archive has no committed WAL/SHM")
		}
	}
	archive := filepath.Join(h.testRoot, "native.tar")
	if out, err := runCommand(ctx, h.testRoot, nil, os.Environ(), "tar", "--create", "--file", archive, "--directory", h.deploymentRoot, "data"); err != nil {
		t.Fatalf("archive: %v %s", err, out)
	}
	archiveHash := fileDigest(t, archive)
	input.Close()
	if err := command.Wait(); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(data, filepath.Join(h.testRoot, "original-data")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(data, 0o700); err != nil {
		t.Fatal(err)
	}
	stage := filepath.Join(h.testRoot, "import-stage")
	doc, err := os.ReadFile(filepath.Join(h.releaseRoot, "docs/deployment.md"))
	if err != nil {
		t.Fatal(err)
	}
	_, example, ok := strings.Cut(string(doc), "<!-- native-import-example -->\n```sh\n")
	if !ok {
		t.Fatal("documented import missing")
	}
	example, _, _ = strings.Cut(example, "\n```")
	environment := append(os.Environ(), "ARCHIVE="+archive, "IMPORT_STAGE="+stage, "IMPORT_DATA_ROOT="+filepath.Join(stage, "data"), "DEPLOY_DIR="+h.deploymentRoot)
	if out, err := runCommand(ctx, h.testRoot, nil, environment, "bash", "-c", example); err != nil {
		t.Fatalf("documented import failed: %v %s", err, out)
	}
	if fileDigest(t, archive) != archiveHash || fileDigest(t, filepath.Join(data, "rsa_key.pem")) != keyBefore {
		t.Fatal("import changed archive or keys")
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(filepath.Join(data, "db.sqlite3"+suffix)); err != nil {
			t.Fatal("import dropped sidecars")
		}
	}
	h.compose("start", "vaultwarden", "backup", "proxy")
	for _, service := range []string{"vaultwarden", "backup", "proxy"} {
		h.waitHealthy(service, 3*time.Minute)
	}
	expectRegistrationRejected(ctx, t, loopbackEndpoint(h.proxyPort), "must-stay-closed@example.com", testPassword)
	imported := newBitwardenCLI(t, filepath.Join(h.testRoot, "bw-imported"), h.publicTLS, h.tlsCAFile, email)
	imported.login(ctx)
	imported.expectSendRejected(ctx)
	assertVaultFixture(t, ctx, filepath.Join(h.testRoot, "verify-imported"), imported, fixture)
	if value := h.sqlite(data, "SELECT v FROM import_wal_fixture;"); strings.TrimSpace(value) != "committed in archive WAL" {
		t.Fatal("committed WAL content lost")
	}
	status := h.check(false)
	if status.Source != "complete" {
		t.Fatal("imported source not complete")
	}
	h.compose("stop", "proxy", "backup", "vaultwarden")
	for _, name := range []string{"primary", "secondary"} {
		restored := h.restore(name, h.latest(name).ID, "imported-"+name)
		if fileDigest(t, filepath.Join(restored, "config.json")) != fileDigest(t, filepath.Join(data, "config.json")) {
			t.Fatal("inactive config not retained")
		}
		h.startRestored(restored, "import-"+name)
		recovered := newBitwardenCLI(t, filepath.Join(h.testRoot, "bw-import-"+name), h.publicTLS, h.tlsCAFile, email)
		recovered.login(ctx)
		assertVaultFixture(t, ctx, filepath.Join(h.testRoot, "verify-import-"+name), recovered, fixture)
	}
}
