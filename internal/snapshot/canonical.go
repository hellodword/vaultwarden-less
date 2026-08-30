package snapshot

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"math"
	"os"
	"slices"
	"strings"

	"github.com/hellodword/vaultwarden-less/internal/privatefs"
	"github.com/hellodword/vaultwarden-less/internal/upstream"
)

const (
	logicalMagic = "vaultwarden-less-logical\x00"
	archiveMagic = "vaultwarden-less-archive\x00"
)

func calculateContentID(ctx context.Context, databasePath, projectCommit string, files []File) (string, error) {
	if !validCommit(projectCommit) {
		return "", fmt.Errorf("invalid project commit")
	}
	database, err := sql.Open("sqlite3", sqliteURI(databasePath, "ro"))
	if err != nil {
		return "", fmt.Errorf("open canonical database: %w", err)
	}
	defer func() { _ = database.Close() }()
	database.SetMaxOpenConns(1)
	database.SetMaxIdleConns(1)

	projection, err := upstream.VaultwardenRecoveryProjection()
	if err != nil {
		return "", err
	}
	included := make([]upstream.ProjectionTable, 0, len(projection.Tables))
	for _, table := range projection.Tables {
		if table.Disposition == "included" {
			included = append(included, table)
		}
	}
	slices.SortFunc(included, func(left, right upstream.ProjectionTable) int {
		return strings.Compare(left.Name, right.Name)
	})

	digest := sha256.New()
	if _, err := io.WriteString(digest, logicalMagic); err != nil {
		return "", err
	}
	if err := writeFrame(digest, []byte(projectCommit)); err != nil {
		return "", err
	}
	if err := writeUint64(digest, uint64(len(included))); err != nil {
		return "", err
	}
	for _, table := range included {
		if err := writeCanonicalTable(ctx, digest, database, table); err != nil {
			return "", err
		}
	}

	references := make([]File, 0, len(files))
	for _, file := range files {
		if file.Path != "db.sqlite3" {
			references = append(references, file)
		}
	}
	slices.SortFunc(references, func(left, right File) int {
		return strings.Compare(left.Path, right.Path)
	})
	if err := writeUint64(digest, uint64(len(references))); err != nil {
		return "", err
	}
	for _, file := range references {
		if !validReferencedPath(file.Path) || file.Size < 0 || !IsContentID(file.SHA256) {
			return "", fmt.Errorf("invalid canonical file entry")
		}
		rawDigest, err := hex.DecodeString(strings.TrimPrefix(file.SHA256, "sha256:"))
		if err != nil || len(rawDigest) != sha256.Size {
			return "", fmt.Errorf("invalid canonical file digest")
		}
		if err := writeFrame(digest, []byte(file.Path)); err != nil {
			return "", err
		}
		if err := writeUint64(digest, uint64(file.Size)); err != nil {
			return "", err
		}
		if _, err := digest.Write(rawDigest); err != nil {
			return "", err
		}
	}
	return digestID(digest), nil
}

func writeCanonicalTable(ctx context.Context, destination io.Writer, database *sql.DB, table upstream.ProjectionTable) error {
	columns := includedColumns(table)
	if len(columns) == 0 {
		return fmt.Errorf("included table %s has no included columns", table.Name)
	}
	if err := writeFrame(destination, []byte(table.Name)); err != nil {
		return err
	}
	if err := writeUint64(destination, uint64(len(columns))); err != nil {
		return err
	}
	for _, column := range columns {
		if err := writeFrame(destination, []byte(column)); err != nil {
			return err
		}
	}

	quotedTable := quoteIdentifier(table.Name)
	var rowCount int64
	if err := database.QueryRowContext(ctx, "SELECT count(*) FROM "+quotedTable).Scan(&rowCount); err != nil || rowCount < 0 {
		return fmt.Errorf("count canonical rows for %s: %w", table.Name, err)
	}
	if err := writeUint64(destination, uint64(rowCount)); err != nil {
		return err
	}
	if err := ensureUniqueRowKey(ctx, database, table); err != nil {
		return err
	}

	selectParts := make([]string, 0, len(columns)*2)
	for _, column := range columns {
		quoted := quoteIdentifier(column)
		selectParts = append(selectParts,
			"typeof("+quoted+")",
			"CASE typeof("+quoted+") WHEN 'text' THEN CAST("+quoted+" AS BLOB) WHEN 'blob' THEN CAST("+quoted+" AS BLOB) ELSE "+quoted+" END",
		)
	}
	orderParts := make([]string, 0, len(table.RowKey))
	for _, column := range table.RowKey {
		orderParts = append(orderParts, quoteIdentifier(column)+" COLLATE BINARY ASC")
	}
	query := "SELECT " + strings.Join(selectParts, ",") + " FROM " + quotedTable + " ORDER BY " + strings.Join(orderParts, ",")
	rows, err := database.QueryContext(ctx, query)
	if err != nil {
		return fmt.Errorf("query canonical rows for %s: %w", table.Name, err)
	}
	defer func() { _ = rows.Close() }()
	seen := int64(0)
	for rows.Next() {
		values := make([]any, len(columns)*2)
		destinations := make([]any, len(values))
		for index := range values {
			destinations[index] = &values[index]
		}
		if err := rows.Scan(destinations...); err != nil {
			return fmt.Errorf("scan canonical row for %s: %w", table.Name, err)
		}
		for index := range columns {
			storageClass, ok := values[index*2].(string)
			if !ok {
				return fmt.Errorf("read SQLite storage class for %s.%s", table.Name, columns[index])
			}
			if err := writeSQLiteValue(destination, storageClass, values[index*2+1]); err != nil {
				return fmt.Errorf("encode %s.%s: %w", table.Name, columns[index], err)
			}
		}
		seen++
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate canonical rows for %s: %w", table.Name, err)
	}
	if seen != rowCount {
		return fmt.Errorf("canonical row count changed for %s", table.Name)
	}
	return nil
}

