package capture

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const RulePath = "/rules/personal-vault.sql"

func readOnlyURI(path string) string {
	u := url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro&immutable=1"}
	return u.String()
}

func sqlite(ctx context.Context, database, query string) (string, error) {
	cmd := exec.CommandContext(ctx, "sqlite3", "-batch", "-bail", "-init", "/dev/null", database, query)
	// Output should only be the comparison bit or "ok". Never log vault rows.
	var result limitedBuffer
	cmd.Stdout = &result
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		return "", errors.New("SQLite query failed")
	}
	return result.data.String(), nil
}

type limitedBuffer struct{ data bytes.Buffer }

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.data.Len()+len(p) > 1024 {
		return 0, errors.New("unexpected SQLite output")
	}
	return b.data.Write(p)
}

type Rule string

func LoadRule(path string) (Rule, error) {
	data, err := os.ReadFile(path)
	if err != nil || len(bytes.TrimSpace(data)) == 0 {
		return "", errors.New("personal vault SQL rule unavailable")
	}
	return Rule(data), nil
}

func (rule Rule) Changed(ctx context.Context, previous, current string) (bool, error) {
	attach := func(path, alias string) string {
		return "ATTACH '" + strings.ReplaceAll(readOnlyURI(path), "'", "''") + "' AS " + alias + ";\n"
	}
	query := attach(previous, "previous") + attach(current, "current") + "PRAGMA query_only=ON;\n" + string(rule)
	result, err := sqlite(ctx, ":memory:", query)
	if err == nil {
		switch result {
		case "0\n":
			return false, nil
		case "1\n":
			return true, nil
		}
	}
	return false, errors.New("personal vault comparison failed")
}

func IntegrityCheck(ctx context.Context, path string) error {
	// Opening an absent or empty file normally would create a valid empty database.
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
		return errors.New("SQLite database file missing or empty")
	}
	// Open this private copy normally so SQLite also verifies CHECK constraints.
	result, err := sqlite(ctx, path, "PRAGMA integrity_check;")
	if err != nil || result != "ok\n" {
		return errors.New("SQLite integrity check failed")
	}
	return nil
}

// OnlineBackup only opens the live database read-only. The CLI destination is
// relative to its private working directory, so dot-command quoting never
// interprets a source-controlled or operator-controlled path.
func OnlineBackup(ctx context.Context, source, destination string) error {
	info, err := os.Lstat(source)
	if err != nil || !info.Mode().IsRegular() {
		return errors.New("source database unavailable")
	}
	if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
		return errors.New("database destination exists or cannot be inspected")
	}
	work, err := os.MkdirTemp(filepath.Dir(destination), ".sqlite-")
	if err != nil {
		return errors.New("create SQLite work directory")
	}
	defer os.RemoveAll(work)
	u := url.URL{Scheme: "file", Path: source}
	cmd := exec.CommandContext(ctx, "sqlite3", "-batch", "-bail", "-readonly", "-init", "/dev/null", u.String())
	cmd.Dir = work
	cmd.Stdin = strings.NewReader(".timeout 5000\n.backup copy.sqlite3\n")
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		return errors.New("read-only SQLite backup failed")
	}
	if err := os.Chmod(filepath.Join(work, "copy.sqlite3"), 0o600); err != nil {
		return errors.New("secure SQLite copy")
	}
	if err := os.Rename(filepath.Join(work, "copy.sqlite3"), destination); err != nil {
		return errors.New("publish SQLite copy")
	}
	return nil
}
