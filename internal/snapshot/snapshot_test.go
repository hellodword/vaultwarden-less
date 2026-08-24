package snapshot

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/hellodword/vaultwarden-less/internal/version"
	_ "github.com/mattn/go-sqlite3"
)

const (
	testCipherID     = "11111111-1111-4111-8111-111111111111"
	testSendID       = "22222222-2222-4222-8222-222222222222"
	testAttachmentID = "0123456789abcdefabcd"
	testSendFileID   = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)

func syntheticData(t *testing.T, includeFiles bool) (string, *sql.DB) {
	t.Helper()
	dataDir := filepath.Join(t.TempDir(), "data")
	if err := os.Mkdir(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	databasePath := filepath.Join(dataDir, "db.sqlite3")
	db, err := sql.Open("sqlite3", "file:"+databasePath+"?_busy_timeout=5000&_foreign_keys=on")
	if err != nil {
		t.Fatal(err)
	}
	schema := []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA foreign_keys=ON",
		"CREATE TABLE __diesel_schema_migrations (version TEXT PRIMARY KEY NOT NULL, run_on TEXT NOT NULL)",
		"CREATE TABLE users (uuid TEXT PRIMARY KEY)",
		"CREATE TABLE ciphers (uuid TEXT PRIMARY KEY)",
		"CREATE TABLE attachments (id TEXT PRIMARY KEY, cipher_uuid TEXT NOT NULL REFERENCES ciphers(uuid), file_size INTEGER NOT NULL)",
		"CREATE TABLE sends (uuid TEXT PRIMARY KEY, atype INTEGER NOT NULL, data TEXT NOT NULL)",
		"CREATE TABLE activity (id INTEGER PRIMARY KEY, value TEXT NOT NULL)",
	}
	for _, statement := range schema {
		if _, err := db.Exec(statement); err != nil {
			db.Close()
			t.Fatal(err)
		}
	}
	attachment := []byte("encrypted-attachment")
	send := []byte("encrypted-send")
	statements := []struct {
		query string
		args  []any
	}{
		{"INSERT INTO __diesel_schema_migrations(version, run_on) VALUES (?, '2026-05-05')", []any{version.VaultwardenSchema}},
		{"INSERT INTO users(uuid) VALUES ('33333333-3333-4333-8333-333333333333')", nil},
		{"INSERT INTO ciphers(uuid) VALUES (?)", []any{testCipherID}},
		{"INSERT INTO attachments(id, cipher_uuid, file_size) VALUES (?, ?, ?)", []any{testAttachmentID, testCipherID, len(attachment)}},
		{"INSERT INTO sends(uuid, atype, data) VALUES (?, 1, ?)", []any{testSendID, fmt.Sprintf(`{"id":%q,"size":%d,"fileName":"encrypted"}`, testSendFileID, len(send))}},
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement.query, statement.args...); err != nil {
			db.Close()
			t.Fatal(err)
		}
	}
	if includeFiles {
		writeFixture(t, filepath.Join(dataDir, "attachments", testCipherID, testAttachmentID), attachment)
		writeFixture(t, filepath.Join(dataDir, "sends", testSendID, testSendFileID), send)
	}
	writeFixture(t, filepath.Join(dataDir, "rsa_key.pem"), []byte("synthetic-rsa"))
	writeFixture(t, filepath.Join(dataDir, "config.json"), []byte(`{"synthetic":true}`))
	writeFixture(t, filepath.Join(dataDir, "templates", "mail", "test.hbs"), []byte("template"))
	writeFixture(t, filepath.Join(dataDir, "icon_cache", "ignored"), []byte("ignore"))
	writeFixture(t, filepath.Join(dataDir, "tmp", "ignored"), []byte("ignore"))
	writeFixture(t, filepath.Join(dataDir, "backups", "ignored"), []byte("ignore"))
	writeFixture(t, filepath.Join(dataDir, "unknown", "ignored"), []byte("ignore"))
	writeFixture(t, filepath.Join(dataDir, ".env"), []byte("SECRET=not-a-real-secret"))
	return dataDir, db
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

