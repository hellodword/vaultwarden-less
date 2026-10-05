package capture

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// Load recomputes comparison state from a locally retained native payload.
// No checksums, file inventories, or database projections are read from disk.
// The caller must first match its native restic ID with the remote latest ID.
func Load(ctx context.Context, directory, tag string) (*Payload, error) {
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() {
		return nil, errors.New("baseline directory unavailable")
	}
	if tag != "complete" && tag != "incomplete" {
		return nil, errors.New("baseline completeness unavailable")
	}
	p := &Payload{Directory: directory, Files: map[string]Entry{}, Issues: map[string]int{}}
	if tag == "incomplete" {
		p.Issues["previous_incomplete_capture"] = 1
	}
	db := filepath.Join(directory, "db.sqlite3")
	info, err = os.Lstat(db)
	if err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("baseline database unavailable")
	}
	p.Database, err = CompareDigest(ctx, db)
	p.Reliable = err == nil
	p.DatabaseBytes, err = hashDatabase(ctx, db)
	if err != nil {
		return nil, err
	}
	err = filepath.WalkDir(directory, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return errors.New("baseline unreadable")
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if path == directory {
			return nil
		}
		relative, err := filepath.Rel(directory, path)
		if err != nil {
			return err
		}
		if relative == "db.sqlite3" {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return errors.New("baseline metadata unavailable")
		}
		value := Entry{}
		switch {
		case info.IsDir():
			value.Kind = "directory"
		case info.Mode()&os.ModeSymlink != 0:
			value.Kind = "symlink"
			value.Link, err = os.Readlink(path)
		case info.Mode().IsRegular():
			value.Kind = "file"
			value.Size = info.Size()
			value.Hash, err = hashDatabase(ctx, path)
		default:
			return errors.New("baseline has unsupported file")
		}
		if err != nil {
			return errors.New("baseline file unavailable")
		}
		p.Files[relative] = value
		return nil
	})
	if err != nil {
		return nil, err
	}
	return p, nil
}
