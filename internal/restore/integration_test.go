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

const (
	syntheticUserID               = "44444444-4444-4444-8444-444444444444"
	syntheticOrganizationID       = "55555555-5555-4555-8555-555555555555"
	syntheticMembershipID         = "66666666-6666-4666-8666-666666666666"
	syntheticCollectionID         = "77777777-7777-4777-8777-777777777777"
	syntheticOrganizationCipherID = "88888888-8888-4888-8888-888888888888"
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
	if _, err := database.Exec(`INSERT INTO __diesel_schema_migrations(version) VALUES (?)`, contract.DatabaseMigration); err != nil {
		_ = database.Close()
		t.Fatal(err)
	}
	organizationInserts := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO users(
			uuid, created_at, updated_at, email, name, password_hash, salt,
			password_iterations, akey, security_stamp, equivalent_domains, excluded_globals
		) VALUES (?, '2026-05-05', '2026-05-05', 'member@example.test', 'member', x'01', x'02', 600000, 'user-key', 'stamp', '[]', '[]')`, []any{syntheticUserID}},
		{"INSERT INTO organizations(uuid, name, billing_email) VALUES (?, 'Synthetic Organization', 'billing@example.test')", []any{syntheticOrganizationID}},
		{`INSERT INTO users_organizations(
			uuid, user_uuid, org_uuid, access_all, akey, status, atype
		) VALUES (?, ?, ?, 0, 'membership-key', 2, 2)`, []any{syntheticMembershipID, syntheticUserID, syntheticOrganizationID}},
		{"INSERT INTO collections(uuid, org_uuid, name) VALUES (?, ?, 'Synthetic Collection')", []any{syntheticCollectionID, syntheticOrganizationID}},
		{"INSERT INTO users_collections(user_uuid, collection_uuid, read_only, hide_passwords, manage) VALUES (?, ?, 0, 0, 1)", []any{syntheticUserID, syntheticCollectionID}},
		{"INSERT INTO ciphers(uuid, created_at, updated_at, organization_uuid, atype, name, data) VALUES (?, '2026-05-05', '2026-05-05', ?, 2, 'organization cipher', '{}')", []any{syntheticOrganizationCipherID, syntheticOrganizationID}},
		{"INSERT INTO ciphers_collections(cipher_uuid, collection_uuid) VALUES (?, ?)", []any{syntheticOrganizationCipherID, syntheticCollectionID}},
	}
	for _, insert := range organizationInserts {
		if _, err := database.Exec(insert.query, insert.args...); err != nil {
			_ = database.Close()
			t.Fatal(err)
		}
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
	) VALUES (?, 'file send', 1, ?, 'key', 0, '2026-05-05', '2026-05-05', '2026-06-05', 0)`, sendID, fmt.Sprintf(`{"id":%q,"size":%d,"sizeName":"synthetic","fileName":"encrypted"}`, sendFileID, len(sendData))); err != nil {
		t.Fatal(err)
	}
	writeDataFile(t, filepath.Join(dataDir, "attachments", cipherID, attachmentID), attachment)
	writeDataFile(t, filepath.Join(dataDir, "sends", sendID, sendFileID), sendData)

	return database
}

func assertSyntheticOrganizationFixture(t *testing.T, databasePath string) {
	t.Helper()
	database, err := sql.Open("sqlite3", "file:"+databasePath+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close() }()
	var email, organization, collection, cipher string
	var accessAll, manage bool
	err = database.QueryRow(`SELECT
		u.email, o.name, c.name, i.name, uo.access_all, uc.manage
		FROM users_organizations AS uo
		JOIN users AS u ON u.uuid = uo.user_uuid
		JOIN organizations AS o ON o.uuid = uo.org_uuid
		JOIN collections AS c ON c.org_uuid = o.uuid
		JOIN users_collections AS uc
			ON uc.user_uuid = u.uuid AND uc.collection_uuid = c.uuid
		JOIN ciphers_collections AS cc ON cc.collection_uuid = c.uuid
		JOIN ciphers AS i
			ON i.uuid = cc.cipher_uuid AND i.organization_uuid = o.uuid
		WHERE uo.uuid = ?`, syntheticMembershipID).Scan(
		&email, &organization, &collection, &cipher, &accessAll, &manage,
	)
	if err != nil {
		t.Fatal(err)
	}
	if email != "member@example.test" || organization != "Synthetic Organization" ||
		collection != "Synthetic Collection" || cipher != "organization cipher" ||
		accessAll || !manage {
		t.Fatalf(
			"organization relationship fixture = %q/%q/%q/%q/%t/%t",
			email, organization, collection, cipher, accessAll, manage,
		)
	}
}

func TestSyntheticVaultwardenIncludesOrganizationRelationships(t *testing.T) {
	dataDir := t.TempDir()
	database := createSyntheticVaultwarden(t, dataDir)
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	assertSyntheticOrganizationFixture(t, filepath.Join(dataDir, "db.sqlite3"))
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