func TestBuilderCreatesCompleteWhitelistedRecoverySetDuringWALWrites(t *testing.T) {
	dataDir, db := syntheticData(t, true)
	defer db.Close()
	stagingDir := filepath.Join(t.TempDir(), "staging")
	mustSnapshotRoot(t, stagingDir)
	builder := Builder{DataDir: dataDir, StagingDir: stagingDir}

	ctx, cancel := context.WithCancel(context.Background())
	var writer sync.WaitGroup
	writer.Add(1)
	go func() {
		defer writer.Done()
		for i := 0; i < 200; i++ {
			select {
			case <-ctx.Done():
				return
			default:
				_, _ = db.Exec("INSERT INTO activity(value) VALUES (?)", fmt.Sprintf("write-%d", i))
			}
		}
	}()
	result, err := builder.Build(context.Background(), false)
	cancel()
	writer.Wait()
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(result.Path)
	if result.Manifest.Status != Complete {
		t.Fatalf("status = %s", result.Manifest.Status)
	}
	verified, err := Verify(context.Background(), result.Path, false)
	if err != nil {
		t.Fatal(err)
	}
	paths := make([]string, 0, len(verified.Files))
	for _, file := range verified.Files {
		paths = append(paths, file.Path)
	}
	for _, required := range []string{
		"data/db.sqlite3",
		"data/attachments/" + testCipherID + "/" + testAttachmentID,
		"data/sends/" + testSendID + "/" + testSendFileID,
		"data/rsa_key.pem", "data/config.json", "data/templates/mail/test.hbs",
	} {
		if !slices.Contains(paths, required) {
			t.Errorf("missing expected path %s", required)
		}
	}
	for _, forbidden := range []string{"wal", "shm", "icon_cache", "tmp/", "backups/", "unknown/", ".env"} {
		for _, path := range paths {
			if filepath.Base(path) != "db.sqlite3" && contains(path, forbidden) {
				t.Errorf("forbidden path %s matched %s", forbidden, path)
			}
		}
	}
}

