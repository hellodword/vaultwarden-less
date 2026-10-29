package capture

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

type column struct {
	Table string `json:"tbl"`
	Name  string `json:"name"`
	PK    int    `json:"pk"`
}

type Digest struct {
	Business string
	Full     string
}

func literal(value string) string    { return "'" + strings.ReplaceAll(value, "'", "''") + "'" }
func identifier(value string) string { return `"` + strings.ReplaceAll(value, `"`, `""`) + `"` }

// runSQLite never forwards tool diagnostics: SQL errors can contain database
// values. No row data crosses this boundary except file reference diagnostics.
func runSQLite(ctx context.Context, path, input string, immutable bool) ([]byte, error) {
	u := url.URL{Scheme: "file", Path: path}
	if immutable {
		u.RawQuery = "immutable=1"
	}
	args := []string{"-batch", "-bail", "-init", "/dev/null"}
	if immutable {
		args = append(args, "-readonly")
	}
	args = append(args, u.String())
	cmd := exec.CommandContext(ctx, "sqlite3", args...)
	cmd.Stdin = strings.NewReader(input)
	var out limitedBuffer
	cmd.Stdout = &out
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		return nil, errors.New("sqlite operation failed")
	}
	return out.Bytes(), nil
}

type limitedBuffer struct{ bytes.Buffer }

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > 16<<20 {
		return 0, errors.New("sqlite result too large")
	}
	return b.Buffer.Write(p)
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

func databaseColumns(ctx context.Context, path string) ([]column, error) {
	data, err := runSQLite(ctx, path, `.mode json
SELECT s.name AS tbl,p.name,p.pk FROM sqlite_schema s,pragma_table_xinfo(s.name) p
WHERE s.type='table' AND p.hidden!=1 ORDER BY s.name,p.cid;
`, true)
	if err != nil {
		return nil, err
	}
	var columns []column
	if len(bytes.TrimSpace(data)) == 0 {
		return columns, nil
	}
	if json.Unmarshal(data, &columns) != nil {
		return nil, errors.New("invalid SQLite schema metadata")
	}
	return columns, nil
}

// CompareDigest hashes query results with SQLite's native typed sha3_query
// encoding. Ordering includes typeof() and BINARY collation, so counts, NULL,
// BLOB, numeric storage classes and collated-text ties are preserved.
func CompareDigest(ctx context.Context, path string) (Digest, error) {
	cols, err := databaseColumns(ctx, path)
	if err != nil {
		return Digest{}, err
	}
	full := databaseQuery(cols, false)
	business := databaseQuery(cols, true)
	query := "SELECT hex(sha3_query(" + literal(business) + ",256)),hex(sha3_query(" + literal(full) + ",256));\n"
	data, err := runSQLite(ctx, path, query, true)
	if err != nil {
		return Digest{}, err
	}
	parts := strings.Split(strings.TrimSpace(string(data)), "|")
	if len(parts) != 2 {
		return Digest{}, errors.New("invalid SQLite digest")
	}
	for _, part := range parts {
		if len(part) != 64 {
			return Digest{}, errors.New("invalid SQLite digest")
		}
		if _, err := hex.DecodeString(part); err != nil {
			return Digest{}, errors.New("invalid SQLite digest")
		}
	}
	return Digest{Business: parts[0], Full: parts[1]}, nil
}

func databaseQuery(cols []column, business bool) string {
	query := `SELECT type,name,tbl_name,sql FROM sqlite_schema ORDER BY type COLLATE BINARY,name COLLATE BINARY;
SELECT user_version FROM pragma_user_version; SELECT application_id FROM pragma_application_id;`
	for start := 0; start < len(cols); {
		end := start + 1
		for end < len(cols) && cols[end].Table == cols[start].Table {
			end++
		}
		tableCols := cols[start:end]
		if business && cols[start].Table == "twofactor" && hasColumn(tableCols, "atype") {
			for _, kind := range []int{0, 1, 7} {
				query += tableQuery(tableCols, true, &kind)
			}
			unknown := -1
			query += tableQuery(tableCols, false, &unknown)
		} else {
			query += tableQuery(tableCols, business, nil)
		}
		start = end
	}
	return query
}

func hasColumn(cols []column, name string) bool {
	return slices.ContainsFunc(cols, func(c column) bool { return c.Name == name })
}

func tableQuery(cols []column, business bool, factor *int) string {
	table := cols[0].Table
	excluded := []string{}
	temporary := false
	if business {
		excluded = strings.Fields(excludedColumns[table])
		temporary = transientTables[table]
		if factor != nil {
			switch *factor {
			case 0:
				excluded = []string{"last_used"}
			case 7:
				excluded = []string{"uuid"}
			case 1000, 1001, 1002, 1003, 1004, 2000:
				temporary = true
			}
		}
	}
	if temporary {
		return ""
	}
	var expressions, aliases []string
	for _, col := range cols {
		if slices.Contains(excluded, col.Name) {
			continue
		}
		name := identifier(col.Name)
		expr := name
		if business {
			if table == "sends" && col.Name == "access_count" && hasColumn(cols, "max_access_count") {
				expr = "CASE WHEN max_access_count IS NULL THEN NULL ELSE access_count END"
			}
			if factor != nil && *factor == 1 && col.Name == "data" {
				expr = emailFactorData(name)
			}
		}
		alias := fmt.Sprintf("c%d", len(aliases))
		expressions = append(expressions, expr+" AS "+alias)
		aliases = append(aliases, alias)
	}
	if len(expressions) == 0 {
		return ""
	}
	predicate := ""
	if factor != nil {
		if *factor < 0 {
			predicate = " WHERE typeof(atype)!='integer' OR atype NOT IN (0,1,7,1000,1001,1002,1003,1004,2000)"
		} else {
			predicate = fmt.Sprintf(" WHERE typeof(atype)='integer' AND atype=%d", *factor)
		}
	}
	var ordering []string
	for _, alias := range aliases {
		ordering = append(ordering, "typeof("+alias+")", alias+" COLLATE BINARY")
	}
	result := "SELECT " + strings.Join(aliases, ",") + " FROM (SELECT " + strings.Join(expressions, ",") + " FROM " + identifier(table) + predicate + ")"
	return result + " ORDER BY " + strings.Join(ordering, ",") + ";"
}

// IntegrityCheck inspects the standalone copy, never the live source. The
// pinned SQLite omits CHECK constraint validation on read-only connections;
// open this disposable copy normally to get the complete integrity result.
func IntegrityCheck(ctx context.Context, path string) error {
	data, err := runSQLite(ctx, path, "PRAGMA integrity_check;\n", false)
	if err != nil || strings.TrimSpace(string(data)) != "ok" {
		return errors.New("database integrity check failed")
	}
	return nil
}
