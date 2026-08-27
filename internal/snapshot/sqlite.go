package snapshot

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/hellodword/vaultwarden-less/internal/upstream"
	"github.com/mattn/go-sqlite3"
)

type referencedFile struct {
	Path         string
	ExpectedSize int64
}

type databaseValidation struct {
	Migration         string
	SchemaFingerprint string
}

// DatabaseContract is the audited migration and schema identity of one fully
// validated Vaultwarden database.
type DatabaseContract struct {
	Migration         string
	SchemaFingerprint string
}

// ValidateDatabaseContract runs the same integrity, foreign-key, schema,
// projection, and migration checks used for published recovery points.
func ValidateDatabaseContract(ctx context.Context, path string) (DatabaseContract, error) {
	validated, err := validateDatabase(ctx, path)
	if err != nil {
		return DatabaseContract{}, err
	}
	return DatabaseContract(validated), nil
}

var unsignedIntegerPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)$`)

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
	file, err := os.OpenFile(destinationPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create destination database: %w", err)
	}
	if err := file.Close(); err != nil {
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

	sourceConnection, err := source.Conn(ctx)
	if err != nil {
		return fmt.Errorf("reserve source connection: %w", err)
	}
	defer func() { _ = sourceConnection.Close() }()
	destinationConnection, err := destination.Conn(ctx)
	if err != nil {
		return fmt.Errorf("reserve destination connection: %w", err)
	}
	defer func() { _ = destinationConnection.Close() }()

	err = sourceConnection.Raw(func(rawSource any) error {
		sourceSQLite, ok := rawSource.(*sqlite3.SQLiteConn)
		if !ok {
			return fmt.Errorf("unexpected source SQLite driver")
		}
		return destinationConnection.Raw(func(rawDestination any) error {
			destinationSQLite, ok := rawDestination.(*sqlite3.SQLiteConn)
			if !ok {
				return fmt.Errorf("unexpected destination SQLite driver")
			}
			backup, err := destinationSQLite.Backup("main", sourceSQLite, "main")
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
	if err := destinationConnection.QueryRowContext(ctx, "PRAGMA journal_mode = DELETE").Scan(&journalMode); err != nil || journalMode != "delete" {
		return fmt.Errorf("normalize recovery database journal mode")
	}
	if err := destinationConnection.Close(); err != nil {
		return fmt.Errorf("close destination connection: %w", err)
	}
	if err := sourceConnection.Close(); err != nil {
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
			if !info.Mode().IsRegular() || info.Size() != 0 {
				return fmt.Errorf("normalized recovery database has an unsafe sidecar")
			}
			if err := os.Remove(sidecar); err != nil {
				return fmt.Errorf("remove normalized recovery sidecar: %w", err)
			}
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("inspect normalized recovery sidecar: %w", err)
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

func validateDatabase(ctx context.Context, path string) (databaseValidation, error) {
	database, err := sql.Open("sqlite3", sqliteURI(path, "ro"))
	if err != nil {
		return databaseValidation{}, fmt.Errorf("open recovery database: %w", err)
	}
	defer func() { _ = database.Close() }()
	database.SetMaxOpenConns(1)
	database.SetMaxIdleConns(1)

	rows, err := database.QueryContext(ctx, "PRAGMA integrity_check")
	if err != nil {
		return databaseValidation{}, fmt.Errorf("run integrity_check: %w", err)
	}
	var integrityResults []string
	for rows.Next() {
		var result string
		if err := rows.Scan(&result); err != nil {
			_ = rows.Close()
			return databaseValidation{}, fmt.Errorf("read integrity_check: %w", err)
		}
		integrityResults = append(integrityResults, result)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return databaseValidation{}, fmt.Errorf("iterate integrity_check: %w", err)
	}
	if err := rows.Close(); err != nil {
		return databaseValidation{}, fmt.Errorf("close integrity_check: %w", err)
	}
	if !slices.Equal(integrityResults, []string{"ok"}) {
		return databaseValidation{}, fmt.Errorf("integrity_check failed")
	}

	rows, err = database.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		return databaseValidation{}, fmt.Errorf("run foreign_key_check: %w", err)
	}
	if rows.Next() {
		_ = rows.Close()
		return databaseValidation{}, fmt.Errorf("foreign_key_check failed")
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return databaseValidation{}, fmt.Errorf("iterate foreign_key_check: %w", err)
	}
	if err := rows.Close(); err != nil {
		return databaseValidation{}, fmt.Errorf("close foreign_key_check: %w", err)
	}

	contract, err := upstream.VaultwardenSchemaContract()
	if err != nil {
		return databaseValidation{}, err
	}
	rows, err = database.QueryContext(ctx, `
		SELECT type, name, tbl_name, coalesce(sql, '')
		FROM sqlite_schema
		WHERE type IN ('index', 'table', 'trigger', 'view')
		  AND name NOT LIKE 'sqlite_%'
		ORDER BY type, name
	`)
	if err != nil {
		return databaseValidation{}, fmt.Errorf("read database schema: %w", err)
	}
	objects := make([]upstream.SchemaObject, 0, len(contract.Objects))
	for rows.Next() {
		var object upstream.SchemaObject
		if err := rows.Scan(&object.Type, &object.Name, &object.Table, &object.SQL); err != nil {
			_ = rows.Close()
			return databaseValidation{}, fmt.Errorf("read database schema object: %w", err)
		}
		object.SQL = strings.TrimSpace(object.SQL)
		objects = append(objects, object)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return databaseValidation{}, fmt.Errorf("iterate database schema: %w", err)
	}
	if err := rows.Close(); err != nil {
		return databaseValidation{}, fmt.Errorf("close database schema: %w", err)
	}
	if !slices.Equal(objects, contract.Objects) {
		return databaseValidation{}, fmt.Errorf("database schema does not match audited Vaultwarden contract")
	}
	schemaFingerprint := upstream.SchemaFingerprint(objects)
	if schemaFingerprint != contract.Fingerprint {
		return databaseValidation{}, fmt.Errorf("database schema fingerprint mismatch")
	}
	if err := validateProjectionSchema(ctx, database, contract); err != nil {
		return databaseValidation{}, err
	}
	var migration sql.NullString
	if err := database.QueryRowContext(ctx, "SELECT max(version) FROM __diesel_schema_migrations").Scan(&migration); err != nil {
		return databaseValidation{}, fmt.Errorf("read database migration: %w", err)
	}
	if !migration.Valid || migration.String != contract.DatabaseMigration {
		return databaseValidation{}, fmt.Errorf("unsupported database migration")
	}
	return databaseValidation{Migration: migration.String, SchemaFingerprint: schemaFingerprint}, nil
}

func validateProjectionSchema(ctx context.Context, database *sql.DB, contract upstream.SchemaContract) error {
	projection, err := upstream.VaultwardenRecoveryProjection()
	if err != nil {
		return err
	}
	tables := make(map[string]struct{})
	for _, object := range contract.Objects {
		if object.Type == "table" {
			tables[object.Name] = struct{}{}
		}
	}
	if len(tables) != len(projection.Tables) {
		return fmt.Errorf("recovery projection table set does not match schema")
	}
	for _, table := range projection.Tables {
		if _, exists := tables[table.Name]; !exists {
			return fmt.Errorf("recovery projection contains an unknown table")
		}
		rows, err := database.QueryContext(ctx, "PRAGMA table_info("+quoteIdentifier(table.Name)+")")
		if err != nil {
			return fmt.Errorf("read projected table %s: %w", table.Name, err)
		}
		columns := make([]string, 0, len(table.Columns))
		for rows.Next() {
			var sequence, notNull, primaryKey int
			var name, declaredType string
			var defaultValue any
			if err := rows.Scan(&sequence, &name, &declaredType, &notNull, &defaultValue, &primaryKey); err != nil {
				_ = rows.Close()
				return fmt.Errorf("read projected column for %s: %w", table.Name, err)
			}
			if sequence != len(columns) {
				_ = rows.Close()
				return fmt.Errorf("projected columns for %s are not in schema order", table.Name)
			}
			columns = append(columns, name)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return fmt.Errorf("iterate projected columns for %s: %w", table.Name, err)
		}
		if err := rows.Close(); err != nil {
			return fmt.Errorf("close projected columns for %s: %w", table.Name, err)
		}
		if !slices.Equal(columns, table.Columns) {
			return fmt.Errorf("recovery projection columns do not match schema for %s", table.Name)
		}
	}
	return nil
}

func referencedFiles(ctx context.Context, path string) ([]referencedFile, error) {
	database, err := sql.Open("sqlite3", sqliteURI(path, "ro"))
	if err != nil {
		return nil, fmt.Errorf("open recovery database references: %w", err)
	}
	defer func() { _ = database.Close() }()
	database.SetMaxOpenConns(1)
	database.SetMaxIdleConns(1)

	refs := make([]referencedFile, 0)
	rows, err := database.QueryContext(ctx, "SELECT cipher_uuid, id, file_size FROM attachments ORDER BY cipher_uuid, id")
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
		if !validUUID(cipherID) || !validAttachmentID(attachmentID) || size < 0 {
			_ = rows.Close()
			return nil, fmt.Errorf("database contains an unsafe attachment reference")
		}
		refs = append(refs, referencedFile{Path: "attachments/" + cipherID + "/" + attachmentID, ExpectedSize: size})
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("iterate attachment references: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close attachment references: %w", err)
	}

	rows, err = database.QueryContext(ctx, "SELECT uuid, data FROM sends WHERE atype = 1 ORDER BY uuid")
	if err != nil {
		return nil, fmt.Errorf("query file Send references: %w", err)
	}
	for rows.Next() {
		var sendID, data string
		if err := rows.Scan(&sendID, &data); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("read file Send reference: %w", err)
		}
		fileID, size, err := decodeSendFileData(data)
		if err != nil || !validUUID(sendID) || !validSendFileID(fileID) || size < 0 {
			_ = rows.Close()
			return nil, fmt.Errorf("database contains an unsafe file Send reference")
		}
		refs = append(refs, referencedFile{Path: "sends/" + sendID + "/" + fileID, ExpectedSize: size})
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("iterate file Send references: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close file Send references: %w", err)
	}

	slices.SortFunc(refs, func(left, right referencedFile) int {
		return strings.Compare(left.Path, right.Path)
	})
	for index := 1; index < len(refs); index++ {
		if refs[index-1].Path == refs[index].Path {
			return nil, fmt.Errorf("database contains duplicate file references")
		}
	}
	return refs, nil
}

func decodeSendFileData(data string) (string, int64, error) {
	if !utf8.ValidString(data) || rejectDuplicateJSONKeys([]byte(data)) != nil {
		return "", 0, fmt.Errorf("invalid file Send data")
	}
	type sendFileData struct {
		ID       string          `json:"id"`
		Size     json.RawMessage `json:"size"`
		FileName *string         `json:"fileName"`
		SizeName *string         `json:"sizeName"`
	}
	decoder := json.NewDecoder(strings.NewReader(data))
	decoder.DisallowUnknownFields()
	var object sendFileData
	if err := decoder.Decode(&object); err != nil || object.FileName == nil || object.SizeName == nil {
		return "", 0, fmt.Errorf("invalid file Send data")
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return "", 0, fmt.Errorf("invalid file Send data")
	}
	if object.ID == "" {
		return "", 0, fmt.Errorf("invalid file Send ID")
	}
	rawSize := bytes.TrimSpace(object.Size)
	if !unsignedIntegerPattern.Match(rawSize) {
		return "", 0, fmt.Errorf("invalid file Send size")
	}
	size, err := json.Number(rawSize).Int64()
	if err != nil {
		return "", 0, fmt.Errorf("invalid file Send size")
	}
	return object.ID, size, nil
}
