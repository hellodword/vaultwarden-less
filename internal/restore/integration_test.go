//go:build systemtest

package restore_test

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hellodword/vaultwarden-less/internal/version"
	_ "github.com/mattn/go-sqlite3"
)

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
		`CREATE TABLE users (uuid TEXT PRIMARY KEY)`,
		`CREATE TABLE __diesel_schema_migrations (version TEXT PRIMARY KEY)`,
	}
	for _, statement := range statements {
		if _, err := database.Exec(statement); err != nil {
			_ = database.Close()
			t.Fatal(err)
		}
	}
	if _, err := database.Exec(`INSERT INTO __diesel_schema_migrations(version) VALUES (?)`, version.VaultwardenSchema); err != nil {
		_ = database.Close()
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
