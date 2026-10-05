// Package capture creates immutable native SQLite and filesystem payloads.
// Digests and file metadata are memory-only comparison aids, never a restore
// format or a persisted checksum list.
package capture

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
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

func (p *Payload) Same(other *Payload, full bool) bool {
	if other == nil || p.Complete() != other.Complete() || len(p.Files) != len(other.Files) {
		return false
	}
	if !p.Reliable || !other.Reliable {
		if p.DatabaseBytes == "" || p.DatabaseBytes != other.DatabaseBytes {
			return false
		}
	} else if full {
		if p.Database.Full != other.Database.Full {
			return false
		}
	} else if p.Database.Business != other.Database.Business {
		return false
	}
	for path, entry := range p.Files {
		if candidate, ok := other.Files[path]; !ok || !entry.equal(candidate) {
			return false
		}
	}
	return true
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
	if err != nil && ctx.Err() == nil {
		return nil, err
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
			payload.Warnings = append(payload.Warnings, "file_reference_diagnostics_unavailable")
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
	var queries []string
	if cols := tables["attachments"]; cols != nil {
		if !cols["cipher_uuid"] || !cols["id"] || !cols["file_size"] {
			return errors.New("unknown attachment reference shape")
		}
		queries = append(queries, `SELECT 'attachments/'||cipher_uuid||'/'||id AS path,file_size AS size FROM attachments`)
	}
	if cols := tables["sends"]; cols != nil && cols["atype"] {
		if !cols["uuid"] || !cols["data"] {
			return errors.New("unknown Send reference shape")
		}
		queries = append(queries, `SELECT 'sends/'||uuid||'/'||json_extract(data,'$.id') AS path,CAST(json_extract(data,'$.size') AS INTEGER) AS size FROM sends WHERE atype=1`)
	}
	if len(queries) == 0 {
		return nil
	}
	data, err := runSQLite(ctx, db, ".mode json\n"+strings.Join(queries, " UNION ALL ")+";\n", true)
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(data)) == "" {
		return nil
	}
	var refs []struct {
		Path string `json:"path"`
		Size *int64 `json:"size"`
	}
	if err := json.Unmarshal(data, &refs); err != nil {
		return errors.New("invalid file reference result")
	}
	for _, ref := range refs {
		if !filepath.IsLocal(ref.Path) || filepath.Clean(ref.Path) != ref.Path || len(strings.Split(ref.Path, "/")) != 3 || ref.Size == nil || *ref.Size < 0 {
			p.Issues["invalid_file_reference"]++
			continue
		}
		entry, ok := p.Files[ref.Path]
		if !ok || entry.Kind != "file" {
			p.Issues["referenced_file_missing"]++
		} else if entry.Size != *ref.Size {
			p.Issues["referenced_file_size_mismatch"]++
		}
	}
	return nil
}
