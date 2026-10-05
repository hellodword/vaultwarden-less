package capture

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestComparisonFailureStillCapturesNativeDatabase(t *testing.T) {
	b := fixtureBuilder(t)
	// This valid native table exceeds SQLite's ORDER BY term limit when every
	// column and storage class are compared. The database remains recoverable.
	columns := make([]string, 1100)
	for i := range columns {
		columns[i] = fmt.Sprintf("future%d", i)
	}
	sqlExec(t, filepath.Join(b.DataDir, "db.sqlite3"), "CREATE TABLE wide("+strings.Join(columns, ",")+");")
	first := build(t, b, nil, true)
	if first.Reliable || !first.Complete() || first.DatabaseBytes == "" || !slices.Contains(first.Warnings, "database_comparison_unavailable") {
		t.Fatalf("comparison fallback did not preserve the complete source: reliable=%v, issues=%v, warnings=%v", first.Reliable, first.Issues, first.Warnings)
	}
	if got := sqlExec(t, filepath.Join(first.Directory, "db.sqlite3"), "PRAGMA integrity_check;"); got != "ok\n" {
		t.Fatal("fallback database is invalid")
	}
	second := build(t, b, first, true)
	if !second.Same(first, false) {
		t.Fatal("unchanged fallback content repeats")
	}
	sqlExec(t, filepath.Join(b.DataDir, "db.sqlite3"), "INSERT INTO wide(future0) VALUES ('new data');")
	third := build(t, b, second, true)
	if third.Same(second, false) {
		t.Fatal("native database change missed during comparison failure")
	}
}

func fixtureBuilder(t *testing.T) Builder {
	t.Helper()
	data, cache := t.TempDir(), t.TempDir()
	sqlExec(t, filepath.Join(data, "db.sqlite3"), `CREATE TABLE users(uuid TEXT PRIMARY KEY,password_hash,updated_at);
INSERT INTO users VALUES('u','hash','before');
CREATE TABLE attachments(cipher_uuid,id,file_size);
CREATE TABLE sends(uuid,atype,data);`)
	return Builder{DataDir: data, CacheDir: cache}
}

func writeFile(t *testing.T, root, path, value string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filepath.Join(root, path)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, path), []byte(value), 0o600); err != nil {
		t.Fatal(err)
	}
}

