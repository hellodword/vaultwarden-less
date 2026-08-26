//go:build systemtest

package restore_test

import (
	"database/sql"
	"fmt"
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
		`CREATE TABLE sends (uuid TEXT PRIMARY KEY, atype INTEGER NOT NULL, data TEXT NOT NULL)`,
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
	sendID := "33333333-3333-3333-3333-333333333333"
	sendFileID := strings.Repeat("b", 64)
	sendData := []byte("synthetic file Send")
	if _, err := database.Exec(`INSERT INTO ciphers(uuid) VALUES (?)`, cipherID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`INSERT INTO attachments(id, cipher_uuid, file_size) VALUES (?, ?, ?)`, attachmentID, cipherID, len(attachment)); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`INSERT INTO sends(uuid, atype, data) VALUES (?, 1, ?)`, sendID, fmt.Sprintf(`{"id":%q,"size":%d}`, sendFileID, len(sendData))); err != nil {
		t.Fatal(err)
	}
	writeDataFile(t, filepath.Join(dataDir, "attachments", cipherID, attachmentID), attachment)
	writeDataFile(t, filepath.Join(dataDir, "sends", sendID, sendFileID), sendData)

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
