package migrator

import (
	"strings"
	"testing"
	"time"
)

func TestUUIDInvalidPredicateByStorageType(t *testing.T) {
	if got := uuidInvalidPredicate("id", "varchar(36)"); !strings.Contains(got, "NOT REGEXP") {
		t.Fatalf("varchar candidates must validate against the UUID pattern: %s", got)
	}
	if got := uuidInvalidPredicate("id", "varbinary(16)"); !strings.Contains(got, "LENGTH(`id`) <> 16") {
		t.Fatalf("binary candidates must validate by length: %s", got)
	}
	if got := uuidInvalidPredicate("id", "binary(16)"); strings.Contains(got, "REGEXP") {
		t.Fatalf("binary(16) must not use the text pattern: %s", got)
	}
}

func TestZeroDateTransform(t *testing.T) {
	m := &Migrator{}
	nullablePlans := []columnPlan{{name: "createdAt", dataType: "timestamp without time zone", kind: kindTimestamp, nullable: true}}
	rows := [][]any{{[]byte("0000-00-00 00:00:00")}, {[]byte("2025-07-19 08:08:54")}}
	if err := m.transformRows("profile_metas", nullablePlans, rows); err != nil {
		t.Fatalf("nullable zero date must convert: %v", err)
	}
	if rows[0][0] != nil {
		t.Fatalf("zero date must become NULL, got %#v", rows[0][0])
	}
	if rows[1][0] != "2025-07-19 08:08:54" {
		t.Fatalf("real timestamps must pass through, got %#v", rows[1][0])
	}

	notNullPlans := []columnPlan{{name: "createdAt", dataType: "timestamp without time zone", kind: kindTimestamp, nullable: false}}
	err := m.transformRows("profile_metas", notNullPlans, [][]any{{[]byte("0000-00-00 00:00:00")}})
	if err == nil {
		t.Fatal("NOT NULL zero date must fail closed")
	}
	if !strings.Contains(err.Error(), "zero date") || !strings.Contains(err.Error(), "NOT NULL") {
		t.Fatalf("error must explain the zero-date situation: %v", err)
	}
}

func TestHasZeroDatePrefix(t *testing.T) {
	for _, value := range []any{[]byte("0000-00-00"), []byte("0000-00-00 00:00:00"), "0000-00-00 00:00:00.000000"} {
		if !hasZeroDatePrefix(value) {
			t.Fatalf("expected %v to be a zero date", value)
		}
	}
	for _, value := range []any{[]byte("2024-01-01"), "2024-00-00", int64(0), nil} {
		if hasZeroDatePrefix(value) {
			t.Fatalf("expected %v not to be a zero date", value)
		}
	}
}

func TestFormatDurationScales(t *testing.T) {
	cases := map[time.Duration]string{
		37 * time.Millisecond:   "37ms",
		2340 * time.Millisecond: "2.3s",
		95 * time.Second:        "1m35s",
		61*time.Minute + 2*time.Second + 700*time.Millisecond: "1h1m3s",
	}
	for input, want := range cases {
		if got := formatDuration(input); got != want {
			t.Fatalf("formatDuration(%v) = %q, want %q", input, got, want)
		}
	}
}
