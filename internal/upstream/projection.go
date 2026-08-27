package upstream

import (
	_ "embed"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
)

type ProjectionTable struct {
	Name            string   `json:"name"`
	Disposition     string   `json:"disposition"`
	RowKey          []string `json:"row_key"`
	Columns         []string `json:"columns"`
	ExcludedColumns []string `json:"excluded_columns"`
}

type RecoveryProjection struct {
	SchemaVersion  int               `json:"schema_version"`
	SchemaContract string            `json:"schema_contract"`
	Tables         []ProjectionTable `json:"tables"`
}

//go:embed recovery-projection.json
var recoveryProjectionJSON []byte

var (
	loadProjectionOnce  sync.Once
	loadedProjection    RecoveryProjection
	loadedProjectionErr error
)

func VaultwardenRecoveryProjection() (RecoveryProjection, error) {
	loadProjectionOnce.Do(func() {
		if err := decodeStrictJSON(recoveryProjectionJSON, &loadedProjection); err != nil {
			loadedProjectionErr = fmt.Errorf("decode embedded recovery projection: %w", err)
			return
		}
		if err := validateProjection(loadedProjection); err != nil {
			loadedProjectionErr = err
		}
	})
	result := loadedProjection
	result.Tables = cloneProjectionTables(loadedProjection.Tables)
	return result, loadedProjectionErr
}

func validateProjection(projection RecoveryProjection) error {
	if projection.SchemaVersion != 1 ||
		projection.SchemaContract != "internal/upstream/vaultwarden-schema.json" ||
		len(projection.Tables) == 0 {
		return fmt.Errorf("embedded recovery projection header is inconsistent")
	}
	seenTables := make(map[string]struct{}, len(projection.Tables))
	for index, table := range projection.Tables {
		if table.Name == "" || len(table.Columns) == 0 || len(table.RowKey) == 0 {
			return fmt.Errorf("embedded recovery projection table %d is incomplete", index)
		}
		if _, duplicate := seenTables[table.Name]; duplicate {
			return fmt.Errorf("embedded recovery projection contains a duplicate table")
		}
		seenTables[table.Name] = struct{}{}
		if index > 0 && strings.Compare(projection.Tables[index-1].Name, table.Name) >= 0 {
			return fmt.Errorf("embedded recovery projection tables are not strictly sorted")
		}
		if table.Disposition != "included" && table.Disposition != "excluded" && table.Disposition != "validate_only" {
			return fmt.Errorf("embedded recovery projection table %s has an invalid disposition", table.Name)
		}
		columns := make(map[string]struct{}, len(table.Columns))
		for _, column := range table.Columns {
			if column == "" {
				return fmt.Errorf("embedded recovery projection table %s has an empty column", table.Name)
			}
			if _, duplicate := columns[column]; duplicate {
				return fmt.Errorf("embedded recovery projection table %s has a duplicate column", table.Name)
			}
			columns[column] = struct{}{}
		}
		for _, column := range append(slices.Clone(table.RowKey), table.ExcludedColumns...) {
			if _, exists := columns[column]; !exists {
				return fmt.Errorf("embedded recovery projection table %s references an unknown column", table.Name)
			}
		}
		rowKeys := make(map[string]struct{}, len(table.RowKey))
		for _, column := range table.RowKey {
			if _, duplicate := rowKeys[column]; duplicate {
				return fmt.Errorf("embedded recovery projection table %s has a duplicate row key", table.Name)
			}
			rowKeys[column] = struct{}{}
		}
		for _, column := range table.ExcludedColumns {
			if _, rowKey := rowKeys[column]; rowKey {
				return fmt.Errorf("embedded recovery projection table %s excludes a row key", table.Name)
			}
		}
		if table.Disposition != "included" && len(table.ExcludedColumns) != 0 {
			return fmt.Errorf("embedded recovery projection table %s excludes columns from a non-included table", table.Name)
		}
	}
	return nil
}

func cloneProjectionTables(tables []ProjectionTable) []ProjectionTable {
	result := slices.Clone(tables)
	for index := range result {
		result[index].RowKey = slices.Clone(result[index].RowKey)
		result[index].Columns = slices.Clone(result[index].Columns)
		result[index].ExcludedColumns = slices.Clone(result[index].ExcludedColumns)
	}
	return result
}

// ProjectionTablesByName returns a defensive map for consumers that validate
// a live SQLite schema against the human-reviewed projection.
func ProjectionTablesByName(projection RecoveryProjection) map[string]ProjectionTable {
	result := make(map[string]ProjectionTable, len(projection.Tables))
	for _, table := range cloneProjectionTables(projection.Tables) {
		result[table.Name] = table
	}
	return maps.Clone(result)
}
