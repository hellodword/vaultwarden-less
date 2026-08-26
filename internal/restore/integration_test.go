//go:build systemtest

package restore_test

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hellodword/vaultwarden-less/internal/upstream"
	_ "github.com/mattn/go-sqlite3"
)

func createSyntheticVaultwarden(t *testing.T, dataDir string) *sql.DB {
	t.Helper()
	databasePath := filepath.Join(dataDir, "db.sqlite3")
	database, err := sql.Open("sqlite3", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	contract, err := upstream.VaultwardenSchemaContract()
	if err != nil {
		t.Fatal(err)
	}
	statements := []string{`PRAGMA journal_mode = WAL`, `PRAGMA foreign_keys = ON`}
	for _, object := range contract.Objects {
		statements = append(statements, object.SQL)
	}
	for _, statement := range statements {
		if _, err := database.Exec(statement); err != nil {
			_ = database.Close()
			t.Fatal(err)
		}
	}
	if _, err := database.Exec(`INSERT INTO __diesel_schema_migrations(version) VALUES (?)`, upstream.VaultwardenMigration); err != nil {
		_ = database.Close()
		t.Fatal(err)
	}

	cipherID := "11111111-1111-1111-1111-111111111111"
	attachmentID := strings.Repeat("a", 20)
	attachment := []byte("synthetic attachment")
	sendID := "33333333-3333-3333-3333-333333333333"
	sendFileID := strings.Repeat("b", 64)
	sendData := []byte("synthetic file Send")
	if _, err := database.Exec(`INSERT INTO ciphers(uuid, created_at, updated_at, atype, name, data) VALUES (?, '2026-05-05', '2026-05-05', 2, 'cipher', '{}')`, cipherID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`INSERT INTO attachments(id, cipher_uuid, file_name, file_size) VALUES (?, ?, 'encrypted', ?)`, attachmentID, cipherID, len(attachment)); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`INSERT INTO sends(
		uuid, name, atype, data, akey, access_count, creation_date,
		revision_date, deletion_date, disabled
	) VALUES (?, 'file send', 1, ?, 'key', 0, '2026-05-05', '2026-05-05', '2026-06-05', 0)`, sendID, fmt.Sprintf(`{"id":%q,"size":%d}`, sendFileID, len(sendData))); err != nil {
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
