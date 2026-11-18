// Package capture creates standalone copies with SQLite's online backup API.
package capture

import (
	"context"
	"errors"
	"os"
	"path/filepath"
)

type Builder struct {
	DataDir, CacheDir string
	Rule              Rule
}

// Build returns the database path. The caller owns its temporary directory.
func (b Builder) Build(ctx context.Context) (string, error) {
	dir, err := os.MkdirTemp(b.CacheDir, "capture-")
	if err != nil {
		return "", errors.New("capture directory unavailable")
	}
	path := filepath.Join(dir, "db.sqlite3")
	if err = OnlineBackup(ctx, filepath.Join(b.DataDir, "db.sqlite3"), path); err == nil {
		err = IntegrityCheck(ctx, path)
	}
	if err == nil {
		// Check the rule against the actual schema even for a first backup.
		var changed bool
		changed, err = b.Rule.Changed(ctx, path, path)
		if changed && err == nil {
			err = errors.New("comparison rule rejected its own input")
		}
	}
	if err != nil {
		os.RemoveAll(dir)
		return "", err
	}
	return path, nil
}
