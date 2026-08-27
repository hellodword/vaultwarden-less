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
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hellodword/vaultwarden-less/internal/upstream"
	_ "github.com/mattn/go-sqlite3"
)

const (
	testCipherID     = "11111111-1111-4111-8111-111111111111"
	testAttachmentID = "0123456789abcdefabcd"
	testSendID       = "22222222-2222-4222-8222-222222222222"
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
		{"INSERT INTO __diesel_schema_migrations(version, run_on) VALUES (?, '2026-05-05')", []any{upstream.VaultwardenMigration}},
		{"INSERT INTO ciphers(uuid, created_at, updated_at, atype, name, data) VALUES (?, '2026-05-05', '2026-05-05', 2, 'cipher', '{}')", []any{testCipherID}},
		{"INSERT INTO attachments(id, cipher_uuid, file_name, file_size) VALUES (?, ?, 'encrypted', ?)", []any{testAttachmentID, testCipherID, len(attachmentData)}},
		{`INSERT INTO sends(
			uuid, name, atype, data, akey, access_count, creation_date,
			revision_date, deletion_date, disabled
		) VALUES (?, 'file send', 1, ?, 'key', 0, '2026-05-05', '2026-05-05', '2026-06-05', 0)`, []any{testSendID, `{"id":"` + testSendFileID + `","size":` + strconv.Itoa(len(sendData)) + `,"fileName":"encrypted"}`}},
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

func TestBuilderCreatesAndVerifiesExactRecoverySet(t *testing.T) {
	data := syntheticData(t, true)
	stagingDir := privateDirectory(t)
	manifest, err := (Builder{DataDir: data.dataDir, StagingDir: stagingDir}).Build(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	point := filepath.Join(stagingDir, "point")
	verified, err := Verify(context.Background(), point)
	if err != nil {
		t.Fatal(err)
	}
	paths := make([]string, 0, len(verified.Files))
	for _, file := range verified.Files {
		paths = append(paths, file.Path)
	}
	want := []string{
		"data/attachments/" + testCipherID + "/" + testAttachmentID,
		"data/db.sqlite3",
		"data/sends/" + testSendID + "/" + testSendFileID,
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
	second, err := (Builder{DataDir: data.dataDir, StagingDir: stagingDir}).Build(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if second.ContentID != manifest.ContentID {
		t.Fatal("unchanged source produced a different content ID")
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
		DataDir: data.dataDir, StagingDir: stagingDir,
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

	recoveryDatabase := filepath.Join(stagingDir, "point", "data", "db.sqlite3")
	if info, err := os.Stat(recoveryDatabase); err != nil || info.Size() < 32<<20 {
		t.Fatalf("recovery database is smaller than 32 MiB: %v/%v", info, err)
	}
	validation, err := validateDatabase(context.Background(), recoveryDatabase)
	if err != nil {
		t.Fatal(err)
	}
	if validation.Migration != upstream.VaultwardenMigration ||
		validation.SchemaFingerprint != upstream.VaultwardenSchemaFingerprint {
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
			_, err := (Builder{DataDir: data.dataDir, StagingDir: stagingDir}).Build(context.Background())
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
	builder := Builder{DataDir: data.dataDir, StagingDir: stagingDir}
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
			builder := Builder{DataDir: data.dataDir, StagingDir: stagingDir}
			runBefore := true
			runAfter := true
			builder.beforeCopy = func(path string) {
				if runBefore && path == "data/attachments/"+testCipherID+"/"+testAttachmentID && test.before != nil {
					runBefore = false
					test.before(t, data)
				}
			}
			builder.afterCopy = func(path string) {
				if runAfter && path == "data/attachments/"+testCipherID+"/"+testAttachmentID && test.after != nil {
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
		if _, err := (Builder{DataDir: data.dataDir, StagingDir: privateDirectory(t)}).Build(context.Background()); err == nil {
			t.Fatal("unsupported migration was accepted")
		}
	})
	for name, value := range map[string]string{
		"send id":   `{"id":"short","size":1}`,
		"send size": `{"id":"` + testSendFileID + `","size":"18"}`,
		"send JSON": `{invalid`,
	} {
		t.Run(name, func(t *testing.T) {
			data := syntheticData(t, true)
			if _, err := data.database.Exec("UPDATE sends SET data = ? WHERE uuid = ?", value, testSendID); err != nil {
				t.Fatal(err)
			}
			if _, err := (Builder{DataDir: data.dataDir, StagingDir: privateDirectory(t)}).Build(context.Background()); err == nil {
				t.Fatal("unsafe file Send reference was accepted")
			}
		})
	}
}

func TestBuilderRejectsSchemaDriftAtTheSameMigration(t *testing.T) {
	t.Parallel()
	data := syntheticData(t, true)
	if _, err := data.database.Exec("CREATE TABLE unexpected_history_residue (id TEXT PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
	if _, err := (Builder{DataDir: data.dataDir, StagingDir: privateDirectory(t)}).Build(context.Background()); err == nil {
		t.Fatal("database schema drift was accepted")
	}
}

func TestVerifyRejectsEveryTreeMismatch(t *testing.T) {
	tests := map[string]func(t *testing.T, point string){
		"extra file": func(t *testing.T, point string) {
			writeFixture(t, filepath.Join(point, "data", "attachments", testCipherID, strings.Repeat("e", 20)), []byte("extra"))
		},
		"missing file": func(t *testing.T, point string) {
			if err := os.Remove(filepath.Join(point, "data", "sends", testSendID, testSendFileID)); err != nil {
				t.Fatal(err)
			}
		},
		"changed file": func(t *testing.T, point string) {
			path := filepath.Join(point, "data", "attachments", testCipherID, testAttachmentID)
			writeFixture(t, path, []byte("tampered-attachment"))
		},
		"extra directory": func(t *testing.T, point string) {
			if err := os.Mkdir(filepath.Join(point, "data", "unexpected"), 0o700); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			data := syntheticData(t, true)
			stagingDir := privateDirectory(t)
			_, err := (Builder{DataDir: data.dataDir, StagingDir: stagingDir}).Build(context.Background())
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

func TestManifestContentIDUsesCanonicalTupleAndFilesOnly(t *testing.T) {
	t.Parallel()
	files := []File{
		{Path: "data/sends/" + testSendID + "/" + testSendFileID, Size: 1, SHA256: strings.Repeat("a", 64)},
		{Path: "data/db.sqlite3", Size: 2, SHA256: strings.Repeat("b", 64)},
	}
	first, err := NewManifest(files)
	if err != nil {
		t.Fatal(err)
	}
	slices.Reverse(files)
	second, err := NewManifest(files)
	if err != nil {
		t.Fatal(err)
	}
	if first.ContentID != second.ContentID {
		t.Fatal("file declaration order changed canonical content ID")
	}
	second.CreatorVersion = "different-creator"
	creatorID, err := second.CalculateContentID()
	if err != nil || creatorID != first.ContentID {
		t.Fatal("creator version changed content ID")
	}
	second.Vaultwarden.DatabaseMigration = "20260505120001"
	migrationID, err := second.CalculateContentID()
	if err != nil || migrationID == first.ContentID {
		t.Fatal("migration did not change content ID")
	}
	second = first
	second.Vaultwarden.Image += "-changed"
	imageID, err := second.CalculateContentID()
	if err != nil || imageID == first.ContentID {
		t.Fatal("image digest did not change content ID")
	}
	second = first
	second.Files[0].SHA256 = strings.Repeat("c", 64)
	fileID, err := second.CalculateContentID()
	if err != nil || fileID == first.ContentID {
		t.Fatal("file content did not change content ID")
	}
}

func TestManifestStrictlyRejectsUnknownFieldsAndUnsupportedTuple(t *testing.T) {
	t.Parallel()
	manifest, err := NewManifest([]File{{Path: "data/db.sqlite3", Size: 1, SHA256: strings.Repeat("a", 64)}})
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	withUnknown := strings.Replace(string(data), `"schema_version":2`, `"schema_version":2,"unknown":true`, 1)
	if _, err := DecodeManifest([]byte(withUnknown)); err == nil {
		t.Fatal("unknown manifest field was accepted")
	}
	manifest.Vaultwarden.Version = "1.37.3"
	manifest.ContentID, err = manifest.CalculateContentID()
	if err != nil {
		t.Fatal(err)
	}
	data, err = json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeManifest(data); err == nil {
		t.Fatal("unsupported Vaultwarden tuple was accepted")
	}
}

func TestExactAttachmentAndSendPathConstraints(t *testing.T) {
	t.Parallel()
	for _, path := range []string{
		"data/db.sqlite3",
		"data/attachments/" + testCipherID + "/" + testAttachmentID,
		"data/sends/" + testSendID + "/" + testSendFileID,
	} {
		if !validManifestPath(path) {
			t.Fatalf("valid path rejected: %s", path)
		}
	}
	for _, path := range []string{
		"data/rsa_key.pem",
		"data/attachments/" + testCipherID + "/Attachment-ID-001",
		"data/attachments/AAAAAAAA-AAAA-4AAA-8AAA-AAAAAAAAAAAA/" + testAttachmentID,
		"data/sends/" + testSendID + "/short",
		"data/sends/" + testSendID + "/" + strings.ToUpper(testSendFileID),
		"../data/db.sqlite3",
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
