package migrator

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestPostgresIndexNameIsTableScopedAndBounded(t *testing.T) {
	first := postgresIndexName("users", "idx_status")
	second := postgresIndexName("orders", "idx_status")
	if first == second {
		t.Fatalf("expected table-scoped index names, got %q", first)
	}
	if postgresIndexName("a", "b_c") == postgresIndexName("a_b", "c") {
		t.Fatal("different table/index pairs must not collapse to the same underscore-delimited name")
	}

	longName := postgresIndexName(strings.Repeat("é", 40), strings.Repeat("x", 80))
	if len(longName) > 63 {
		t.Fatalf("index name is %d bytes; PostgreSQL allows 63", len(longName))
	}
	if !utf8.ValidString(longName) {
		t.Fatalf("index name is not valid UTF-8: %q", longName)
	}
}

func TestGeneratedPostgresObjectNamesAreBoundedAndUnambiguous(t *testing.T) {
	firstEnum := postgresEnumTypeName("a", "b_c")
	secondEnum := postgresEnumTypeName("a_b", "c")
	if firstEnum == secondEnum {
		t.Fatal("different table/column pairs must not produce the same enum type name")
	}
	if len(firstEnum) > maxPostgresIdentifierBytes || !utf8.ValidString(firstEnum) {
		t.Fatalf("invalid generated enum name %q", firstEnum)
	}
	primaryKeyName := postgresPrimaryKeyName(strings.Repeat("table", 12))
	if len(primaryKeyName) > maxPostgresIdentifierBytes || !utf8.ValidString(primaryKeyName) {
		t.Fatalf("invalid primary key name %q", primaryKeyName)
	}

	longConstraint := strings.Repeat("é", 40)
	bounded := boundedPostgresObjectName(longConstraint, "orders\x00"+longConstraint)
	if len(bounded) > maxPostgresIdentifierBytes || !utf8.ValidString(bounded) {
		t.Fatalf("invalid bounded constraint name %q", bounded)
	}
}

func TestValidatePostgresIdentifierRejectsSilentTruncation(t *testing.T) {
	if err := validatePostgresIdentifier(strings.Repeat("x", maxPostgresIdentifierBytes), "table"); err != nil {
		t.Fatalf("valid identifier rejected: %v", err)
	}
	if err := validatePostgresIdentifier(strings.Repeat("x", maxPostgresIdentifierBytes+1), "table"); err == nil {
		t.Fatal("expected overlong identifier to be rejected")
	}
}

func TestPostgresIndexColumnPreservesPrefixAndDirection(t *testing.T) {
	got, err := postgresIndexColumnSQL(IndexColumnInfo{
		Name:          "email",
		PrefixLength:  12,
		SortDirection: "D",
	})
	if err != nil {
		t.Fatalf("convert index column: %v", err)
	}
	want := `substring("email" FROM 1 FOR 12) DESC`
	if got != want {
		t.Fatalf("unexpected index column\nwant: %s\n got: %s", want, got)
	}
}

func TestNormalizeForeignKeyRuleRejectsUnknownRule(t *testing.T) {
	if got, err := normalizeForeignKeyRule(" cascade "); err != nil || got != "CASCADE" {
		t.Fatalf("normalize cascade: got=%q err=%v", got, err)
	}
	if _, err := normalizeForeignKeyRule("DROP TABLE"); err == nil {
		t.Fatal("expected unsupported foreign key rule to fail")
	}
}

func TestEqualIdentifierListsChecksOrder(t *testing.T) {
	if !equalIdentifierLists([]string{"Tenant_ID", "id"}, []string{"tenant_id", "ID"}) {
		t.Fatal("expected case-insensitive identifier match")
	}
	if equalIdentifierLists([]string{"tenant_id", "id"}, []string{"id", "tenant_id"}) {
		t.Fatal("primary key column order must be significant")
	}
}
