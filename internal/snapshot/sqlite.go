package snapshot

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/mattn/go-sqlite3"
)

type referencedFile struct {
	Path         string
	ExpectedSize int64
}

func onlineBackup(ctx context.Context, sourcePath, destinationPath string) error {
	sourceInfo, err := os.Lstat(sourcePath)
	if err != nil {
		return fmt.Errorf("inspect source database: %w", err)
	}
	if !sourceInfo.Mode().IsRegular() || sourceInfo.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("source database is not a regular non-symlink file")
	}
	if err := mkdirAllSecure(filepath.Dir(destinationPath)); err != nil {
		return fmt.Errorf("create database destination directory: %w", err)
	}
	f, err := os.OpenFile(destinationPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create destination database: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close destination database: %w", err)
	}

	source, err := sql.Open("sqlite3", sqliteURI(sourcePath, "ro"))
	if err != nil {
		return fmt.Errorf("open source database: %w", err)
	}
	defer func() { _ = source.Close() }()
	source.SetMaxOpenConns(4)
	source.SetMaxIdleConns(4)
	destination, err := sql.Open("sqlite3", sqliteURI(destinationPath, "rw"))
	if err != nil {
		return fmt.Errorf("open destination database: %w", err)
	}
	defer func() { _ = destination.Close() }()
	destination.SetMaxOpenConns(1)
	destination.SetMaxIdleConns(1)

	if _, err := source.ExecContext(ctx, "PRAGMA foreign_keys = ON"); err != nil {
		return fmt.Errorf("enable source foreign keys: %w", err)
	}
	if _, err := source.ExecContext(ctx, "PRAGMA busy_timeout = 5000"); err != nil {
		return fmt.Errorf("set source busy timeout: %w", err)
	}
	if _, err := destination.ExecContext(ctx, "PRAGMA foreign_keys = ON"); err != nil {
		return fmt.Errorf("enable destination foreign keys: %w", err)
	}

	sourceConn, err := source.Conn(ctx)
	if err != nil {
		return fmt.Errorf("reserve source connection: %w", err)
	}
	defer func() { _ = sourceConn.Close() }()
	destinationConn, err := destination.Conn(ctx)
	if err != nil {
		return fmt.Errorf("reserve destination connection: %w", err)
	}
	defer func() { _ = destinationConn.Close() }()

	err = sourceConn.Raw(func(rawSource any) error {
		src, ok := rawSource.(*sqlite3.SQLiteConn)
		if !ok {
			return fmt.Errorf("unexpected source SQLite driver")
		}
		return destinationConn.Raw(func(rawDestination any) error {
			dst, ok := rawDestination.(*sqlite3.SQLiteConn)
			if !ok {
				return fmt.Errorf("unexpected destination SQLite driver")
			}
			backup, err := dst.Backup("main", src, "main")
			if err != nil {
				return fmt.Errorf("start online backup: %w", err)
			}
			finished := false
			defer func() {
				if !finished {
					_ = backup.Close()
				}
			}()
			for {
				if err := ctx.Err(); err != nil {
					return err
				}
				complete, err := backup.Step(128)
				if err != nil {
					return fmt.Errorf("copy online backup pages: %w", err)
				}
				if complete {
					err := backup.Finish()
					finished = true
					return err
				}
				timer := time.NewTimer(2 * time.Millisecond)
				select {
				case <-ctx.Done():
					timer.Stop()
					return ctx.Err()
				case <-timer.C:
				}
			}
		})
	})
	if err != nil {
		return fmt.Errorf("online backup: %w", err)
	}
	var journalMode string
	if err := destinationConn.QueryRowContext(ctx, "PRAGMA journal_mode = DELETE").Scan(&journalMode); err != nil || journalMode != "delete" {
		return fmt.Errorf("normalize snapshot journal mode")
	}
	if err := destinationConn.Close(); err != nil {
		return fmt.Errorf("close destination connection: %w", err)
	}
	if err := sourceConn.Close(); err != nil {
		return fmt.Errorf("close source connection: %w", err)
	}
	if err := destination.Close(); err != nil {
		return fmt.Errorf("close destination database: %w", err)
	}
	if err := source.Close(); err != nil {
		return fmt.Errorf("close source database: %w", err)
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		sidecar := destinationPath + suffix
		if info, err := os.Lstat(sidecar); err == nil {
			if !info.Mode().IsRegular() {
				return fmt.Errorf("snapshot database sidecar is not regular")
			}
			if info.Size() != 0 {
				return fmt.Errorf("normalized snapshot database has a non-empty sidecar")
			}
			if err := os.Remove(sidecar); err != nil {
				return fmt.Errorf("remove normalized snapshot sidecar: %w", err)
			}
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("inspect normalized snapshot sidecar: %w", err)
		}
	}
	if err := os.Chmod(destinationPath, 0o600); err != nil {
		return fmt.Errorf("secure destination database: %w", err)
	}
	return nil
}