func ensureUniqueRowKey(ctx context.Context, database *sql.DB, table upstream.ProjectionTable) error {
	keys := make([]string, 0, len(table.RowKey))
	for _, key := range table.RowKey {
		keys = append(keys, quoteIdentifier(key))
	}
	query := "SELECT 1 FROM " + quoteIdentifier(table.Name) + " GROUP BY " + strings.Join(keys, ",") + " HAVING count(*) > 1 LIMIT 1"
	var duplicate int
	err := database.QueryRowContext(ctx, query).Scan(&duplicate)
	if err == nil {
		return fmt.Errorf("canonical row key is not unique for %s", table.Name)
	}
	if err != sql.ErrNoRows {
		return fmt.Errorf("validate canonical row key for %s: %w", table.Name, err)
	}
	return nil
}

func includedColumns(table upstream.ProjectionTable) []string {
	excluded := make(map[string]struct{}, len(table.ExcludedColumns))
	for _, column := range table.ExcludedColumns {
		excluded[column] = struct{}{}
	}
	result := make([]string, 0, len(table.Columns)-len(excluded))
	for _, column := range table.Columns {
		if _, omit := excluded[column]; !omit {
			result = append(result, column)
		}
	}
	return result
}

func writeSQLiteValue(destination io.Writer, storageClass string, value any) error {
	switch storageClass {
	case "null":
		if value != nil {
			return fmt.Errorf("NULL value has a payload")
		}
		_, err := destination.Write([]byte{0})
		return err
	case "integer":
		integer, ok := value.(int64)
		if !ok {
			return fmt.Errorf("INTEGER value is not int64")
		}
		if _, err := destination.Write([]byte{1}); err != nil {
			return err
		}
		return writeUint64(destination, uint64(integer))
	case "real":
		real, ok := value.(float64)
		if !ok {
			return fmt.Errorf("REAL value is not float64")
		}
		if _, err := destination.Write([]byte{2}); err != nil {
			return err
		}
		return writeUint64(destination, math.Float64bits(real))
	case "text":
		text, ok := value.([]byte)
		if !ok {
			return fmt.Errorf("TEXT value is not raw bytes")
		}
		if _, err := destination.Write([]byte{3}); err != nil {
			return err
		}
		return writeFrame(destination, text)
	case "blob":
		blob, ok := value.([]byte)
		if !ok {
			return fmt.Errorf("BLOB value is not raw bytes")
		}
		if _, err := destination.Write([]byte{4}); err != nil {
			return err
		}
		return writeFrame(destination, blob)
	default:
		return fmt.Errorf("unsupported SQLite storage class %q", storageClass)
	}
}

func calculateArchiveID(ctx context.Context, root string, files []File) (string, error) {
	ordered := slices.Clone(files)
	slices.SortFunc(ordered, func(left, right File) int {
		return strings.Compare(left.Path, right.Path)
	})
	digest := sha256.New()
	if _, err := io.WriteString(digest, archiveMagic); err != nil {
		return "", err
	}
	if err := writeUint64(digest, uint64(len(ordered))); err != nil {
		return "", err
	}
	for _, listed := range ordered {
		path, err := secureJoin(root, listed.Path)
		if err != nil {
			return "", err
		}
		if err := writeFrame(digest, []byte(listed.Path)); err != nil {
			return "", err
		}
		if err := writeUint64(digest, uint64(listed.Size)); err != nil {
			return "", err
		}
		if err := writeExactRegularFile(ctx, digest, path, listed.Size); err != nil {
			return "", fmt.Errorf("archive %s: %w", listed.Path, err)
		}
	}
	return digestID(digest), nil
}

func writeExactRegularFile(ctx context.Context, destination io.Writer, path string, expectedSize int64) error {
	if expectedSize < 0 {
		return fmt.Errorf("negative file size")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || info.Size() != expectedSize {
		return fmt.Errorf("file is not the listed 0600 regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return fmt.Errorf("file changed before archive read")
	}
	limited := &io.LimitedReader{R: file, N: expectedSize}
	written, err := copyWithContext(ctx, destination, limited)
	if err != nil {
		return err
	}
	var extra [1]byte
	if count, readErr := file.Read(extra[:]); count != 0 || !errors.Is(readErr, io.EOF) {
		return fmt.Errorf("file grew during archive read")
	}
	after, statErr := file.Stat()
	if statErr != nil || written != expectedSize || !privatefs.SameFileVersion(opened, after) {
		return fmt.Errorf("file changed during archive read")
	}
	return nil
}

func writeFrame(destination io.Writer, value []byte) error {
	if err := writeUint64(destination, uint64(len(value))); err != nil {
		return err
	}
	_, err := destination.Write(value)
	return err
}

func writeUint64(destination io.Writer, value uint64) error {
	var buffer [8]byte
	binary.BigEndian.PutUint64(buffer[:], value)
	_, err := destination.Write(buffer[:])
	return err
}

func quoteIdentifier(value string) string {
	return `"` + strings.ReplaceAll(value, `"`, `""`) + `"`
}

func digestID(digest hash.Hash) string {
	return "sha256:" + hex.EncodeToString(digest.Sum(nil))
}
