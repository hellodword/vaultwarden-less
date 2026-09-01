package snapshot

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hellodword/vaultwarden-less/internal/upstream"
	"github.com/mattn/go-sqlite3"
)

const (
	testCipherID             = "11111111-1111-4111-8111-111111111111"
	testAttachmentID         = "0123456789abcdefabcd"
	testSendID               = "22222222-2222-4222-8222-222222222222"
	testUserID               = "44444444-4444-4444-8444-444444444444"
	testOrganizationID       = "55555555-5555-4555-8555-555555555555"
	testMembershipID         = "66666666-6666-4666-8666-666666666666"
	testCollectionID         = "77777777-7777-4777-8777-777777777777"
	testOrganizationCipherID = "88888888-8888-4888-8888-888888888888"
	testProjectCommit        = "0123456789abcdef0123456789abcdef01234567"
)

var testSendFileID = strings.Repeat("b", 64)

type fixture struct {
	dataDir        string
	database       *sql.DB
	attachmentPath string
	sendPath       string
	attachmentData []byte
	sendData       []byte
}

func syntheticData(t *testing.T, includeFiles bool) fixture {
	t.Helper()
	dataDir := filepath.Join(t.TempDir(), "data")
	if err := os.Mkdir(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	databasePath := filepath.Join(dataDir, "db.sqlite3")
	database, err := sql.Open("sqlite3", "file:"+databasePath+"?_busy_timeout=5000&_foreign_keys=on")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	contract, err := upstream.VaultwardenSchemaContract()
	if err != nil {
		t.Fatal(err)
	}
	statements := []string{"PRAGMA journal_mode=WAL", "PRAGMA foreign_keys=ON"}
	for _, object := range contract.Objects {
		statements = append(statements, object.SQL)
	}
	for _, statement := range statements {
		if _, err := database.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	attachmentData := []byte("encrypted-attachment")
	sendData := []byte("encrypted-file-send")
	inserts := []struct {
		query string
		args  []any
	}{
		{"INSERT INTO __diesel_schema_migrations(version, run_on) VALUES (?, '2026-05-05')", []any{contract.DatabaseMigration}},
		{`INSERT INTO users(
			uuid, created_at, updated_at, email, name, password_hash, salt,
			password_iterations, akey, security_stamp, equivalent_domains, excluded_globals
		) VALUES (?, '2026-05-05', '2026-05-05', 'member@example.test', 'member', x'01', x'02', 600000, 'user-key', 'stamp', '[]', '[]')`, []any{testUserID}},
		{"INSERT INTO organizations(uuid, name, billing_email) VALUES (?, 'Synthetic Organization', 'billing@example.test')", []any{testOrganizationID}},
		{`INSERT INTO users_organizations(
			uuid, user_uuid, org_uuid, access_all, akey, status, atype
		) VALUES (?, ?, ?, 0, 'membership-key', 2, 2)`, []any{testMembershipID, testUserID, testOrganizationID}},
		{"INSERT INTO collections(uuid, org_uuid, name) VALUES (?, ?, 'Synthetic Collection')", []any{testCollectionID, testOrganizationID}},
		{"INSERT INTO users_collections(user_uuid, collection_uuid, read_only, hide_passwords, manage) VALUES (?, ?, 0, 0, 1)", []any{testUserID, testCollectionID}},
		{"INSERT INTO ciphers(uuid, created_at, updated_at, user_uuid, atype, name, data) VALUES (?, '2026-05-05', '2026-05-05', ?, 2, 'cipher', '{}')", []any{testCipherID, testUserID}},
		{"INSERT INTO ciphers(uuid, created_at, updated_at, organization_uuid, atype, name, data) VALUES (?, '2026-05-05', '2026-05-05', ?, 2, 'organization cipher', '{}')", []any{testOrganizationCipherID, testOrganizationID}},
		{"INSERT INTO ciphers_collections(cipher_uuid, collection_uuid) VALUES (?, ?)", []any{testOrganizationCipherID, testCollectionID}},
		{"INSERT INTO attachments(id, cipher_uuid, file_name, file_size) VALUES (?, ?, 'encrypted', ?)", []any{testAttachmentID, testCipherID, len(attachmentData)}},
		{`INSERT INTO sends(
			uuid, name, atype, data, akey, access_count, creation_date,
			revision_date, deletion_date, disabled
		) VALUES (?, 'file send', 1, ?, 'key', 0, '2026-05-05', '2026-05-05', '2026-06-05', 0)`, []any{testSendID, `{"id":"` + testSendFileID + `","size":` + strconv.Itoa(len(sendData)) + `,"sizeName":"18 Bytes","fileName":"encrypted"}`}},
		{`INSERT INTO sends(
			uuid, name, atype, data, akey, access_count, creation_date,
			revision_date, deletion_date, disabled
		) VALUES ('33333333-3333-4333-8333-333333333333', 'text send', 0, 'text sends need no file and are not parsed', 'key', 0, '2026-05-05', '2026-05-05', '2026-06-05', 0)`, nil},
	}
	for _, insert := range inserts {
		if _, err := database.Exec(insert.query, insert.args...); err != nil {
			t.Fatal(err)
		}
	}
	attachmentPath := filepath.Join(dataDir, "attachments", testCipherID, testAttachmentID)
	sendPath := filepath.Join(dataDir, "sends", testSendID, testSendFileID)
	if includeFiles {
		writeFixture(t, attachmentPath, attachmentData)
		writeFixture(t, sendPath, sendData)
	}
	writeFixture(t, filepath.Join(dataDir, "rsa_key.pem"), []byte("synthetic-rsa"))
	writeFixture(t, filepath.Join(dataDir, "rsa_key.pub.pem"), []byte("synthetic-rsa-public"))
	writeFixture(t, filepath.Join(dataDir, "config.json"), []byte("{}"))
	writeFixture(t, filepath.Join(dataDir, "icon_cache", "ignored"), []byte("ignore"))
	writeFixture(t, filepath.Join(dataDir, "tmp", "ignored"), []byte("ignore"))
	writeFixture(t, filepath.Join(dataDir, "backups", "ignored"), []byte("ignore"))
	writeFixture(t, filepath.Join(dataDir, "attachments", testCipherID, strings.Repeat("f", 20)), []byte("orphan"))
	return fixture{
		dataDir:        dataDir,
		database:       database,
		attachmentPath: attachmentPath,
		sendPath:       sendPath,
		attachmentData: attachmentData,
		sendData:       sendData,
	}
}

func writeFixture(t *testing.T, path string, data []byte) {
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

func privateDirectory(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "staging")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func emptyCurrentData(t *testing.T) string {
	t.Helper()
	dataDir := filepath.Join(t.TempDir(), "data")
	if err := os.Mkdir(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	database, err := sql.Open("sqlite3", filepath.Join(dataDir, "db.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	contract, err := upstream.VaultwardenSchemaContract()
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range append([]string{"PRAGMA journal_mode=WAL", "PRAGMA foreign_keys=ON"}, objectSQL(contract.Objects)...) {
		if _, err := database.Exec(statement); err != nil {
			_ = database.Close()
			t.Fatal(err)
		}
	}
	if _, err := database.Exec("INSERT INTO __diesel_schema_migrations(version, run_on) VALUES (?, '2026-05-05')", contract.DatabaseMigration); err != nil {
		_ = database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	return dataDir
}

func objectSQL(objects []upstream.SchemaObject) []string {
	statements := make([]string, 0, len(objects))
	for _, object := range objects {
		statements = append(statements, object.SQL)
	}
	return statements
}

func buildFixture(t *testing.T, dataDir string) (Manifest, string) {
	t.Helper()
	stagingDir := privateDirectory(t)
	manifest, err := (Builder{
		DataDir: dataDir, StagingDir: stagingDir, ProjectCommit: testProjectCommit,
	}).Build(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return manifest, filepath.Join(stagingDir, "point")
}

func assertOrganizationFixture(t *testing.T, databasePath string) {
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
		WHERE uo.uuid = ?`, testMembershipID).Scan(
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

func rewriteManifest(t *testing.T, point string, mutate func(*Manifest)) {
	t.Helper()
	path := filepath.Join(point, "manifest.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := DecodeManifest(data)
	if err != nil {
		t.Fatal(err)
	}
	mutate(&manifest)
	data, err = json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

func insertProjectionExclusionFixture(t *testing.T, database *sql.DB) {
	t.Helper()
	const userID = "66666666-6666-4666-8666-666666666666"
	if _, err := database.Exec(`INSERT INTO users(
		uuid, created_at, updated_at, email, name, password_hash, salt,
		password_iterations, akey, security_stamp, equivalent_domains, excluded_globals
	) VALUES (?, '2026-05-05', '2026-05-05', 'user@example.test', 'user', x'01', x'02', 100000, 'key', 'stamp', '[]', '[]')`, userID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec("INSERT INTO twofactor(uuid, user_uuid, atype, enabled, data, last_used) VALUES ('77777777-7777-4777-8777-777777777777', ?, 1, 1, 'secret', 0)", userID); err != nil {
		t.Fatal(err)
	}
}

func TestBuilderCreatesAndVerifiesExactRecoverySet(t *testing.T) {
	data := syntheticData(t, true)
	stagingDir := privateDirectory(t)
	manifest, err := (Builder{DataDir: data.dataDir, StagingDir: stagingDir, ProjectCommit: testProjectCommit}).Build(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	point := filepath.Join(stagingDir, "point")
	verified, err := Verify(context.Background(), point)
	if err != nil {
		t.Fatal(err)
	}
	assertOrganizationFixture(t, filepath.Join(point, "db.sqlite3"))
	paths := make([]string, 0, len(verified.Files))
	for _, file := range verified.Files {
		paths = append(paths, file.Path)
	}
	want := []string{
		"attachments/" + testCipherID + "/" + testAttachmentID,
		"db.sqlite3",
		"sends/" + testSendID + "/" + testSendFileID,
	}
	if !slices.Equal(paths, want) {
		t.Fatalf("recovery paths = %#v", paths)
	}
	for _, excluded := range []string{"rsa_key", "config.json", "icon_cache", "tmp", "backups", "-wal", "-shm", "journal", strings.Repeat("f", 20)} {
		for _, path := range paths {
			if strings.Contains(path, excluded) {
				t.Fatalf("excluded path %q appeared as %q", excluded, path)
			}
		}
	}
	second, err := (Builder{DataDir: data.dataDir, StagingDir: stagingDir, ProjectCommit: testProjectCommit}).Build(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if second.ContentID != manifest.ContentID {
		t.Fatal("unchanged source produced a different content ID")
	}
}

func TestVerifyOwnedBindsTheCompleteTreeToTheExpectedIdentity(t *testing.T) {
	data := syntheticData(t, true)
	_, point := buildFixture(t, data.dataDir)
	uid, gid := uint32(os.Geteuid()), uint32(os.Getegid())
	if _, err := VerifyOwned(context.Background(), point, uid, gid); err != nil {
		t.Fatalf("verify explicit owner: %v", err)
	}
	wrongUID := uid + 1
	if wrongUID == 0 {
		wrongUID = uid - 1
	}
	if _, err := VerifyOwned(context.Background(), point, wrongUID, gid); err == nil {
		t.Fatal("recovery point owned by a different UID was accepted")
	}
	wrongGID := gid + 1
	if wrongGID == 0 {
		wrongGID = gid - 1
	}
	if _, err := VerifyOwned(context.Background(), point, uid, wrongGID); err == nil {
		t.Fatal("recovery point owned by a different GID was accepted")
	}
}

func TestBuilderAcceptsEmptyCurrentSchema(t *testing.T) {
	manifest, point := buildFixture(t, emptyCurrentData(t))
	if len(manifest.Files) != 1 || manifest.Files[0].Path != "db.sqlite3" {
		t.Fatalf("empty recovery files = %#v", manifest.Files)
	}
	if _, err := Verify(context.Background(), point); err != nil {
		t.Fatal(err)
	}
}

func TestCanonicalIdentityExcludesSessionStateAndReviewedColumns(t *testing.T) {
	data := syntheticData(t, true)
	insertProjectionExclusionFixture(t, data.database)
	first, _ := buildFixture(t, data.dataDir)
	if _, err := data.database.Exec(`UPDATE users SET
		updated_at = '2030-01-01', last_verifying_at = '2030-01-01',
		login_verify_count = 9, stamp_exception = 'temporary'
		WHERE email = 'user@example.test'`); err != nil {
		t.Fatal(err)
	}
	if _, err := data.database.Exec("UPDATE twofactor SET last_used = 99"); err != nil {
		t.Fatal(err)
	}
	if _, err := data.database.Exec("INSERT INTO event(uuid, event_type, event_date) VALUES ('88888888-8888-4888-8888-888888888888', 1, '2030-01-01')"); err != nil {
		t.Fatal(err)
	}
	second, _ := buildFixture(t, data.dataDir)
	if second.ContentID != first.ContentID {
		t.Fatalf("excluded/session mutation changed content ID: %s != %s", second.ContentID, first.ContentID)
	}
	if second.ArchiveID == first.ArchiveID {
		t.Fatal("raw database mutation did not change archive ID")
	}
	if _, err := data.database.Exec("UPDATE users SET name = 'durable-change' WHERE email = 'user@example.test'"); err != nil {
		t.Fatal(err)
	}
	third, _ := buildFixture(t, data.dataDir)
	if third.ContentID == second.ContentID {
		t.Fatal("included durable mutation did not change content ID")
	}
	writeFixture(t, data.attachmentPath, []byte("encrypted-attachmenU"))
	if _, err := data.database.Exec("UPDATE attachments SET file_size = ? WHERE id = ?", len("encrypted-attachmenU"), testAttachmentID); err != nil {
		t.Fatal(err)
	}
	fourth, _ := buildFixture(t, data.dataDir)
	if fourth.ContentID == third.ContentID {
		t.Fatal("referenced file mutation did not change content ID")
	}
}

func TestCanonicalIdentityIgnoresInsertionOrderPageLayoutAndVacuum(t *testing.T) {
	firstData := syntheticData(t, true)
	secondData := syntheticData(t, true)
	rows := [][2]string{
		{"99999999-9999-4999-8999-999999999991", "alpha"},
		{"99999999-9999-4999-8999-999999999992", "omega"},
	}
	insertCipher := func(t *testing.T, database *sql.DB, row [2]string) {
		t.Helper()
		if _, err := database.Exec(`INSERT INTO ciphers(
			uuid, created_at, updated_at, atype, name, data
		) VALUES (?, '2026-05-05', '2026-05-05', 2, ?, '{}')`, row[0], row[1]); err != nil {
			t.Fatal(err)
		}
	}
	insertCipher(t, firstData.database, rows[0])
	insertCipher(t, firstData.database, rows[1])
	insertCipher(t, secondData.database, rows[1])
	insertCipher(t, secondData.database, rows[0])
	first, _ := buildFixture(t, firstData.dataDir)
	if _, err := secondData.database.Exec("PRAGMA journal_mode=DELETE"); err != nil {
		t.Fatal(err)
	}
	if _, err := secondData.database.Exec("PRAGMA page_size=8192"); err != nil {
		t.Fatal(err)
	}
	if _, err := secondData.database.Exec("VACUUM"); err != nil {
		t.Fatal(err)
	}
	var pageSize int
	if err := secondData.database.QueryRow("PRAGMA page_size").Scan(&pageSize); err != nil || pageSize != 8192 {
		t.Fatalf("second database page size = %d/%v", pageSize, err)
	}
	second, _ := buildFixture(t, secondData.dataDir)
	if second.ContentID != first.ContentID {
		t.Fatalf("physical layout changed content ID: %s != %s", second.ContentID, first.ContentID)
	}
	if second.ArchiveID == first.ArchiveID {
		t.Fatal("different SQLite page layouts produced the same archive ID")
	}
	if _, err := secondData.database.Exec("VACUUM"); err != nil {
		t.Fatal(err)
	}
	afterVacuum, _ := buildFixture(t, secondData.dataDir)
	if afterVacuum.ContentID != second.ContentID {
		t.Fatal("VACUUM changed logical content ID")
	}
}

func TestCanonicalPrimitiveEncodingGoldenBytes(t *testing.T) {
	t.Parallel()
	var encoded bytes.Buffer
	for _, operation := range []func() error{
		func() error { return writeFrame(&encoded, []byte("A")) },
		func() error { return writeSQLiteValue(&encoded, "null", nil) },
		func() error { return writeSQLiteValue(&encoded, "integer", int64(-2)) },
		func() error { return writeSQLiteValue(&encoded, "real", 1.5) },
		func() error { return writeSQLiteValue(&encoded, "text", []byte("x")) },
		func() error { return writeSQLiteValue(&encoded, "blob", []byte{0, 0xff}) },
	} {
		if err := operation(); err != nil {
			t.Fatal(err)
		}
	}
	want := "00000000000000014100" +
		"01fffffffffffffffe" +
		"023ff8000000000000" +
		"03000000000000000178" +
		"04000000000000000200ff"
	if got := hex.EncodeToString(encoded.Bytes()); got != want {
		t.Fatalf("canonical bytes = %s, want %s", got, want)
	}
}

func TestCanonicalFullStreamGoldenDigest(t *testing.T) {
	data := syntheticData(t, true)
	manifest, _ := buildFixture(t, data.dataDir)
	const want = "sha256:baa6784dc9d46674f0d5f08a72d4535f048f5a3f35bc6467230d158ea29234d5"
	if manifest.ContentID != want {
		t.Fatalf("canonical content ID = %s, want %s", manifest.ContentID, want)
	}
}

func TestOnlineBackupCapturesOneConsistentWALTransactionDuringWrites(t *testing.T) {
	data := syntheticData(t, true)
	largePayload := strings.Repeat("x", 34<<20)
	if _, err := data.database.Exec(
		"UPDATE ciphers SET data = ?, name = 'generation-0' WHERE uuid = ?",
		largePayload,
		testCipherID,
	); err != nil {
		t.Fatal(err)
	}
	secondCipherID := "55555555-5555-4555-8555-555555555555"
	if _, err := data.database.Exec(
		`INSERT INTO ciphers(
			uuid, created_at, updated_at, atype, name, data
		) VALUES (?, '2026-05-05', '2026-05-05', 2, 'generation-0', '{}')`,
		secondCipherID,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := data.database.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		t.Fatal(err)
	}
	databasePath := filepath.Join(data.dataDir, "db.sqlite3")
	if info, err := os.Stat(databasePath); err != nil || info.Size() < 32<<20 {
		t.Fatalf("WAL source database is smaller than 32 MiB: %v/%v", info, err)
	}

	startWriter := make(chan struct{})
	writerDone := make(chan error, 1)
	commitTimes := make(chan time.Time, 12)
	var committed atomic.Int64
	go func() {
		defer close(commitTimes)
		<-startWriter
		time.Sleep(10 * time.Millisecond)
		for generation := int64(1); generation <= 12; generation++ {
			transaction, err := data.database.Begin()
			if err != nil {
				writerDone <- err
				return
			}
			marker := fmt.Sprintf("generation-%d", generation)
			if _, err := transaction.Exec(
				"UPDATE ciphers SET name = ?, notes = ? WHERE uuid = ?",
				marker, marker, testCipherID,
			); err != nil {
				_ = transaction.Rollback()
				writerDone <- err
				return
			}
			time.Sleep(200 * time.Microsecond)
			if _, err := transaction.Exec(
				"UPDATE ciphers SET name = ?, notes = ? WHERE uuid = ?",
				marker, marker, secondCipherID,
			); err != nil {
				_ = transaction.Rollback()
				writerDone <- err
				return
			}
			if err := transaction.Commit(); err != nil {
				writerDone <- err
				return
			}
			commitTimes <- time.Now()
			committed.Add(1)
			time.Sleep(5 * time.Millisecond)
		}
		writerDone <- nil
	}()
	close(startWriter)
	deadline := time.Now().Add(2 * time.Second)
	for committed.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if committed.Load() == 0 {
		if writerError := <-writerDone; writerError != nil {
			t.Fatal(writerError)
		}
		t.Fatal("concurrent WAL writer did not establish an initial marker")
	}
	buildStarted := time.Now()
	stagingDir := privateDirectory(t)
	_, err := (Builder{
		DataDir: data.dataDir, StagingDir: stagingDir, ProjectCommit: testProjectCommit,
	}).Build(context.Background())
	buildFinished := time.Now()
	writerError := <-writerDone
	if writerError != nil {
		t.Fatal(writerError)
	}
	if err != nil {
		t.Fatal(err)
	}
	overlapped := false
	for committedAt := range commitTimes {
		if !committedAt.Before(buildStarted) && !committedAt.After(buildFinished) {
			overlapped = true
		}
	}
	if !overlapped {
		t.Fatal("concurrent WAL writer did not commit during online backup")
	}

	recoveryDatabase := filepath.Join(stagingDir, "point", "db.sqlite3")
	if info, err := os.Stat(recoveryDatabase); err != nil || info.Size() < 32<<20 {
		t.Fatalf("recovery database is smaller than 32 MiB: %v/%v", info, err)
	}
	validation, err := validateDatabase(context.Background(), recoveryDatabase)
	if err != nil {
		t.Fatal(err)
	}
	contract, err := upstream.VaultwardenSchemaContract()
	if err != nil {
		t.Fatal(err)
	}
	if validation.Migration != contract.DatabaseMigration ||
		validation.SchemaFingerprint != contract.Fingerprint {
		t.Fatalf("recovery database contract = %#v", validation)
	}
	recovered, err := sql.Open("sqlite3", sqliteURI(recoveryDatabase, "ro"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = recovered.Close() }()
	rows, err := recovered.Query(
		"SELECT name, notes FROM ciphers WHERE uuid IN (?, ?) ORDER BY uuid",
		testCipherID,
		secondCipherID,
	)
	if err != nil {
		t.Fatal(err)
	}
	markers := make([][2]string, 0, 2)
	for rows.Next() {
		var marker [2]string
		if err := rows.Scan(&marker[0], &marker[1]); err != nil {
			t.Fatal(err)
		}
		markers = append(markers, marker)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	if len(markers) != 2 || markers[0][0] != markers[1][0] ||
		markers[0][0] != markers[0][1] || markers[1][0] != markers[1][1] {
		t.Fatalf("online backup captured a torn marker transaction: %#v", markers)
	}
	var journalMode, integrity string
	if err := recovered.QueryRow("PRAGMA journal_mode").Scan(&journalMode); err != nil {
		t.Fatal(err)
	}
	if err := recovered.QueryRow("PRAGMA integrity_check").Scan(&integrity); err != nil {
		t.Fatal(err)
	}
	if journalMode != "delete" || integrity != "ok" {
		t.Fatalf("recovery journal/integrity = %q/%q", journalMode, integrity)
	}
	var foreignKeyViolations int
	if err := recovered.QueryRow("SELECT count(*) FROM pragma_foreign_key_check").Scan(&foreignKeyViolations); err != nil {
		t.Fatal(err)
	}
	if foreignKeyViolations != 0 {
		t.Fatalf("recovery has %d foreign-key violations", foreignKeyViolations)
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if _, err := os.Lstat(recoveryDatabase + suffix); !os.IsNotExist(err) {
			t.Fatalf("recovery retained SQLite sidecar %s: %v", suffix, err)
		}
	}
}

func TestPrepareStoppedDatabaseCheckpointsPersistentWAL(t *testing.T) {
	data := syntheticData(t, true)
	data.database.SetMaxOpenConns(1)
	data.database.SetMaxIdleConns(1)
	connection, err := data.database.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := connection.Raw(func(raw any) error {
		sqlite, ok := raw.(*sqlite3.SQLiteConn)
		if !ok {
			return fmt.Errorf("unexpected SQLite driver")
		}
		return sqlite.SetFileControlInt("main", sqlite3.SQLITE_FCNTL_PERSIST_WAL, 1)
	}); err != nil {
		_ = connection.Close()
		t.Fatal(err)
	}
	const marker = "stopped-wal-marker"
	if _, err := connection.ExecContext(
		context.Background(),
		"UPDATE ciphers SET name = ? WHERE uuid = ?",
		marker,
		testCipherID,
	); err != nil {
		_ = connection.Close()
		t.Fatal(err)
	}
	if err := connection.Close(); err != nil {
		t.Fatal(err)
	}
	if err := data.database.Close(); err != nil {
		t.Fatal(err)
	}
	databasePath := filepath.Join(data.dataDir, "db.sqlite3")
	for _, suffix := range []string{"", "-wal", "-shm"} {
		path := databasePath + suffix
		if info, err := os.Lstat(path); err != nil || !info.Mode().IsRegular() {
			t.Fatalf("persistent SQLite file %q = %v/%v", suffix, info, err)
		}
		if err := os.Chmod(path, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if err := PrepareStoppedDatabase(context.Background(), databasePath); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if _, err := os.Lstat(databasePath + suffix); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("prepared database retained %s: %v", suffix, err)
		}
	}
	prepared, err := sql.Open("sqlite3", sqliteURI(databasePath, "ro"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = prepared.Close() }()
	var gotMarker, journalMode string
	if err := prepared.QueryRow("SELECT name FROM ciphers WHERE uuid = ?", testCipherID).Scan(&gotMarker); err != nil {
		t.Fatal(err)
	}
	if err := prepared.QueryRow("PRAGMA journal_mode").Scan(&journalMode); err != nil {
		t.Fatal(err)
	}
	if gotMarker != marker || journalMode != "delete" {
		t.Fatalf("prepared database marker/journal = %q/%q", gotMarker, journalMode)
	}
}

func TestPrepareStoppedDatabaseRejectsLiveReaderWithoutRemovingWAL(t *testing.T) {
	data := syntheticData(t, true)
	reader, err := data.database.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Rollback() }()
	var count int
	if err := reader.QueryRow("SELECT count(*) FROM ciphers").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if _, err := data.database.Exec(
		"UPDATE ciphers SET name = 'after-reader' WHERE uuid = ?",
		testCipherID,
	); err != nil {
		t.Fatal(err)
	}
	databasePath := filepath.Join(data.dataDir, "db.sqlite3")
	ctx, cancel := context.WithTimeout(context.Background(), 7*time.Second)
	defer cancel()
	if err := PrepareStoppedDatabase(ctx, databasePath); err == nil {
		t.Fatal("database with a live reader was prepared")
	}
	if info, err := os.Lstat(databasePath + "-wal"); err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
		t.Fatalf("live WAL was removed or replaced: %v/%v", info, err)
	}
	var marker string
	if err := data.database.QueryRow("SELECT name FROM ciphers WHERE uuid = ?", testCipherID).Scan(&marker); err != nil {
		t.Fatal(err)
	}
	if marker != "after-reader" {
		t.Fatalf("live database marker = %q", marker)
	}
}

func TestPrepareStoppedDatabaseRejectsUnsafeOrCorruptInput(t *testing.T) {
	t.Run("corrupt", func(t *testing.T) {
		dataDir := filepath.Join(t.TempDir(), "data")
		if err := os.Mkdir(dataDir, 0o700); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dataDir, "db.sqlite3")
		original := []byte("not a SQLite database")
		writeFixture(t, path, original)
		if err := PrepareStoppedDatabase(context.Background(), path); err == nil {
			t.Fatal("corrupt database was prepared")
		}
		if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, original) {
			t.Fatalf("corrupt database changed = %q/%v", got, err)
		}
	})
	t.Run("symlink", func(t *testing.T) {
		root := t.TempDir()
		dataDir := filepath.Join(root, "data")
		if err := os.Mkdir(dataDir, 0o700); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(root, "target")
		writeFixture(t, target, []byte("target"))
		path := filepath.Join(dataDir, "db.sqlite3")
		if err := os.Symlink(target, path); err != nil {
			t.Fatal(err)
		}
		if err := PrepareStoppedDatabase(context.Background(), path); err == nil {
			t.Fatal("symlink database was prepared")
		}
		if got, err := os.ReadFile(target); err != nil || string(got) != "target" {
			t.Fatalf("symlink target changed = %q/%v", got, err)
		}
	})
	t.Run("hard link", func(t *testing.T) {
		root := t.TempDir()
		dataDir := filepath.Join(root, "data")
		if err := os.Mkdir(dataDir, 0o700); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(root, "target")
		writeFixture(t, target, []byte("target"))
		path := filepath.Join(dataDir, "db.sqlite3")
		if err := os.Link(target, path); err != nil {
			t.Fatal(err)
		}
		if err := PrepareStoppedDatabase(context.Background(), path); err == nil {
			t.Fatal("hard-linked database was prepared")
		}
	})
}

func TestBuilderRejectsMissingAttachmentAndSendWithoutPublishing(t *testing.T) {
	for name, removeOther := range map[string]bool{"attachment": false, "send": true} {
		t.Run(name, func(t *testing.T) {
			data := syntheticData(t, false)
			if removeOther {
				writeFixture(t, data.attachmentPath, data.attachmentData)
			} else {
				writeFixture(t, data.sendPath, data.sendData)
			}
			stagingDir := privateDirectory(t)
			_, err := (Builder{DataDir: data.dataDir, StagingDir: stagingDir, ProjectCommit: testProjectCommit}).Build(context.Background())
			var consistency *ConsistencyError
			if !errors.As(err, &consistency) || consistency.Reason != "not_found" {
				t.Fatalf("error = %v", err)
			}
			if _, err := os.Lstat(filepath.Join(stagingDir, "point")); !os.IsNotExist(err) {
				t.Fatal("failed build published a point")
			}
			builds, err := filepath.Glob(filepath.Join(stagingDir, ".build-*"))
			if err != nil || len(builds) != 0 {
				t.Fatalf("transient builds = %#v, %v", builds, err)
			}
		})
	}
}

func TestTwoStageSendWindowPreservesPreviousPoint(t *testing.T) {
	data := syntheticData(t, true)
	stagingDir := privateDirectory(t)
	builder := Builder{DataDir: data.dataDir, StagingDir: stagingDir, ProjectCommit: testProjectCommit}
	first, err := builder.Build(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	newSendID := "44444444-4444-4444-8444-444444444444"
	newFileID := strings.Repeat("c", 64)
	if _, err := data.database.Exec(
		`INSERT INTO sends(
			uuid, name, atype, data, akey, access_count, creation_date,
			revision_date, deletion_date, disabled
		) VALUES (?, 'missing file', 1, ?, 'key', 0, '2026-05-05', '2026-05-05', '2026-06-05', 0)`,
		newSendID,
		`{"id":"`+newFileID+`","size":12}`,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := builder.Build(context.Background()); err == nil {
		t.Fatal("missing second-stage Send file was accepted")
	}
	current, err := Verify(context.Background(), filepath.Join(stagingDir, "point"))
	if err != nil {
		t.Fatal(err)
	}
	if current.ContentID != first.ContentID {
		t.Fatal("failed build replaced the previous recovery point")
	}
}

func TestBuilderRejectsSizeDeletionSymlinkAndChangedRaces(t *testing.T) {
	tests := map[string]struct {
		before func(t *testing.T, data fixture)
		after  func(t *testing.T, data fixture)
	}{
		"size mismatch": {
			before: func(t *testing.T, data fixture) {
				writeFixture(t, data.attachmentPath, append(data.attachmentData, 'x'))
			},
		},
		"deleted": {
			before: func(t *testing.T, data fixture) {
				if err := os.Remove(data.attachmentPath); err != nil {
					t.Fatal(err)
				}
			},
		},
		"symlink": {
			before: func(t *testing.T, data fixture) {
				if err := os.Remove(data.attachmentPath); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(data.sendPath, data.attachmentPath); err != nil {
					t.Fatal(err)
				}
			},
		},
		"symlink ancestor": {
			before: func(t *testing.T, data fixture) {
				cipherDirectory := filepath.Dir(data.attachmentPath)
				external := filepath.Join(t.TempDir(), "external")
				writeFixture(t, filepath.Join(external, testAttachmentID), data.attachmentData)
				if err := os.RemoveAll(cipherDirectory); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(external, cipherDirectory); err != nil {
					t.Fatal(err)
				}
			},
		},
		"changed during copy": {
			after: func(t *testing.T, data fixture) {
				changed := slices.Clone(data.attachmentData)
				changed[0] ^= 1
				writeFixture(t, data.attachmentPath, changed)
			},
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			data := syntheticData(t, true)
			stagingDir := privateDirectory(t)
			builder := Builder{DataDir: data.dataDir, StagingDir: stagingDir, ProjectCommit: testProjectCommit}
			runBefore := true
			runAfter := true
			builder.beforeCopy = func(path string) {
				if runBefore && path == "attachments/"+testCipherID+"/"+testAttachmentID && test.before != nil {
					runBefore = false
					test.before(t, data)
				}
			}
			builder.afterCopy = func(path string) {
				if runAfter && path == "attachments/"+testCipherID+"/"+testAttachmentID && test.after != nil {
					runAfter = false
					test.after(t, data)
				}
			}
			if _, err := builder.Build(context.Background()); err == nil {
				t.Fatal("unsafe source race was accepted")
			}
			if _, err := os.Lstat(filepath.Join(stagingDir, "point")); !os.IsNotExist(err) {
				t.Fatal("unsafe source race published a point")
			}
		})
	}
}

func TestBuilderRejectsUnsupportedMigrationAndUnsafeReferences(t *testing.T) {
	t.Run("migration", func(t *testing.T) {
		data := syntheticData(t, true)
		if _, err := data.database.Exec("DELETE FROM __diesel_schema_migrations"); err != nil {
			t.Fatal(err)
		}
		if _, err := data.database.Exec("INSERT INTO __diesel_schema_migrations(version, run_on) VALUES ('20260505120001', 'future')"); err != nil {
			t.Fatal(err)
		}
		if _, err := (Builder{DataDir: data.dataDir, StagingDir: privateDirectory(t), ProjectCommit: testProjectCommit}).Build(context.Background()); err == nil {
			t.Fatal("unsupported migration was accepted")
		}
	})
	for name, value := range map[string]string{
		"send id":                `{"id":"short","size":1,"sizeName":"1 Byte","fileName":"x"}`,
		"send size":              `{"id":"` + testSendFileID + `","size":"18","sizeName":"18 Bytes","fileName":"x"}`,
		"send JSON":              `{invalid`,
		"send unknown field":     `{"id":"` + testSendFileID + `","size":18,"sizeName":"18 Bytes","fileName":"x","extra":true}`,
		"send duplicate key":     `{"id":"` + testSendFileID + `","id":"` + testSendFileID + `","size":18,"sizeName":"18 Bytes","fileName":"x"}`,
		"send missing name":      `{"id":"` + testSendFileID + `","size":18,"sizeName":"18 Bytes"}`,
		"send missing size name": `{"id":"` + testSendFileID + `","size":18,"fileName":"x"}`,
		"send size name type":    `{"id":"` + testSendFileID + `","size":18,"sizeName":18,"fileName":"x"}`,
	} {
		t.Run(name, func(t *testing.T) {
			data := syntheticData(t, true)
			if _, err := data.database.Exec("UPDATE sends SET data = ? WHERE uuid = ?", value, testSendID); err != nil {
				t.Fatal(err)
			}
			if _, err := (Builder{DataDir: data.dataDir, StagingDir: privateDirectory(t), ProjectCommit: testProjectCommit}).Build(context.Background()); err == nil {
				t.Fatal("unsafe file Send reference was accepted")
			}
		})
	}
	t.Run("path escape", func(t *testing.T) {
		data := syntheticData(t, true)
		if _, err := data.database.Exec("UPDATE sends SET uuid = '../escape' WHERE uuid = ?", testSendID); err != nil {
			t.Fatal(err)
		}
		if _, err := (Builder{DataDir: data.dataDir, StagingDir: privateDirectory(t), ProjectCommit: testProjectCommit}).Build(context.Background()); err == nil {
			t.Fatal("path-escaping Send reference was accepted")
		}
	})
}

func TestBuilderRejectsSchemaDriftAtTheSameMigration(t *testing.T) {
	for name, statement := range map[string]string{
		"unknown table":  "CREATE TABLE unexpected_history_residue (id TEXT PRIMARY KEY)",
		"unknown column": "ALTER TABLE ciphers ADD COLUMN unexpected_history_residue TEXT",
	} {
		t.Run(name, func(t *testing.T) {
			data := syntheticData(t, true)
			if _, err := data.database.Exec(statement); err != nil {
				t.Fatal(err)
			}
			if _, err := (Builder{DataDir: data.dataDir, StagingDir: privateDirectory(t), ProjectCommit: testProjectCommit}).Build(context.Background()); err == nil {
				t.Fatal("database schema drift was accepted")
			}
		})
	}
}

func TestVerifyRejectsEveryTreeMismatch(t *testing.T) {
	tests := map[string]func(t *testing.T, point string){
		"extra file": func(t *testing.T, point string) {
			writeFixture(t, filepath.Join(point, "attachments", testCipherID, strings.Repeat("e", 20)), []byte("extra"))
		},
		"missing file": func(t *testing.T, point string) {
			if err := os.Remove(filepath.Join(point, "sends", testSendID, testSendFileID)); err != nil {
				t.Fatal(err)
			}
		},
		"changed file": func(t *testing.T, point string) {
			path := filepath.Join(point, "attachments", testCipherID, testAttachmentID)
			writeFixture(t, path, []byte("tampered-attachment"))
		},
		"extra directory": func(t *testing.T, point string) {
			if err := os.Mkdir(filepath.Join(point, "unexpected"), 0o700); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			data := syntheticData(t, true)
			stagingDir := privateDirectory(t)
			_, err := (Builder{DataDir: data.dataDir, StagingDir: stagingDir, ProjectCommit: testProjectCommit}).Build(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			point := filepath.Join(stagingDir, "point")
			mutate(t, point)
			if _, err := Verify(context.Background(), point); err == nil {
				t.Fatal("mismatched recovery tree was accepted")
			}
		})
	}
}

func TestManifestSortsAndValidatesExactFields(t *testing.T) {
	t.Parallel()
	files := []File{
		{Path: "sends/" + testSendID + "/" + testSendFileID, Size: 1, SHA256: "sha256:" + strings.Repeat("a", 64)},
		{Path: "db.sqlite3", Size: 2, SHA256: "sha256:" + strings.Repeat("b", 64)},
	}
	contentID := "sha256:" + strings.Repeat("c", 64)
	archiveID := "sha256:" + strings.Repeat("d", 64)
	first, err := NewManifest(testProjectCommit, contentID, archiveID, files)
	if err != nil {
		t.Fatal(err)
	}
	slices.Reverse(files)
	second, err := NewManifest(testProjectCommit, contentID, archiveID, files)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(first.Files, second.Files) || first.ProjectCommit != testProjectCommit || first.ContentID != contentID || first.ArchiveID != archiveID {
		t.Fatalf("manifest = %#v", first)
	}
}

func TestManifestStrictlyRejectsMalformedJSONAndIdentity(t *testing.T) {
	t.Parallel()
	manifest, err := NewManifest(
		testProjectCommit,
		"sha256:"+strings.Repeat("b", 64),
		"sha256:"+strings.Repeat("c", 64),
		[]File{{Path: "db.sqlite3", Size: 1, SHA256: "sha256:" + strings.Repeat("a", 64)}},
	)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	withUnknown := strings.Replace(string(data), "{", `{"unknown":true,`, 1)
	if _, err := DecodeManifest([]byte(withUnknown)); err == nil {
		t.Fatal("unknown manifest field was accepted")
	}
	duplicate := strings.Replace(string(data), `"project_commit":"`+testProjectCommit+`"`, `"project_commit":"`+testProjectCommit+`","project_commit":"`+testProjectCommit+`"`, 1)
	for name, malformed := range map[string][]byte{
		"duplicate":    []byte(duplicate),
		"invalid utf8": append(slices.Clone(data), 0xff),
		"trailing":     append(slices.Clone(data), []byte(` {}`)...),
	} {
		if _, err := DecodeManifest(malformed); err == nil {
			t.Fatalf("%s manifest was accepted", name)
		}
	}
}

func TestVerifyRejectsEveryManifestIdentityAndFileTamper(t *testing.T) {
	for name, mutate := range map[string]func(*Manifest){
		"project commit": func(manifest *Manifest) {
			manifest.ProjectCommit = strings.Repeat("f", 40)
		},
		"content ID": func(manifest *Manifest) {
			manifest.ContentID = "sha256:" + strings.Repeat("e", 64)
		},
		"archive ID": func(manifest *Manifest) {
			manifest.ArchiveID = "sha256:" + strings.Repeat("d", 64)
		},
		"file size": func(manifest *Manifest) { manifest.Files[0].Size++ },
		"file digest": func(manifest *Manifest) {
			manifest.Files[0].SHA256 = "sha256:" + strings.Repeat("c", 64)
		},
		"file set": func(manifest *Manifest) {
			manifest.Files = manifest.Files[:len(manifest.Files)-1]
		},
		"file order": func(manifest *Manifest) { slices.Reverse(manifest.Files) },
	} {
		t.Run(name, func(t *testing.T) {
			data := syntheticData(t, true)
			_, point := buildFixture(t, data.dataDir)
			rewriteManifest(t, point, mutate)
			if _, err := Verify(context.Background(), point); err == nil {
				t.Fatal("tampered manifest was accepted")
			}
		})
	}
}

func TestExactAttachmentAndSendPathConstraints(t *testing.T) {
	t.Parallel()
	for _, path := range []string{
		"db.sqlite3",
		"attachments/" + testCipherID + "/" + testAttachmentID,
		"sends/" + testSendID + "/" + testSendFileID,
	} {
		if !validManifestPath(path) {
			t.Fatalf("valid path rejected: %s", path)
		}
	}
	for _, path := range []string{
		"rsa_key.pem",
		"attachments/" + testCipherID + "/Attachment-ID-001",
		"attachments/AAAAAAAA-AAAA-4AAA-8AAA-AAAAAAAAAAAA/" + testAttachmentID,
		"sends/" + testSendID + "/short",
		"sends/" + testSendID + "/" + strings.ToUpper(testSendFileID),
		"../db.sqlite3",
	} {
		if validManifestPath(path) {
			t.Fatalf("unsafe path accepted: %s", path)
		}
	}
}

func TestSpacePreflightRejectsImpossibleRequirement(t *testing.T) {
	t.Parallel()
	if err := preflightSpace(t.TempDir(), math.MaxInt64); err == nil {
		t.Fatal("impossible staging space requirement was accepted")
	}
}

func TestAtomicPublicationFailurePreservesPreviousPoint(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	point := filepath.Join(root, "point")
	build := filepath.Join(root, ".build-test")
	for _, path := range []string{point, build} {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	oldMarker := filepath.Join(point, "old")
	newMarker := filepath.Join(build, "new")
	if err := os.WriteFile(oldMarker, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(newMarker, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	want := errors.New("synthetic exchange failure")
	published, err := publishRecoveryPointUsing(
		build,
		point,
		func(string, string) error { return errors.New("unexpected no-replace call") },
		func(string, string) error { return want },
		func(string) error { return nil },
	)
	if published || !errors.Is(err, want) {
		t.Fatalf("publication result = %t/%v", published, err)
	}
	if data, readErr := os.ReadFile(oldMarker); readErr != nil || string(data) != "old" {
		t.Fatalf("previous point changed after failed publication: %q/%v", data, readErr)
	}
	if data, readErr := os.ReadFile(newMarker); readErr != nil || string(data) != "new" {
		t.Fatalf("verified build changed after failed publication: %q/%v", data, readErr)
	}
}
