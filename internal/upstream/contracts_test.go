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

func TestRecoveryProjectionMatchesReviewedPolicy(t *testing.T) {
	t.Parallel()
	projection, err := VaultwardenRecoveryProjection()
	if err != nil {
		t.Fatal(err)
	}
	tables := ProjectionTablesByName(projection)
	excluded := map[string]bool{
		"auth_requests": true, "devices": true, "event": true,
		"sso_auth": true, "twofactor_duo_ctx": true, "twofactor_incomplete": true,
	}
	for name, table := range tables {
		wantDisposition := "included"
		if excluded[name] {
			wantDisposition = "excluded"
		}
		if name == "__diesel_schema_migrations" {
			wantDisposition = "validate_only"
		}
		if table.Disposition != wantDisposition {
			t.Fatalf("table %s disposition = %s, want %s", name, table.Disposition, wantDisposition)
		}
		if name != "users" && name != "twofactor" && len(table.ExcludedColumns) != 0 {
			t.Fatalf("table %s has unexpected excluded columns %#v", name, table.ExcludedColumns)
		}
	}
	if !slices.Equal(tables["users"].ExcludedColumns, []string{"updated_at", "last_verifying_at", "login_verify_count", "stamp_exception"}) ||
		!slices.Equal(tables["twofactor"].ExcludedColumns, []string{"last_used"}) {
		t.Fatal("reviewed column exclusions changed")
	}
	if !slices.Equal(tables["collections_groups"].RowKey, []string{"collections_uuid", "groups_uuid"}) ||
		!slices.Equal(tables["groups_users"].RowKey, []string{"groups_uuid", "users_organizations_uuid"}) {
		t.Fatal("reviewed composite row keys changed")
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
