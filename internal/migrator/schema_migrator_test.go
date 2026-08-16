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

func TestBuildForeignKeySQLRendersRulesAndQuoting(t *testing.T) {
	sm := &SchemaMigrator{schema: "app"}
	fk := ForeignKeyInfo{
		ConstraintName:    "fk_orders_customer",
		TableName:         "orders",
		ColumnNames:       []string{"customer_id", "region"},
		ReferencedTable:   "customers",
		ReferencedColumns: []string{"id", "region"},
		OnDelete:          "CASCADE",
		OnUpdate:          "NO ACTION",
	}
	got := sm.buildForeignKeySQL(fk, "fk_orders_customer")
	want := `ALTER TABLE "app"."orders" ADD CONSTRAINT "fk_orders_customer" FOREIGN KEY ("customer_id", "region") REFERENCES "app"."customers" ("id", "region") ON DELETE CASCADE`
	if got != want {
		t.Fatalf("unexpected constraint SQL\nwant: %s\n got: %s", want, got)
	}
}

func TestBuildColumnDefinitionAcceptsOnUpdateCurrentTimestamp(t *testing.T) {
	m := &Migrator{schemaName: "app"}
	def, pgType, err := m.buildColumnDefinition("orders", ColumnInfo{
		Name:         "updatedAt",
		Type:         "timestamp",
		Nullable:     false,
		HasDefault:   true,
		DefaultValue: "CURRENT_TIMESTAMP",
		Extra:        "DEFAULT_GENERATED on update CURRENT_TIMESTAMP",
	})
	if err != nil {
		t.Fatalf("ON UPDATE CURRENT_TIMESTAMP column must be accepted: %v", err)
	}
	if pgType != "TIMESTAMP" {
		t.Fatalf("unexpected type %q", pgType)
	}
	want := `"updatedAt" TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP`
	if def != want {
		t.Fatalf("unexpected definition\nwant: %s\n got: %s", want, def)
	}
}

func TestBuildAutoUpdateTriggerSQL(t *testing.T) {
	sm := &SchemaMigrator{schema: "app"}
	sql := sm.buildAutoUpdateTriggerSQL("orders", "updatedAt")
	for _, fragment := range []string{
		`RETURNS trigger`,
		`IF NEW."updatedAt" IS NOT DISTINCT FROM OLD."updatedAt" THEN`,
		`NEW."updatedAt" := CURRENT_TIMESTAMP;`,
		`BEFORE UPDATE ON "app"."orders" FOR EACH ROW`,
	} {
		if !strings.Contains(sql, fragment) {
			t.Fatalf("trigger SQL missing %q:\n%s", fragment, sql)
		}
	}
}
