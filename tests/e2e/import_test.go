package e2e_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/mattn/go-sqlite3"
)

const importWALMarker = 82801

type initialImportResult struct {
	ImportID             string `json:"import_id"`
	Phase                string `json:"phase"`
	ProjectCommit        string `json:"project_commit"`
	Release              string `json:"release"`
	SourceTreeID         string `json:"source_tree_id"`
	CandidateTreeID      string `json:"candidate_tree_id"`
	ClientLoginConfirmed bool   `json:"client_login_confirmed"`
}

func TestInitialImportLifecycle(t *testing.T) {
	if !*runImportE2E {
		t.Skip("pass -run-import-e2e to run the real initial SQLite import lifecycle")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Minute)
	defer cancel()
	harness := newE2EHarness(t, ctx)
	harness.startMotoAndRepository()
	harness.initializeDeployment()
	harness.startVaultwarden()

	unique := time.Now().UnixNano()
	email := fmt.Sprintf("vaultwarden-less-e2e-import-%d@example.com", unique)
	_ = registerAccount(ctx, t, harness.publicHTTP, email, testPassword)
	oldClient := newBitwardenCLI(
		t,
		filepath.Join(harness.localRoot, "bw-import-source"),
		harness.publicTLS,
		harness.tlsCAFile,
		email,
	)
	oldClient.login(ctx)
	itemID := oldClient.createItem(ctx, "Imported Recovery Item", initialItemPassword, "")
	attachmentData := []byte("vaultwarden-less imported attachment\n")
	attachmentPath := privateTestPath(t, harness.localRoot, "import-attachment.txt", attachmentData)
	attachmentID := oldClient.createAttachment(ctx, itemID, attachmentPath)
	sendData := []byte("vaultwarden-less imported file Send\n")
	sendPath := privateTestPath(t, harness.localRoot, "import-send.txt", sendData)
	sendID := oldClient.createFileSend(ctx, sendPath, "Imported Recovery Send")

	harness.stopProduction()
	externalRoot := t.TempDir()
	if err := os.Chmod(externalRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	externalSource := filepath.Join(externalRoot, "stopped-source")
	copyPrivateImportTree(t, filepath.Join(harness.localRoot, "data"), externalSource)
	forcePersistentImportWAL(t, filepath.Join(externalSource, "db.sqlite3"))
	externalTreeID := privateImportTreeID(t, externalSource)
	oldRSA := privateImportFileDigest(t, filepath.Join(externalSource, "rsa_key.pem"))

	resetImportTarget(t, harness, externalSource)
	source := filepath.Join(harness.localRoot, "import", "source")
	if sourceTreeID := privateImportTreeID(t, source); sourceTreeID != externalTreeID {
		t.Fatalf("project source copy = %q, want external %q", sourceTreeID, externalTreeID)
	}

	prepared := parseInitialImportResult(t, runImportAdmin(t, harness, "deployment", "import", "prepare"), "prepared")
	if prepared.ClientLoginConfirmed || prepared.SourceTreeID == "" || prepared.CandidateTreeID == "" {
		t.Fatalf("prepared import result = %#v", prepared)
	}
	assertImportSourceUnchanged(t, source, externalTreeID)
	candidate := filepath.Join(harness.localRoot, "import", "candidate")
	assertImportSQLiteMarker(t, filepath.Join(candidate, "db.sqlite3"), importWALMarker)
	assertNoImportSQLiteSidecars(t, candidate)
	for _, name := range []string{"rsa_key.pem", "rsa_key.pub.pem", "rsa_key.der"} {
		if _, err := os.Lstat(filepath.Join(candidate, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("prepared candidate retained %s: %v", name, err)
		}
	}

	harness.compose("--profile", "import", "up", "--detach", "import-vaultwarden")
	harness.waitHealthy("import-vaultwarden", 3*time.Minute)
	harness.setBackend(harness.mappedEndpoint("import-vaultwarden", 80))
	harness.waitHTTP(harness.publicHTTP+"/alive", 200, time.Minute)
	oldClient.expectSyncFailure(ctx)

	candidateClient := newBitwardenCLI(
		t,
		filepath.Join(harness.localRoot, "bw-import-candidate"),
		harness.publicTLS,
		harness.tlsCAFile,
		email,
	)
	candidateClient.login(ctx)
	assertImportedClientData(
		t,
		ctx,
		candidateClient,
		itemID,
		attachmentID,
		sendID,
		attachmentData,
		sendData,
		filepath.Join(harness.localRoot, "candidate-client"),
	)
	assertImportSourceUnchanged(t, source, externalTreeID)
	harness.compose("--profile", "import", "stop", "import-vaultwarden")

	for _, failure := range []struct {
		arguments []string
		code      string
	}{
		{[]string{"deployment", "import", "verify"}, "usage_error"},
		{[]string{"deployment", "import", "accept"}, "import_state_invalid"},
	} {
		output, err := tryImportAdmin(harness, failure.arguments...)
		if err == nil {
			t.Fatalf("unconfirmed import command unexpectedly succeeded: %s", harnessDiagnostics(output))
		}
		if actual := exactCommandErrorCode(t, output); actual != failure.code {
			t.Fatalf("unconfirmed import error = %q, want %q", actual, failure.code)
		}
	}

	verified := parseInitialImportResult(
		t,
		runImportAdmin(t, harness, "deployment", "import", "verify", "--confirm-client-login"),
		"verified",
	)
	if !verified.ClientLoginConfirmed || verified.CandidateTreeID == prepared.CandidateTreeID {
		t.Fatalf("verified import result = %#v", verified)
	}
	assertImportSourceUnchanged(t, source, externalTreeID)
	assertPrivateImportTree(t, candidate)
	assertNoImportSQLiteSidecars(t, candidate)
	assertImportSQLiteMarker(t, filepath.Join(candidate, "db.sqlite3"), importWALMarker)
	newRSA := privateImportFileDigest(t, filepath.Join(candidate, "rsa_key.pem"))
	if newRSA == oldRSA {
		t.Fatal("candidate retained the external RSA identity")
	}

	accepted := runImportTerminalSequence(t, harness)
	if accepted.Phase != "finalized" || accepted.ProjectCommit != verified.ProjectCommit ||
		accepted.Release != verified.Release {
		t.Fatalf("terminal import = %#v, verified %#v", accepted, verified)
	}
	if _, err := os.Lstat(candidate); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("finalized candidate survived: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(harness.localRoot, "state", "import", "current")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("finalized current state survived: %v", err)
	}
	if info, err := os.Lstat(filepath.Join(harness.localRoot, "state", "import", "history", "initial")); err != nil || !info.IsDir() {
		t.Fatalf("finalized import history = %#v/%v", info, err)
	}
	assertImportSourceUnchanged(t, source, externalTreeID)
	assertImportSQLiteMarker(t, filepath.Join(harness.localRoot, "data", "db.sqlite3"), importWALMarker)
	if finalRSA := privateImportFileDigest(t, filepath.Join(harness.localRoot, "data", "rsa_key.pem")); finalRSA != newRSA {
		t.Fatalf("accepted RSA identity = %q, want candidate %q", finalRSA, newRSA)
	}

	harness.setBackend(harness.mappedEndpoint("proxy", 8080))
	harness.waitHTTP(harness.publicHTTP+"/.vaultwarden-less/healthz", 204, 2*time.Minute)
	oldClient.expectSyncFailure(ctx)
	candidateClient.sync(ctx)
	postImportClient := newBitwardenCLI(
		t,
		filepath.Join(harness.localRoot, "bw-import-normal"),
		harness.publicTLS,
		harness.tlsCAFile,
		email,
	)
	postImportClient.login(ctx)
	assertImportedClientData(
		t,
		ctx,
		postImportClient,
		itemID,
		attachmentID,
		sendID,
		attachmentData,
		sendData,
		filepath.Join(harness.localRoot, "normal-client"),
	)
}

func runImportAdmin(t *testing.T, harness *e2eHarness, arguments ...string) string {
	t.Helper()
	output, err := tryImportAdmin(harness, arguments...)
	if err != nil {
		t.Fatalf("deployment admin %s: %v: %s", strings.Join(arguments, " "), err, harnessDiagnostics(output))
	}
	return output
}

func tryImportAdmin(harness *e2eHarness, arguments ...string) (string, error) {
	command := []string{"--profile", "deployment", "run", "--rm", "--no-deps", "deployment-admin"}
	return harness.tryCompose(harness.ctx, append(command, arguments...)...)
}

func parseInitialImportResult(t *testing.T, output, phase string) initialImportResult {
	t.Helper()
	var result initialImportResult
	if decodeExactJSON(output, &result) != nil || result.ImportID != "initial" ||
		result.Phase != phase || !lowerHex40Pattern.MatchString(result.ProjectCommit) || result.Release == "" ||
		!contentIDPattern.MatchString(result.SourceTreeID) || !contentIDPattern.MatchString(result.CandidateTreeID) {
		t.Fatalf("invalid %s import result: %s", phase, harnessDiagnostics(output))
	}
	return result
}

func runImportTerminalSequence(t *testing.T, harness *e2eHarness) initialImportResult {
	t.Helper()
	harness.compose("--profile", "import", "stop", "import-vaultwarden")
	harness.compose("--profile", "import", "rm", "--force", "--stop", "import-vaultwarden")
	accepted := parseInitialImportResult(
		t,
		runImportAdmin(t, harness, "deployment", "import", "accept"),
		"accepted",
	)
	// Replay the immutable Go boundary before any host-side runtime action. It
	// must continue the same decision and must not exchange the trees twice.
	replayed := parseInitialImportResult(
		t,
		runImportAdmin(t, harness, "deployment", "import", "accept"),
		"accepted",
	)
	if replayed.ProjectCommit != accepted.ProjectCommit || replayed.Release != accepted.Release ||
		replayed.CandidateTreeID != accepted.CandidateTreeID {
		t.Fatalf("replayed import decision changed identity: first=%#v replay=%#v", accepted, replayed)
	}
	harness.compose("up", "--detach", "--wait", "--wait-timeout", "180", "vaultwarden", "backup", "proxy")
	assertImportedTerminalServices(t, harness, accepted.ProjectCommit, accepted.Release)
	return parseInitialImportResult(
		t,
		runImportAdmin(t, harness, "deployment", "import", "finalize"),
		"finalized",
	)
}

func assertImportedTerminalServices(t *testing.T, harness *e2eHarness, commit, release string) {
	t.Helper()
	for _, service := range []string{"vaultwarden", "backup", "proxy"} {
		harness.waitHealthy(service, 4*time.Minute)
		reference := "vaultwarden-less-" + service + ":local"
		imageOutput, err := harness.docker("image", "inspect", reference)
		if err != nil {
			t.Fatalf("inspect imported %s image: %v: %s", service, err, harnessDiagnostics(imageOutput))
		}
		var images []struct {
			ID     string `json:"Id"`
			Config struct {
				Labels map[string]string `json:"Labels"`
			} `json:"Config"`
		}
		if err := json.Unmarshal([]byte(imageOutput), &images); err != nil || len(images) != 1 ||
			images[0].Config.Labels["org.opencontainers.image.revision"] != commit ||
			images[0].Config.Labels["org.opencontainers.image.version"] != release {
			t.Fatalf("imported %s image identity is invalid: %v", service, err)
		}
		container := strings.TrimSpace(harness.compose("ps", "--quiet", service))
		if !lowerHex64Pattern.MatchString(container) {
			t.Fatalf("imported %s container identity is invalid: %q", service, container)
		}
		containerOutput, err := harness.docker("container", "inspect", container)
		if err != nil {
			t.Fatalf("inspect imported %s container: %v: %s", service, err, harnessDiagnostics(containerOutput))
		}
		var containers []struct {
			Image  string `json:"Image"`
			Config struct {
				Image string `json:"Image"`
			} `json:"Config"`
			State struct {
				Running bool `json:"Running"`
				Health  struct {
					Status string `json:"Status"`
				} `json:"Health"`
			} `json:"State"`
		}
		if err := json.Unmarshal([]byte(containerOutput), &containers); err != nil || len(containers) != 1 ||
			containers[0].Image != images[0].ID || containers[0].Config.Image != reference ||
			!containers[0].State.Running || containers[0].State.Health.Status != "healthy" {
			t.Fatalf("imported %s container identity or health is invalid: %v", service, err)
		}
	}
}

func resetImportTarget(t *testing.T, harness *e2eHarness, externalSource string) {
	t.Helper()
	data := filepath.Join(harness.localRoot, "data")
	active := filepath.Join(harness.localRoot, "state", "deployment")
	importRoot := filepath.Join(harness.localRoot, "import")
	if filepath.Dir(data) != harness.localRoot || filepath.Dir(filepath.Dir(active)) != harness.localRoot ||
		filepath.Dir(importRoot) != harness.localRoot {
		t.Fatal("disposable import paths escaped the E2E root")
	}
	for _, path := range []string{data, active, importRoot} {
		if err := os.RemoveAll(path); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{data, importRoot} {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	copyPrivateImportTree(t, externalSource, filepath.Join(importRoot, "source"))
}

func forcePersistentImportWAL(t *testing.T, path string) {
	t.Helper()
	database, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	database.SetMaxOpenConns(1)
	database.SetMaxIdleConns(1)
	connection, err := database.Conn(context.Background())
	if err != nil {
		_ = database.Close()
		t.Fatal(err)
	}
	var journal string
	if err := connection.QueryRowContext(context.Background(), "PRAGMA journal_mode = WAL").Scan(&journal); err != nil || journal != "wal" {
		t.Fatalf("enable source WAL = %q/%v", journal, err)
	}
	if _, err := connection.ExecContext(context.Background(), "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		t.Fatal(err)
	}
	if err := connection.Raw(func(raw any) error {
		sqlite, ok := raw.(*sqlite3.SQLiteConn)
		if !ok {
			return fmt.Errorf("unexpected SQLite driver")
		}
		return sqlite.SetFileControlInt("main", sqlite3.SQLITE_FCNTL_PERSIST_WAL, 1)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := connection.ExecContext(context.Background(), "PRAGMA wal_autocheckpoint = 0"); err != nil {
		t.Fatal(err)
	}
	if _, err := connection.ExecContext(context.Background(), fmt.Sprintf("PRAGMA user_version = %d", importWALMarker)); err != nil {
		t.Fatal(err)
	}
	if err := connection.Close(); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	for _, candidate := range []string{path, path + "-wal", path + "-shm"} {
		info, err := os.Lstat(candidate)
		if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
			t.Fatalf("persistent source SQLite file %s = %#v/%v", filepath.Base(candidate), info, err)
		}
		if err := os.Chmod(candidate, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func assertImportSQLiteMarker(t *testing.T, path string, want int) {
	t.Helper()
	database, err := sql.Open("sqlite3", "file:"+filepath.ToSlash(path)+"?mode=ro&immutable=1")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close() }()
	var got int
	if err := database.QueryRow("PRAGMA user_version").Scan(&got); err != nil || got != want {
		t.Fatalf("import SQLite WAL marker = %d/%v, want %d", got, err, want)
	}
}

func assertNoImportSQLiteSidecars(t *testing.T, root string) {
	t.Helper()
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if _, err := os.Lstat(filepath.Join(root, "db.sqlite3") + suffix); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("candidate retained SQLite sidecar %s: %v", suffix, err)
		}
	}
}

func assertImportedClientData(
	t *testing.T,
	ctx context.Context,
	client *bitwardenCLI,
	itemID, attachmentID, sendID string,
	attachmentData, sendData []byte,
	outputRoot string,
) {
	t.Helper()
	if err := os.Mkdir(outputRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if itemPassword(t, client.getItem(ctx, itemID)) != initialItemPassword {
		t.Fatal("imported login changed")
	}
	attachment := filepath.Join(outputRoot, "attachment")
	client.getAttachment(ctx, attachmentID, itemID, attachment)
	assertFileContent(t, attachment, attachmentData)
	send := filepath.Join(outputRoot, "send")
	client.getFileSend(ctx, sendID, send)
	assertFileContent(t, send, sendData)
}

func copyPrivateImportTree(t *testing.T, source, destination string) {
	t.Helper()
	if err := os.Mkdir(destination, 0o700); err != nil {
		t.Fatal(err)
	}
	err := filepath.WalkDir(source, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == source {
			return nil
		}
		relative, err := filepath.Rel(source, path)
		if err != nil || relative == "." || filepath.IsAbs(relative) || filepath.Clean(relative) != relative {
			return fmt.Errorf("invalid synthetic import path")
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || info.Mode()&os.ModeSymlink != 0 || (!info.IsDir() && (!info.Mode().IsRegular() || stat.Nlink != 1)) {
			return fmt.Errorf("synthetic external source contains an unsafe entry")
		}
		target := filepath.Join(destination, relative)
		if info.IsDir() {
			return os.Mkdir(target, 0o700)
		}
		sourceFile, err := os.Open(path)
		if err != nil {
			return err
		}
		targetFile, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			_ = sourceFile.Close()
			return err
		}
		_, copyErr := io.Copy(targetFile, sourceFile)
		closeTargetErr := targetFile.Close()
		closeSourceErr := sourceFile.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeTargetErr != nil {
			return closeTargetErr
		}
		return closeSourceErr
	})
	if err != nil {
		t.Fatal(err)
	}
	assertPrivateImportTree(t, destination)
}

func assertPrivateImportTree(t *testing.T, root string) {
	t.Helper()
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != uint32(os.Geteuid()) || stat.Gid != uint32(os.Getegid()) || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("import tree has an unsafe owner or symlink")
		}
		want := os.FileMode(0o600)
		if info.IsDir() {
			want = 0o700
		} else if !info.Mode().IsRegular() || stat.Nlink != 1 {
			return fmt.Errorf("import tree has an unsafe file type")
		}
		if info.Mode().Perm() != want {
			return fmt.Errorf("import tree mode = %04o, want %04o", info.Mode().Perm(), want)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func privateImportTreeID(t *testing.T, root string) string {
	t.Helper()
	digest := sha256.New()
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if err := writeImportDigestFrame(digest, []byte(relative)); err != nil {
			return err
		}
		kind := byte('f')
		if info.IsDir() {
			kind = 'd'
		}
		if _, err := digest.Write([]byte{kind}); err != nil {
			return err
		}
		if err := binary.Write(digest, binary.BigEndian, uint32(info.Mode().Perm())); err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(digest, file)
		closeErr := file.Close()
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	})
	if err != nil {
		t.Fatal(err)
	}
	return "sha256:" + hex.EncodeToString(digest.Sum(nil))
}

func writeImportDigestFrame(destination io.Writer, value []byte) error {
	if err := binary.Write(destination, binary.BigEndian, uint64(len(value))); err != nil {
		return err
	}
	_, err := destination.Write(value)
	return err
}

func privateImportFileDigest(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(digest[:])
}

func assertImportSourceUnchanged(t *testing.T, source, want string) {
	t.Helper()
	if got := privateImportTreeID(t, source); got != want {
		t.Fatalf("import source changed: got %q, want %q", got, want)
	}
}