func TestBuilderRetriesMissingReferencesThenAllowsIncomplete(t *testing.T) {
	dataDir, db := syntheticData(t, false)
	defer db.Close()
	builder := Builder{DataDir: dataDir, StagingDir: mustSnapshotRoot(t, filepath.Join(t.TempDir(), "staging"))}
	if _, err := builder.Build(context.Background(), false); err == nil {
		t.Fatal("missing references produced a complete result")
	} else {
		var missing *MissingFilesError
		if !errors.As(err, &missing) || missing.Count != 2 {
			t.Fatalf("error = %v", err)
		}
	}
	result, err := builder.Build(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(result.Path)
	if result.Manifest.Status != Incomplete || len(result.Manifest.Missing) != 2 {
		t.Fatalf("manifest = %#v", result.Manifest)
	}
	if _, err := Verify(context.Background(), result.Path, false); err == nil {
		t.Fatal("incomplete set verified without authorization")
	}
	if _, err := Verify(context.Background(), result.Path, true); err != nil {
		t.Fatal(err)
	}
}

func TestBuilderRejectsTemplateSymlink(t *testing.T) {
	dataDir, db := syntheticData(t, true)
	defer db.Close()
	link := filepath.Join(dataDir, "templates", "link")
	if err := os.Symlink(filepath.Join(dataDir, "config.json"), link); err != nil {
		t.Fatal(err)
	}
	builder := Builder{DataDir: dataDir, StagingDir: mustSnapshotRoot(t, filepath.Join(t.TempDir(), "staging"))}
	if _, err := builder.Build(context.Background(), false); err == nil {
		t.Fatal("template symlink was accepted")
	}
}

func TestVerifyRejectsUnreferencedDatabaseManagedFile(t *testing.T) {
	dataDir, db := syntheticData(t, true)
	defer db.Close()
	builder := Builder{DataDir: dataDir, StagingDir: mustSnapshotRoot(t, filepath.Join(t.TempDir(), "staging"))}
	result, err := builder.Build(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(result.Path)

	extraPath := "data/attachments/" + testCipherID + "/ffffffffffffffffffff"
	writeFixture(t, filepath.Join(result.Path, filepath.FromSlash(extraPath)), []byte("unreferenced"))
	extra, err := hashRegularFile(filepath.Join(result.Path, filepath.FromSlash(extraPath)))
	if err != nil {
		t.Fatal(err)
	}
	extra.Path = extraPath
	manifest, err := NewManifest(Complete, result.Manifest.DatabaseMigration, append(result.Manifest.Files, extra), nil)
	if err != nil {
		t.Fatal(err)
	}
	writeTestManifest(t, result.Path, manifest)

	if _, err := Verify(context.Background(), result.Path, false); err == nil {
		t.Fatal("unreferenced attachment was accepted")
	}
}

func TestVerifyRejectsUnreferencedMissingDatabaseManagedFile(t *testing.T) {
	dataDir, db := syntheticData(t, true)
	defer db.Close()
	builder := Builder{DataDir: dataDir, StagingDir: mustSnapshotRoot(t, filepath.Join(t.TempDir(), "staging"))}
	result, err := builder.Build(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(result.Path)

	missing := []Missing{{
		Path:   "data/sends/" + testSendID + "/ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff",
		Reason: "not_found",
	}}
	manifest, err := NewManifest(Incomplete, result.Manifest.DatabaseMigration, result.Manifest.Files, missing)
	if err != nil {
		t.Fatal(err)
	}
	writeTestManifest(t, result.Path, manifest)

	if _, err := Verify(context.Background(), result.Path, true); err == nil {
		t.Fatal("unreferenced missing send was accepted")
	}
}

func writeTestManifest(t *testing.T, root string, manifest Manifest) {
	t.Helper()
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(filepath.Join(root, "manifest.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func mustSnapshotRoot(t *testing.T, path string) string {
	t.Helper()
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestManifestContentIDIsDeterministic(t *testing.T) {
	t.Parallel()
	files := []File{
		{Path: "data/rsa_key.pem", Size: 1, SHA256: stringsOf("a", 64)},
		{Path: "data/db.sqlite3", Size: 2, SHA256: stringsOf("b", 64)},
	}
	first, err := NewManifest(Complete, version.VaultwardenSchema, files, nil)
	if err != nil {
		t.Fatal(err)
	}
	slices.Reverse(files)
	second, err := NewManifest(Complete, version.VaultwardenSchema, files, nil)
	if err != nil {
		t.Fatal(err)
	}
	if first.ContentID != second.ContentID {
		t.Fatalf("content IDs differ: %s != %s", first.ContentID, second.ContentID)
	}
	second.Files[0].Path = "../escape"
	if err := second.Validate(); err == nil {
		t.Fatal("path traversal was accepted")
	}
}

func TestManifestRejectsContradictoryEntries(t *testing.T) {
	t.Parallel()
	database := File{Path: "data/db.sqlite3", Size: 1, SHA256: stringsOf("a", 64)}
	if _, err := NewManifest(Incomplete, version.VaultwardenSchema, []File{database}, nil); err == nil {
		t.Fatal("incomplete manifest without missing files was accepted")
	}
	missing := []Missing{{Path: "data/attachments/aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa/aaaaaaaaaaaaaaaaaaaa", Reason: "not_found"}}
	manifest, err := NewManifest(Incomplete, version.VaultwardenSchema, []File{database}, missing)
	if err != nil {
		t.Fatal(err)
	}
	manifest.Files = append(manifest.Files, File{Path: missing[0].Path, Size: 1, SHA256: stringsOf("b", 64)})
	slices.SortFunc(manifest.Files, func(a, b File) int { return strings.Compare(a.Path, b.Path) })
	manifest.ContentID, err = manifest.CalculateContentID()
	if err != nil {
		t.Fatal(err)
	}
	if err := manifest.Validate(); err == nil {
		t.Fatal("path listed as both present and missing was accepted")
	}
}

func TestSpacePreflightRejectsImpossibleRequirement(t *testing.T) {
	t.Parallel()
	if err := preflightSpace(t.TempDir(), math.MaxInt64); err == nil {
		t.Fatal("impossible staging space requirement was accepted")
	}
}

func TestOpaqueFileIDValidationMatchesVaultwardenPathContract(t *testing.T) {
	t.Parallel()
	for _, value := range []string{
		testAttachmentID,
		testSendFileID,
		"legacy-Attachment-ID-001",
	} {
		if !validOpaqueID(value) {
			t.Errorf("valid opaque file ID %q was rejected", value)
		}
	}
	for _, value := range []string{
		"",
		"../escape",
		"nested/file",
		`nested\file`,
		"non-ASCII-附件",
		strings.Repeat("a", 256),
	} {
		if validOpaqueID(value) {
			t.Errorf("unsafe opaque file ID %q was accepted", value)
		}
	}
}

func contains(value, fragment string) bool {
	return strings.Contains(value, fragment)
}

func stringsOf(value string, count int) string {
	return strings.Repeat(value, count)
}
