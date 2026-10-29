// Package capture creates immutable native SQLite and filesystem payloads.
// Digests and file metadata are memory-only comparison aids, never a restore
// format or a persisted checksum list.
package capture

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const Timeout = 30 * time.Minute

type Payload struct {
	Directory     string
	Database      Digest
	DatabaseBytes string
	Reliable      bool
	Files         map[string]Entry
	Issues        map[string]int
	Warnings      []string
}

func (p *Payload) Complete() bool { return len(p.Issues) == 0 }
func (p *Payload) Tag() string {
	if p.Complete() {
		return "complete"
	}
	return "incomplete"
}

// Fingerprint is a comparison key, not a restore manifest. JSON map keys are
// sorted by the encoder; local paths and filesystem metadata are excluded.
func (p *Payload) Fingerprint() string {
	database := p.Database.Business
	if !p.Reliable {
		database = p.DatabaseBytes
	}
	encoded, _ := json.Marshal(struct {
		Database string
		Reliable bool
		Files    map[string]Entry
		Issues   map[string]int
		Warnings []string
	}{database, p.Reliable, p.Files, p.Issues, p.Warnings})
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}
func (p *Payload) Same(other *Payload, _ bool) bool {
	return other != nil && p.Fingerprint() == other.Fingerprint()
}

type Builder struct {
	DataDir        string
	CacheDir       string
	afterRead      func(string)
	afterDatabase  func()
	referenceCheck func(context.Context, *Payload) error
}

// Build is called serially. It retains a complete SQLite copy even if the
// remaining source tree is incomplete. Any destination failure aborts the
// entire build. The caller owns the returned directory's lifetime.
func (b Builder) Build(ctx context.Context, previous *Payload, readAll bool) (payload *Payload, err error) {
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	workRoot := filepath.Join(b.CacheDir, "sqlite")
	payloadRoot := filepath.Join(b.CacheDir, "payloads")
	for _, path := range []string{workRoot, payloadRoot} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			return nil, errors.New("create capture cache")
		}
	}
	work, err := os.MkdirTemp(workRoot, "build-")
	if err != nil {
		return nil, errors.New("create database workspace")
	}
	defer os.RemoveAll(work)
	directory, err := os.MkdirTemp(payloadRoot, "payload-")
	if err != nil {
		return nil, errors.New("create payload workspace")
	}
	defer func() {
		if err != nil {
			os.RemoveAll(directory)
		}
	}()
	db := filepath.Join(work, "db.sqlite3")
	if err = OnlineBackup(ctx, filepath.Join(b.DataDir, "db.sqlite3"), db); err != nil {
		return nil, err
	}
	payload = &Payload{Directory: directory, Files: map[string]Entry{}, Issues: map[string]int{}}
	payload.DatabaseBytes, err = hashDatabase(ctx, db)
	if err != nil {
		return nil, err
	}
	if IntegrityCheck(ctx, db) != nil {
		payload.Issues["database_integrity_failed"] = 1
	}
	digest, digestErr := CompareDigest(ctx, db)
	if digestErr != nil {
		payload.Warnings = append(payload.Warnings, "database_comparison_unavailable")
	} else {
		payload.Database = digest
		payload.Reliable = true
	}
	// The standalone DB is opened immutable for comparison. Its backup journal
	// mode may remain WAL, but no live-source sidecars or work files are copied.
	if err = os.Rename(db, filepath.Join(directory, "db.sqlite3")); err != nil {
		return nil, errors.New("move database into payload")
	}
	if b.afterDatabase != nil {
		b.afterDatabase()
	}
	fd, openErr := syscall.Open(b.DataDir, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
	if openErr != nil {
		payload.Issues["data_directory_unreadable"]++
		return payload, nil
	}
	source := os.NewFile(uintptr(fd), b.DataDir)
	defer source.Close()
	files := fileCapture{ctx: ctx, destination: directory, previous: previous, readAll: readAll, entries: payload.Files, issues: payload.Issues, afterRead: b.afterRead}
	if err = files.walk(source, ""); err != nil {
		return nil, err
	}
	if ctx.Err() == nil {
		check := b.referenceCheck
		if check == nil {
			check = func(ctx context.Context, p *Payload) error { return p.checkReferences(ctx) }
		}
		if diagnosticErr := check(ctx, payload); diagnosticErr != nil {
			payload.Issues["file_reference_diagnostics_unavailable"] = 1
		}
	}
	// Reference diagnostics share the capture deadline and may consume its
	// remaining time. Keep the native payload, but never label it complete.
	if ctx.Err() != nil {
		payload.Issues["capture_timeout"] = 1
	}
	return payload, nil
}

func (p *Payload) checkReferences(ctx context.Context) error {
	db := filepath.Join(p.Directory, "db.sqlite3")
	columns, err := databaseColumns(ctx, db)
	if err != nil {
		return err
	}
	tables := map[string]map[string]bool{}
	for _, col := range columns {
		if tables[col.Table] == nil {
			tables[col.Table] = map[string]bool{}
		}
		tables[col.Table][col.Name] = true
	}
	// Read each class independently: malformed Send JSON must not hide a
	// missing attachment (or vice versa).
	var failed bool
	for _, kind := range []string{"attachments", "sends"} {
		cols := tables[kind]
		if cols == nil {
			continue
		}
		query := ""
		if kind == "attachments" && cols["cipher_uuid"] && cols["id"] && cols["file_size"] {
			query = `SELECT 'attachments/'||cipher_uuid||'/'||id AS path,file_size AS size FROM attachments`
		}
		if kind == "sends" && cols["atype"] && cols["uuid"] && cols["data"] {
			query = `SELECT 'sends/'||uuid||'/'||json_extract(data,'$.id') AS path,json_extract(data,'$.size') AS size FROM sends WHERE atype=1`
		}
		if query == "" || p.readReferences(ctx, db, query) != nil {
			p.Issues[kind+"_references_unavailable"] = 1
			failed = true
		}
	}
	if failed {
		return errors.New("file reference check failed")
	}
	return nil
}

func (p *Payload) readReferences(ctx context.Context, db, query string) error {
	data, err := runSQLite(ctx, db, ".mode json\n"+query+";\n", true)
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(data)) == "" {
		return nil
	}
	var refs []struct {
		Path string          `json:"path"`
		Size json.RawMessage `json:"size"`
	}
	if json.Unmarshal(data, &refs) != nil {
		return errors.New("invalid references")
	}
	for _, ref := range refs {
		// Vaultwarden Send JSON stores sizes as either strings or numbers.
		var size int64
		sizeText := strings.Trim(string(ref.Size), `"`)
		_, sizeErr := fmt.Sscan(sizeText, &size)
		if !filepath.IsLocal(ref.Path) || filepath.Clean(ref.Path) != ref.Path || len(strings.Split(ref.Path, "/")) != 3 || sizeErr != nil || size < 0 || strconv.FormatInt(size, 10) != sizeText {
			p.Issues["invalid_file_reference"]++
			continue
		}
		entry, ok := p.Files[ref.Path]
		if !ok || entry.Kind != "file" {
			p.Issues["referenced_file_missing"]++
		} else if entry.Size != size {
			p.Issues["referenced_file_size_mismatch"]++
		}
	}
	return nil
}
