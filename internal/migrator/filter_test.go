package migrator

import (
	"reflect"
	"strings"
	"testing"
)

func TestNewTableFilterRejectsCombinedSelections(t *testing.T) {
	if _, err := newTableFilter([]string{"users"}, []string{"audit_log"}); err == nil {
		t.Fatal("expected combined include and exclude selections to be rejected")
	}
}

func TestNewTableFilterRejectsEmptyNames(t *testing.T) {
	if _, err := newTableFilter([]string{"users", "  "}, nil); err == nil {
		t.Fatal("expected an empty table name to be rejected")
	}
}

func TestNilTableFilterSelectsEverything(t *testing.T) {
	filter, err := newTableFilter(nil, nil)
	if err != nil {
		t.Fatalf("build empty filter: %v", err)
	}
	if filter != nil {
		t.Fatalf("expected no filter for empty selections, got %#v", filter)
	}
	if filter.active() {
		t.Fatal("nil filter must report inactive")
	}
	if !filter.selects("anything") {
		t.Fatal("nil filter must select every table")
	}
	tables, err := filter.apply([]string{"users", "orders"})
	if err != nil {
		t.Fatalf("apply nil filter: %v", err)
	}
	if !reflect.DeepEqual(tables, []string{"users", "orders"}) {
		t.Fatalf("nil filter changed the table list: %#v", tables)
	}
}

func TestIncludeFilterSelectsOnlyRequestedTables(t *testing.T) {
	filter, err := newTableFilter([]string{"Users", " orders "}, nil)
	if err != nil {
		t.Fatalf("build include filter: %v", err)
	}
	tables, err := filter.apply([]string{"audit_log", "orders", "users"})
	if err != nil {
		t.Fatalf("apply include filter: %v", err)
	}
	if !reflect.DeepEqual(tables, []string{"orders", "users"}) {
		t.Fatalf("unexpected selection: %#v", tables)
	}
}

func TestExcludeFilterRemovesRequestedTables(t *testing.T) {
	filter, err := newTableFilter(nil, []string{"AUDIT_LOG"})
	if err != nil {
		t.Fatalf("build exclude filter: %v", err)
	}
	tables, err := filter.apply([]string{"audit_log", "orders", "users"})
	if err != nil {
		t.Fatalf("apply exclude filter: %v", err)
	}
	if !reflect.DeepEqual(tables, []string{"orders", "users"}) {
		t.Fatalf("unexpected selection: %#v", tables)
	}
}

func TestFilterFailsClosedOnUnknownTables(t *testing.T) {
	filter, err := newTableFilter([]string{"users", "missing_table"}, nil)
	if err != nil {
		t.Fatalf("build include filter: %v", err)
	}
	_, err = filter.apply([]string{"users", "orders"})
	if err == nil {
		t.Fatal("expected unknown selected table to fail")
	}
	if !strings.Contains(err.Error(), "missing_table") {
		t.Fatalf("error should name the missing table: %v", err)
	}
}

func TestFilterFailsWhenNothingRemains(t *testing.T) {
	filter, err := newTableFilter(nil, []string{"users"})
	if err != nil {
		t.Fatalf("build exclude filter: %v", err)
	}
	if _, err := filter.apply([]string{"users"}); err == nil {
		t.Fatal("expected an empty selection to fail")
	}
}

func TestWithLoadMethodValidation(t *testing.T) {
	migrator := &Migrator{}
	if err := WithLoadMethod(LoadMethodInsert)(migrator); err != nil {
		t.Fatalf("insert load method rejected: %v", err)
	}
	if migrator.loadMethod != LoadMethodInsert {
		t.Fatalf("load method not applied: %q", migrator.loadMethod)
	}
	if err := WithLoadMethod("bulk")(migrator); err == nil {
		t.Fatal("expected unsupported load method to be rejected")
	}
}

func TestWithTableFilterOption(t *testing.T) {
	migrator := &Migrator{}
	if err := WithTableFilter([]string{"users"}, []string{"orders"})(migrator); err == nil {
		t.Fatal("expected combined include/exclude to be rejected")
	}
	if err := WithTableFilter(nil, nil)(migrator); err != nil {
		t.Fatalf("empty filter rejected: %v", err)
	}
	if migrator.filter.active() {
		t.Fatal("empty selections must leave the filter inactive")
	}
}

func TestWithWorkersValidation(t *testing.T) {
	migrator := &Migrator{}
	if err := WithWorkers(4)(migrator); err != nil {
		t.Fatalf("4 workers rejected: %v", err)
	}
	if migrator.workers != 4 {
		t.Fatalf("workers not applied: %d", migrator.workers)
	}
	if err := WithWorkers(0)(migrator); err == nil {
		t.Fatal("expected zero workers to be rejected")
	}
	if err := WithWorkers(maxWorkers + 1)(migrator); err == nil {
		t.Fatal("expected out-of-range workers to be rejected")
	}
	if err := WithSkipSnapshotLock(true)(migrator); err != nil {
		t.Fatalf("skip snapshot lock rejected: %v", err)
	}
	if !migrator.skipSnapLock {
		t.Fatal("skip snapshot lock not applied")
	}
}
