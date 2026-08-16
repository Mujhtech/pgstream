package migrator

import (
	"database/sql"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestBuildMySQLBatchQueryUsesCompositeKeyset(t *testing.T) {
	query, args, err := buildMySQLBatchQuery(
		"events",
		[]string{"tenant_id", "id", "payload"},
		[]string{"tenant_id", "id"},
		[]any{[]byte("acme"), int64(42)},
		5000,
		1000,
	)
	if err != nil {
		t.Fatalf("build query: %v", err)
	}

	wantQuery := "SELECT `tenant_id`, `id`, `payload` FROM `events` WHERE ((`tenant_id` > ?) OR (`tenant_id` = ? AND `id` > ?)) ORDER BY `tenant_id`, `id` LIMIT ?"
	if query != wantQuery {
		t.Fatalf("unexpected query\nwant: %s\n got: %s", wantQuery, query)
	}
	wantArgs := []any{[]byte("acme"), []byte("acme"), int64(42), 1000}
	if !reflect.DeepEqual(args, wantArgs) {
		t.Fatalf("unexpected args\nwant: %#v\n got: %#v", wantArgs, args)
	}
}

func TestBuildMySQLBatchQueryRejectsKeylessPagination(t *testing.T) {
	if _, _, err := buildMySQLBatchQuery("audit_log", []string{"event"}, nil, nil, 250, 50); err == nil {
		t.Fatal("expected keyless pagination to be rejected in favor of a single streaming query")
	}
}

func TestBuildMySQLStreamingQueryQuotesIdentifiersWithoutPagination(t *testing.T) {
	query, err := buildMySQLStreamingQuery("audit`log", []string{"id", "event"})
	if err != nil {
		t.Fatalf("build streaming query: %v", err)
	}
	want := "SELECT `id`, `event` FROM `audit``log`"
	if query != want {
		t.Fatalf("unexpected query\nwant: %s\n got: %s", want, query)
	}
}

func TestCursorRoundTripPreservesDatabaseTypes(t *testing.T) {
	want := []any{
		[]byte{0, 1, 2, 255},
		"customer-12",
		int64(-42),
		uint64(1<<63 + 10),
		12.5,
		true,
		time.Date(2026, time.July, 12, 10, 11, 12, 13, time.UTC),
	}

	encoded, err := encodeCursor(want)
	if err != nil {
		t.Fatalf("encode cursor: %v", err)
	}
	got, err := decodeCursor(encoded)
	if err != nil {
		t.Fatalf("decode cursor: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("cursor changed during round trip\nwant: %#v\n got: %#v", want, got)
	}
}

func TestBuildPostgresInsertCreatesOneMultiRowStatement(t *testing.T) {
	query, args, err := buildPostgresInsert(
		"tenant\"data",
		"user",
		[]string{"id", "display_name"},
		[]string{"id"},
		[][]any{{1, "Ada"}, {2, "Grace"}},
	)
	if err != nil {
		t.Fatalf("build insert: %v", err)
	}

	wantQuery := `INSERT INTO "tenant""data"."user" ("id", "display_name") VALUES ($1, $2), ($3, $4) ON CONFLICT ("id") DO UPDATE SET "display_name" = EXCLUDED."display_name"`
	if query != wantQuery {
		t.Fatalf("unexpected query\nwant: %s\n got: %s", wantQuery, query)
	}
	wantArgs := []any{1, "Ada", 2, "Grace"}
	if !reflect.DeepEqual(args, wantArgs) {
		t.Fatalf("unexpected args\nwant: %#v\n got: %#v", wantArgs, args)
	}
}

func TestBuildPostgresInsertWithoutKeyDoesNotHideConflicts(t *testing.T) {
	query, _, err := buildPostgresInsert(
		"public",
		"audit_log",
		[]string{"event"},
		nil,
		[][]any{{"created"}},
	)
	if err != nil {
		t.Fatalf("build insert: %v", err)
	}
	want := `INSERT INTO "public"."audit_log" ("event") VALUES ($1)`
	if query != want {
		t.Fatalf("unexpected query\nwant: %s\n got: %s", want, query)
	}
}

func TestPostgresInsertChunkSizeHonorsParameterLimit(t *testing.T) {
	if got := postgresInsertChunkSize(2); got != maxRowsPerInsert {
		t.Fatalf("expected row cap %d, got %d", maxRowsPerInsert, got)
	}
	if got := postgresInsertChunkSize(100); got != 655 {
		t.Fatalf("expected parameter-limited chunk of 655, got %d", got)
	}
}

func TestMySQLTypeMappingHandlesLargeMigrationTypes(t *testing.T) {
	migrator := &Migrator{schemaName: "public"}
	tests := map[string]string{
		"tinyint(1) unsigned":    "BOOLEAN",
		"varchar(36)":            "VARCHAR(36)",
		"binary(16)":             "BYTEA",
		"bigint unsigned":        "NUMERIC(20)",
		"int unsigned":           "BIGINT",
		"varbinary(255)":         "BYTEA",
		"bit(8)":                 "BYTEA",
		"year":                   "SMALLINT",
		"geometry":               "BYTEA",
		"point":                  "BYTEA",
		"multipoint":             "BYTEA",
		"decimal(20,4) unsigned": "DECIMAL(20,4)",
	}
	for mysqlType, want := range tests {
		t.Run(mysqlType, func(t *testing.T) {
			got, err := migrator.convertMySQLTypeToPostgres(mysqlType, "value", "events")
			if err != nil {
				t.Fatalf("convert type: %v", err)
			}
			if got != want {
				t.Fatalf("expected %s to map to %s, got %s", mysqlType, want, got)
			}
		})
	}
	if _, err := migrator.convertMySQLTypeToPostgres("vector(3)", "embedding", "events"); err == nil {
		t.Fatal("expected unknown types to fail rather than silently map to text")
	}
}

func TestBuildCreateTableSQLRejectsGeneratedBehaviorItCannotPreserve(t *testing.T) {
	migrator := &Migrator{schemaName: "public"}
	// Expression defaults cannot be preserved and still fail closed.
	if _, err := migrator.buildCreateTableSQL("events", []ColumnInfo{
		{Name: "token", Type: "varchar(36)", Extra: "DEFAULT_GENERATED", HasDefault: true, DefaultValue: "uuid()"},
	}); err == nil {
		t.Fatal("expected expression default to fail")
	}

	// ON UPDATE CURRENT_TIMESTAMP is accepted: the column migrates normally
	// and the auto-update behavior becomes trigger DDL in the manual work
	// file during schema object migration.
	if _, err := migrator.buildCreateTableSQL("events", []ColumnInfo{
		{Name: "updated_at", Type: "timestamp", Extra: "DEFAULT_GENERATED on update CURRENT_TIMESTAMP", HasDefault: true, DefaultValue: "CURRENT_TIMESTAMP"},
	}); err != nil {
		t.Fatalf("ON UPDATE CURRENT_TIMESTAMP column must build: %v", err)
	}
}

func TestBuildColumnDefinitionMigratesGeneratedColumnsAsPlainColumns(t *testing.T) {
	migrator := &Migrator{schemaName: "public"}
	for _, extra := range []string{"STORED GENERATED", "VIRTUAL GENERATED"} {
		def, pgType, err := migrator.buildColumnDefinition("transactions", ColumnInfo{
			Name:                 "is_international_payout",
			Type:                 "tinyint(1)",
			Nullable:             true,
			Extra:                extra,
			GenerationExpression: "(`type` = 'INTERNATIONAL')",
		})
		if err != nil {
			t.Fatalf("%s column must build as a plain column: %v", extra, err)
		}
		if pgType != "BOOLEAN" {
			t.Fatalf("unexpected type %q", pgType)
		}
		if def != `"is_international_payout" BOOLEAN` {
			t.Fatalf("generated column must carry no default or identity, got %q", def)
		}
	}
}

func TestValueTransformationPreservesTextAndBinaryData(t *testing.T) {
	migrator := &Migrator{}
	got, err := migrator.validateAndTransformValue([]byte{}, "text")
	if err != nil {
		t.Fatalf("transform empty text: %v", err)
	}
	if got != "" {
		t.Fatalf("expected empty text to remain empty, got %#v", got)
	}
	got, err = migrator.validateAndTransformValue([]byte("  preserve me  "), "character varying")
	if err != nil {
		t.Fatalf("transform padded text: %v", err)
	}
	if got != "  preserve me  " {
		t.Fatalf("expected whitespace to be preserved, got %#v", got)
	}
	binary := []byte{0, 1, 2}
	got, err = migrator.validateAndTransformValue(binary, "bytea")
	if err != nil {
		t.Fatalf("transform binary: %v", err)
	}
	if !reflect.DeepEqual(got, binary) {
		t.Fatalf("expected bytea to remain binary, got %#v", got)
	}
	got, err = migrator.validateAndTransformValue([]byte{1}, "boolean")
	if err != nil {
		t.Fatalf("transform boolean: %v", err)
	}
	if got != true {
		t.Fatalf("expected bit value 1 to become true, got %#v", got)
	}
}

func TestValueTransformationRejectsLossyDateConversion(t *testing.T) {
	migrator := &Migrator{}
	if _, err := migrator.validateAndTransformValue([]byte("0000-00-00"), "date"); err == nil {
		t.Fatal("expected zero date to fail instead of becoming NULL")
	}
	if _, err := migrator.validateAndTransformValue([]byte("01/02/2026"), "date"); err == nil {
		t.Fatal("expected ambiguous non-ISO date to fail")
	}
	for _, value := range []string{"12:34:56.1234", "2026-07-13 12:34:56.12345"} {
		dataType := "time"
		if strings.Contains(value, "-") {
			dataType = "timestamp without time zone"
		}
		if _, err := migrator.validateAndTransformValue([]byte(value), dataType); err != nil {
			t.Fatalf("expected MySQL fractional temporal value %q to remain valid: %v", value, err)
		}
	}
}

func TestExtractEnumValuesHandlesCommasQuotesAndEmptyValues(t *testing.T) {
	migrator := &Migrator{}
	got := migrator.extractEnumValues(`enum('ready','needs, review','it\'s done','')`)
	want := []string{"ready", "needs, review", "it's done", ""}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected enum values\nwant: %#v\n got: %#v", want, got)
	}
}

func TestBuildCreateTableSQLQuotesNamesAndMapsDefaults(t *testing.T) {
	migrator := &Migrator{schemaName: `tenant"data`}
	query, err := migrator.buildCreateTableSQL(`order`, []ColumnInfo{
		{Name: "id", Type: "bigint", Nullable: false, Extra: "auto_increment"},
		{Name: "external_id", Type: "char(36)", Nullable: false, IsUUID: true},
		{Name: "status", Type: "varchar(20)", Nullable: false, HasDefault: true, DefaultValue: "it's ready"},
		{Name: "enabled", Type: "tinyint(1)", Nullable: false, HasDefault: true, DefaultValue: "1"},
	})
	if err != nil {
		t.Fatalf("build create table SQL: %v", err)
	}
	want := `CREATE TABLE IF NOT EXISTS "tenant""data"."order" ("id" BIGINT NOT NULL GENERATED BY DEFAULT AS IDENTITY, "external_id" UUID NOT NULL, "status" VARCHAR(20) NOT NULL DEFAULT 'it''s ready', "enabled" BOOLEAN NOT NULL DEFAULT TRUE)`
	if query != want {
		t.Fatalf("unexpected create table SQL\nwant: %s\n got: %s", want, query)
	}
}

func TestBuildCreateTableSQLRejectsUnsupportedAutoIncrementType(t *testing.T) {
	migrator := &Migrator{schemaName: "public"}
	if _, err := migrator.buildCreateTableSQL("events", []ColumnInfo{{
		Name: "id", Type: "bigint unsigned", Extra: "auto_increment",
	}}); err == nil {
		t.Fatal("expected unsigned bigint AUTO_INCREMENT to fail instead of losing generation behavior")
	}
}

func TestSequenceStatePreservesSourceNextValueAndTargetMaximum(t *testing.T) {
	tests := []struct {
		name       string
		maximum    sql.NullInt64
		sourceNext sql.NullInt64
		wantValue  int64
		wantCalled bool
	}{
		{name: "empty default", wantValue: 1, wantCalled: false},
		{name: "custom source start", sourceNext: sql.NullInt64{Int64: 100, Valid: true}, wantValue: 99, wantCalled: true},
		{name: "target ahead", maximum: sql.NullInt64{Int64: 150, Valid: true}, sourceNext: sql.NullInt64{Int64: 100, Valid: true}, wantValue: 150, wantCalled: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			value, called, err := sequenceState(test.maximum, test.sourceNext)
			if err != nil || value != test.wantValue || called != test.wantCalled {
				t.Fatalf("sequence state: value=%d called=%v err=%v", value, called, err)
			}
		})
	}
}

func TestColumnMappingRejectsFuzzyMatches(t *testing.T) {
	migrator := &Migrator{}
	if _, err := migrator.mapColumnsToPostgres([]string{"id"}, []string{"user_id"}); err == nil {
		t.Fatal("expected fuzzy id to user_id mapping to be rejected")
	}
	got, err := migrator.mapColumnsToPostgres([]string{"DisplayName"}, []string{"displayname"})
	if err != nil {
		t.Fatalf("expected case-insensitive exact mapping: %v", err)
	}
	if !reflect.DeepEqual(got, []string{"displayname"}) {
		t.Fatalf("unexpected mapped columns: %#v", got)
	}
}
