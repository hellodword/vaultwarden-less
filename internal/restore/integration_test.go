package restore_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hellodword/vaultwarden-less/internal/config"
	"github.com/hellodword/vaultwarden-less/internal/restic"
	restoretool "github.com/hellodword/vaultwarden-less/internal/restore"
	"github.com/hellodword/vaultwarden-less/internal/snapshot"
	"github.com/hellodword/vaultwarden-less/internal/version"
	_ "github.com/mattn/go-sqlite3"
)

func TestResticRoundTrip(t *testing.T) {
	if os.Getenv("VAULTWARDEN_LESS_RESTIC_INTEGRATION") != "1" {
		t.Skip("set VAULTWARDEN_LESS_RESTIC_INTEGRATION=1 to run the Restic round trip")
	}
	resticBinary := os.Getenv("RESTIC_BINARY")
	if resticBinary == "" {
		t.Fatal("RESTIC_BINARY is required for the Restic integration test")
	}

	root := t.TempDir()
	dataDir := filepath.Join(root, "data-source")
	stagingDir := filepath.Join(root, "staging")
	cacheDir := filepath.Join(root, "cache")
	for _, path := range []string{dataDir, stagingDir, cacheDir} {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	database := createSyntheticVaultwarden(t, dataDir)
	defer database.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	built, err := (snapshot.Builder{DataDir: dataDir, StagingDir: stagingDir}).Build(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(built.Path, filepath.Join(stagingDir, "current")); err != nil {
		t.Fatal(err)
	}

	repositoryPath := filepath.Join(root, "repository")
	repositoryFile := writeSecret(t, root, "repository-file", repositoryPath)
	passwordFile := writeSecret(t, root, "password-file", "synthetic-restic-password")
	maintenance := config.Credentials{PasswordFile: passwordFile}
	target := config.Target{
		Name: "roundtrip", Backend: "local", RepositoryFile: repositoryFile,
		BackupCredentials: maintenance, MaintenanceCredentials: &maintenance,
	}
	runner := restic.Runner{Binary: resticBinary, CacheDir: cacheDir, StagingDir: stagingDir}
	if err := runner.VerifyVersion(ctx); err != nil {
		t.Fatal(err)
	}
	if err := runner.Init(ctx, target); err != nil {
		t.Fatal(err)
	}
	backup, err := runner.Backup(ctx, target, built.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	incompleteManifest, err := snapshot.NewManifest(snapshot.Incomplete, built.Manifest.DatabaseMigration, built.Manifest.Files, []snapshot.Missing{{
		Path:   "data/attachments/33333333-3333-3333-3333-333333333333/cccccccccccccccccccc",
		Reason: "not_found",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Backup(ctx, target, incompleteManifest); err != nil {
		t.Fatal(err)
	}
	if err := runner.Check(ctx, target, ""); err != nil {
		t.Fatal(err)
	}
	pruneReports, err := runner.Prune(ctx, target, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(pruneReports) != 2 {
		t.Fatalf("dry-run retention report count = %d", len(pruneReports))
	}

	output := filepath.Join(root, "prepared")
	cfg := &config.Backup{DataDir: dataDir}
	restored, err := restoretool.Run(ctx, cfg, runner, restoretool.Request{Target: target, Output: output})
	if err != nil {
		t.Fatal(err)
	}
	if restored.SnapshotID != backup.SnapshotID || restored.Manifest.ContentID != built.Manifest.ContentID {
		t.Fatalf("restored result does not identify the backed-up point: %#v", restored)
	}
	verified, err := snapshot.Verify(ctx, output, false)
	if err != nil {
		t.Fatal(err)
	}
	if verified.ContentID != built.Manifest.ContentID {
		t.Fatalf("verified content ID = %s, want %s", verified.ContentID, built.Manifest.ContentID)
	}
}

func createSyntheticVaultwarden(t *testing.T, dataDir string) *sql.DB {
	t.Helper()
	databasePath := filepath.Join(dataDir, "db.sqlite3")
	database, err := sql.Open("sqlite3", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	statements := []string{
		`PRAGMA journal_mode = WAL`,
		`PRAGMA foreign_keys = ON`,
		`CREATE TABLE attachments (id TEXT PRIMARY KEY, cipher_uuid TEXT NOT NULL, file_size INTEGER NOT NULL)`,
		`CREATE TABLE ciphers (uuid TEXT PRIMARY KEY)`,
		`CREATE TABLE sends (uuid TEXT PRIMARY KEY, atype INTEGER NOT NULL, data TEXT NOT NULL)`,
		`CREATE TABLE users (uuid TEXT PRIMARY KEY)`,
		`CREATE TABLE __diesel_schema_migrations (version TEXT PRIMARY KEY)`,
	}
	for _, statement := range statements {
		if _, err := database.Exec(statement); err != nil {
			database.Close()
			t.Fatal(err)
		}
	}
	if _, err := database.Exec(`INSERT INTO __diesel_schema_migrations(version) VALUES (?)`, version.VaultwardenSchema); err != nil {
		database.Close()
		t.Fatal(err)
	}

	cipherID := "11111111-1111-1111-1111-111111111111"
	attachmentID := strings.Repeat("a", 20)
	attachment := []byte("synthetic attachment")
	if _, err := database.Exec(`INSERT INTO ciphers(uuid) VALUES (?)`, cipherID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`INSERT INTO attachments(id, cipher_uuid, file_size) VALUES (?, ?, ?)`, attachmentID, cipherID, len(attachment)); err != nil {
		t.Fatal(err)
	}
	writeDataFile(t, filepath.Join(dataDir, "attachments", cipherID, attachmentID), attachment)

	sendID := "22222222-2222-2222-2222-222222222222"
	fileID := strings.Repeat("b", 64)
	send := []byte("synthetic send")
	if _, err := database.Exec(`INSERT INTO sends(uuid, atype, data) VALUES (?, 1, ?)`, sendID, fmt.Sprintf(`{"id":%q,"size":%d}`, fileID, len(send))); err != nil {
		t.Fatal(err)
	}
	writeDataFile(t, filepath.Join(dataDir, "sends", sendID, fileID), send)
	writeDataFile(t, filepath.Join(dataDir, "config.json"), []byte("{}\n"))
	return database
}

func writeDataFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeSecret(t *testing.T, root, name, value string) string {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.WriteFile(path, []byte(value+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
