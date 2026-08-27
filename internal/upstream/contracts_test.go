package upstream

import (
	"slices"
	"testing"
)

func TestEmbeddedSchemaAndProjectionContractsLoad(t *testing.T) {
	t.Parallel()
	schema, err := VaultwardenSchemaContract()
	if err != nil {
		t.Fatal(err)
	}
	projection, err := VaultwardenRecoveryProjection()
	if err != nil {
		t.Fatal(err)
	}
	tables := ProjectionTablesByName(projection)
	if len(tables) == 0 || schema.VaultwardenVersion == "" || schema.DatabaseMigration == "" {
		t.Fatal("embedded contracts are incomplete")
	}
	if tables["__diesel_schema_migrations"].Disposition != "validate_only" {
		t.Fatal("migration ledger must be validation-only")
	}
	if !slices.Equal(tables["users"].ExcludedColumns, []string{
		"updated_at", "last_verifying_at", "login_verify_count", "stamp_exception",
	}) {
		t.Fatal("users exclusions changed")
	}
}

func TestEmbeddedContractResultsAreDefensiveCopies(t *testing.T) {
	t.Parallel()
	schema, err := VaultwardenSchemaContract()
	if err != nil {
		t.Fatal(err)
	}
	schema.Objects[0].Name = "changed"
	again, err := VaultwardenSchemaContract()
	if err != nil {
		t.Fatal(err)
	}
	if again.Objects[0].Name == "changed" {
		t.Fatal("schema result shares mutable storage")
	}

	projection, err := VaultwardenRecoveryProjection()
	if err != nil {
		t.Fatal(err)
	}
	projection.Tables[0].Columns[0] = "changed"
	againProjection, err := VaultwardenRecoveryProjection()
	if err != nil {
		t.Fatal(err)
	}
	if againProjection.Tables[0].Columns[0] == "changed" {
		t.Fatal("projection result shares mutable storage")
	}
}