func build(t *testing.T, b Builder, previous *Payload, readAll bool) *Payload {
	t.Helper()
	p, err := b.Build(context.Background(), previous, readAll)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestNativeTreeAndMetadataCaching(t *testing.T) {
	b := fixtureBuilder(t)
	for _, path := range []string{"rsa_key.pem", "rsa_key.pub.pem", "orphan/file", "templates/mail", "unknown.sqlite3-wal", "sub/db.sqlite3-wal"} {
		writeFile(t, b.DataDir, path, "content")
	}
	for _, path := range []string{"icon_cache/skip", "tmp/skip"} {
		writeFile(t, b.DataDir, path, "not persistent")
	}
	if err := os.Mkdir(filepath.Join(b.DataDir, "empty"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/outside/never-follow", filepath.Join(b.DataDir, "link")); err != nil {
		t.Fatal(err)
	}
	first := build(t, b, nil, true)
	if !first.Complete() || !first.Reliable {
		t.Fatalf("unexpected incomplete capture: %+v", first)
	}
	reloaded, err := Load(context.Background(), first.Directory, first.Tag())
	if err != nil || !first.Same(reloaded, true) || !reloaded.Same(first, true) {
		t.Fatalf("reloaded native baseline differs: %v", err)
	}
	for _, path := range []string{"rsa_key.pem", "orphan/file", "empty", "link", "unknown.sqlite3-wal", "sub/db.sqlite3-wal"} {
		if _, ok := first.Files[path]; !ok {
			t.Fatalf("missing %s", path)
		}
	}
	if _, ok := first.Files["icon_cache"]; ok {
		t.Fatal("included icon cache")
	}
	if _, ok := first.Files["tmp"]; ok {
		t.Fatal("included temporary directory")
	}
	link, err := os.Readlink(filepath.Join(first.Directory, "link"))
	if err != nil || link != "/outside/never-follow" {
		t.Fatal("link not preserved")
	}
	for _, path := range []string{"manifest.json", "db.sqlite3-wal", "db.sqlite3-shm"} {
		if _, err := os.Lstat(filepath.Join(first.Directory, path)); !os.IsNotExist(err) {
			t.Fatalf("unexpected payload file %s", path)
		}
	}
	second := build(t, b, first, false)
	if !second.Same(first, false) {
		t.Fatal("identical capture changed")
	}
	sourceInfo, _ := os.Stat(filepath.Join(b.DataDir, "rsa_key.pem"))
	firstInfo, _ := os.Stat(filepath.Join(first.Directory, "rsa_key.pem"))
	secondInfo, _ := os.Stat(filepath.Join(second.Directory, "rsa_key.pem"))
	if os.SameFile(sourceInfo, firstInfo) {
		t.Fatal("hard linked live data")
	}
	if !os.SameFile(firstInfo, secondInfo) {
		t.Fatal("did not reuse completed file")
	}
	third := build(t, b, second, true)
	thirdInfo, _ := os.Stat(filepath.Join(third.Directory, "rsa_key.pem"))
	if os.SameFile(secondInfo, thirdInfo) || !third.Same(second, true) {
		t.Fatal("full file read or logical equality failed")
	}
	if err := os.Chtimes(filepath.Join(b.DataDir, "rsa_key.pem"), time.Unix(42, 0), time.Unix(42, 0)); err != nil {
		t.Fatal(err)
	}
	fourth := build(t, b, third, false)
	if !fourth.Same(third, false) {
		t.Fatal("metadata alone triggered change")
	}
	writeFile(t, b.DataDir, "orphan/file", "changed")
	fifth := build(t, b, fourth, false)
	if fifth.Same(fourth, false) {
		t.Fatal("file-only change missed")
	}
	if contents, _ := os.ReadFile(filepath.Join(first.Directory, "orphan/file")); string(contents) != "content" {
		t.Fatal("old payload mutated")
	}
	if err := os.Remove(filepath.Join(b.DataDir, "empty")); err != nil {
		t.Fatal(err)
	}
	if next := build(t, b, fifth, false); next.Same(fifth, false) {
		t.Fatal("empty directory change missed")
	}
}

func TestIncompleteCaptureRetainsDatabaseAndAvailableFiles(t *testing.T) {
	b := fixtureBuilder(t)
	sqlExec(t, filepath.Join(b.DataDir, "db.sqlite3"), `INSERT INTO attachments VALUES('c','missing',7);
INSERT INTO sends VALUES('s',1,'{"id":"file","size":4,"future":{}}');`)
	writeFile(t, b.DataDir, "rsa_key.pem", "key")
	writeFile(t, b.DataDir, "sends/s/file", "send")
	first := build(t, b, nil, true)
	if first.Complete() || first.Issues["referenced_file_missing"] != 1 {
		t.Fatalf("missing reference not diagnosed: %v", first.Issues)
	}
	if got := sqlExec(t, filepath.Join(first.Directory, "db.sqlite3"), "SELECT id FROM attachments;"); got != "missing\n" {
		t.Fatal("database reference removed")
	}
	if first.Files["rsa_key.pem"].Kind != "file" {
		t.Fatal("available file lost")
	}
	second := build(t, b, first, false)
	if !second.Same(first, false) {
		t.Fatal("same incomplete content repeats")
	}
	writeFile(t, b.DataDir, "attachments/c/missing", "present")
	third := build(t, b, second, false)
	if !third.Complete() || third.Same(second, false) {
		t.Fatal("completion not detected")
	}
	if err := syscall.Mkfifo(filepath.Join(b.DataDir, "pipe"), 0o600); err != nil {
		t.Fatal(err)
	}
	fourth := build(t, b, third, false)
	if fourth.Complete() || fourth.Issues["unsupported_file"] != 1 {
		t.Fatal("special file not reported")
	}
}

func TestChangedDuringCopyNeverReusesOldContent(t *testing.T) {
	b := fixtureBuilder(t)
	writeFile(t, b.DataDir, "changing", "before")
	first := build(t, b, nil, true)
	b.afterRead = func(path string) {
		if path == "changing" {
			writeFile(t, b.DataDir, path, "changed")
		}
	}
	second := build(t, b, first, true)
	if second.Complete() || second.Issues["file_changed"] != 1 {
		t.Fatalf("changing file accepted: %v", second.Issues)
	}
	if _, err := os.Lstat(filepath.Join(second.Directory, "changing")); !os.IsNotExist(err) {
		t.Fatal("failed file included")
	}
	if got, _ := os.ReadFile(filepath.Join(first.Directory, "changing")); string(got) != "before" {
		t.Fatal("immutable file changed")
	}
	b.afterRead = nil
	third := build(t, b, second, false)
	if !third.Complete() || third.Same(second, false) {
		t.Fatal("later stable content not captured")
	}
}

func TestTimeoutAfterDatabaseKeepsIncompletePayload(t *testing.T) {
	b := fixtureBuilder(t)
	writeFile(t, b.DataDir, "file", "value")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b.afterDatabase = cancel
	p, err := b.Build(ctx, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if p.Complete() || p.Issues["capture_timeout"] == 0 {
		t.Fatal("timeout not reported as incomplete")
	}
	if got := sqlExec(t, filepath.Join(p.Directory, "db.sqlite3"), "SELECT password_hash FROM users;"); got != "hash\n" {
		t.Fatal("complete database lost")
	}
	ctx, cancel = context.WithCancel(context.Background())
	cancel()
	if _, err := b.Build(ctx, nil, true); err == nil {
		t.Fatal("capture without database succeeded")
	}
}

func TestDestinationFailureAbortsCapture(t *testing.T) {
	b := fixtureBuilder(t)
	writeFile(t, b.DataDir, "file", "value")
	b.afterDatabase = func() {
		matches, err := filepath.Glob(filepath.Join(b.CacheDir, "payloads", "payload-*"))
		if err != nil || len(matches) != 1 {
			t.Fatal("missing payload")
		}
		// A pre-existing output name makes exclusive creation fail, exercising
		// destination errors independently of source readability and UID.
		writeFile(t, matches[0], "file", "occupied")
	}
	if _, err := b.Build(context.Background(), nil, true); err == nil {
		t.Fatal("destination failure treated as incomplete capture")
	}
}

func TestTimeoutDuringReferencesKeepsNativePayloadAndRecovers(t *testing.T) {
	b := fixtureBuilder(t)
	writeFile(t, b.DataDir, "attachments/c/file", "content")
	sqlExec(t, filepath.Join(b.DataDir, "db.sqlite3"), `INSERT INTO attachments VALUES('c','file',7);`)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	called := false
	b.referenceCheck = func(ctx context.Context, p *Payload) error {
		called = true
		if ctx.Err() != nil || p.Files["attachments/c/file"].Kind != "file" {
			t.Fatal("diagnostics did not begin with a live context and captured files")
		}
		cancel()
		return p.checkReferences(ctx)
	}
	p, err := b.Build(ctx, nil, true)
	if err != nil || !called {
		t.Fatalf("capture failed before diagnostics: %v", err)
	}
	if p.Tag() != "incomplete" || p.Issues["capture_timeout"] != 1 || !slices.Contains(p.Warnings, "file_reference_diagnostics_unavailable") {
		t.Fatalf("diagnostic timeout mislabeled: issues=%v warnings=%v", p.Issues, p.Warnings)
	}
	if got := sqlExec(t, filepath.Join(p.Directory, "db.sqlite3"), "PRAGMA integrity_check; SELECT id FROM attachments;"); got != "ok\nfile\n" {
		t.Fatal("native database or file reference lost")
	}
	if got, err := os.ReadFile(filepath.Join(p.Directory, "attachments/c/file")); err != nil || string(got) != "content" {
		t.Fatal("available file lost")
	}
	b.referenceCheck = nil
	recovered := build(t, b, p, true)
	if recovered.Tag() != "complete" || recovered.Same(p, false) {
		t.Fatal("completed retry did not replace incomplete capture")
	}
	// An unsupported diagnostic shape is only a warning when time remains.
	sqlExec(t, filepath.Join(b.DataDir, "db.sqlite3"), "ALTER TABLE attachments RENAME COLUMN file_size TO future_size;")
	warning := build(t, b, recovered, true)
	if !warning.Complete() || !slices.Contains(warning.Warnings, "file_reference_diagnostics_unavailable") {
		t.Fatal("diagnostic failure without timeout made the native capture incomplete")
	}
}