func sqliteURI(path, mode string) string {
	uri := url.URL{Scheme: "file", Path: path}
	query := uri.Query()
	query.Set("mode", mode)
	query.Set("_busy_timeout", "5000")
	query.Set("_foreign_keys", "on")
	uri.RawQuery = query.Encode()
	return uri.String()
}

func validateDatabase(ctx context.Context, path string) (string, error) {
	db, err := sql.Open("sqlite3", sqliteURI(path, "ro"))
	if err != nil {
		return "", fmt.Errorf("open snapshot database: %w", err)
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	rows, err := db.QueryContext(ctx, "PRAGMA integrity_check")
	if err != nil {
		return "", fmt.Errorf("run integrity_check: %w", err)
	}
	var results []string
	for rows.Next() {
		var result string
		if err := rows.Scan(&result); err != nil {
			_ = rows.Close()
			return "", fmt.Errorf("read integrity_check: %w", err)
		}
		results = append(results, result)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return "", fmt.Errorf("iterate integrity_check: %w", err)
	}
	if err := rows.Close(); err != nil {
		return "", fmt.Errorf("close integrity_check: %w", err)
	}
	if len(results) != 1 || results[0] != "ok" {
		return "", fmt.Errorf("integrity_check failed")
	}

	rows, err = db.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		return "", fmt.Errorf("run foreign_key_check: %w", err)
	}
	if rows.Next() {
		_ = rows.Close()
		return "", fmt.Errorf("foreign_key_check failed")
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return "", fmt.Errorf("iterate foreign_key_check: %w", err)
	}
	if err := rows.Close(); err != nil {
		return "", fmt.Errorf("close foreign_key_check: %w", err)
	}

	required := []struct {
		table   string
		columns []string
	}{
		{table: "attachments", columns: []string{"cipher_uuid", "id", "file_size"}},
		{table: "__diesel_schema_migrations", columns: []string{"version"}},
	}
	for _, requirement := range required {
		table := requirement.table
		var count int
		if err := db.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = ?", table).Scan(&count); err != nil || count != 1 {
			return "", fmt.Errorf("required table %s is missing", table)
		}
		for _, column := range requirement.columns {
			if err := db.QueryRowContext(ctx, "SELECT count(*) FROM pragma_table_info(?) WHERE name = ?", table, column).Scan(&count); err != nil || count != 1 {
				return "", fmt.Errorf("required column %s.%s is missing", table, column)
			}
		}
	}
	var migration sql.NullString
	if err := db.QueryRowContext(ctx, "SELECT max(version) FROM __diesel_schema_migrations").Scan(&migration); err != nil {
		return "", fmt.Errorf("read database migration: %w", err)
	}
	if !migration.Valid || !validMigration(migration.String) {
		return "", fmt.Errorf("unsupported database migration")
	}
	return migration.String, nil
}

func referencedFiles(ctx context.Context, path string) ([]referencedFile, error) {
	db, err := sql.Open("sqlite3", sqliteURI(path, "ro"))
	if err != nil {
		return nil, err
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	var refs []referencedFile
	rows, err := db.QueryContext(ctx, "SELECT cipher_uuid, id, file_size FROM attachments ORDER BY cipher_uuid, id")
	if err != nil {
		return nil, fmt.Errorf("query attachment references: %w", err)
	}
	for rows.Next() {
		var cipherID, attachmentID string
		var size int64
		if err := rows.Scan(&cipherID, &attachmentID, &size); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("read attachment reference: %w", err)
		}
		if !validUUID(cipherID) || !validOpaqueID(attachmentID) || size < 0 {
			_ = rows.Close()
			return nil, fmt.Errorf("database contains an unsafe attachment reference")
		}
		refs = append(refs, referencedFile{Path: "data/attachments/" + cipherID + "/" + attachmentID, ExpectedSize: size})
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("iterate attachment references: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close attachment references: %w", err)
	}
	return refs, nil
}
