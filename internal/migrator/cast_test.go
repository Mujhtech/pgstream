package migrator

import (
	"strings"
	"testing"
)

func TestParseCastRulesForms(t *testing.T) {
	rules, err := parseCastRules([]string{
		"jobs.id=text",
		"datetime=timestamptz",
		"tinyint(1)=smallint",
		"decimal=numeric(20, 4)",
	})
	if err != nil {
		t.Fatalf("parseCastRules: %v", err)
	}

	if got := rules.lookup("jobs", "id", "varchar(36)"); got != "text" {
		t.Fatalf("column rule: got %q, want %q", got, "text")
	}
	if got := rules.lookup("orders", "created_at", "datetime"); got != "timestamptz" {
		t.Fatalf("type rule: got %q, want %q", got, "timestamptz")
	}
	// Type rules match on prefix, so datetime(6) hits the datetime rule.
	if got := rules.lookup("orders", "updated_at", "datetime(6)"); got != "timestamptz" {
		t.Fatalf("prefix match: got %q, want %q", got, "timestamptz")
	}
	// tinyint(1) has a paren, so it parses as a type rule, not a column rule.
	if got := rules.lookup("flags", "status", "tinyint(1)"); got != "smallint" {
		t.Fatalf("parenthesized type rule: got %q, want %q", got, "smallint")
	}
	// Targets may carry precision arguments with commas.
	if got := rules.lookup("ledger", "amount", "decimal(18,2)"); got != "numeric(20, 4)" {
		t.Fatalf("target with precision: got %q, want %q", got, "numeric(20, 4)")
	}
	if got := rules.lookup("users", "name", "varchar(255)"); got != "" {
		t.Fatalf("unmatched column: got %q, want empty", got)
	}
}

func TestParseCastRulesColumnRuleWinsOverTypeRule(t *testing.T) {
	rules, err := parseCastRules([]string{
		"datetime=timestamptz",
		"audits.created_at=timestamp",
	})
	if err != nil {
		t.Fatalf("parseCastRules: %v", err)
	}
	if got := rules.lookup("audits", "created_at", "datetime"); got != "timestamp" {
		t.Fatalf("column precedence: got %q, want %q", got, "timestamp")
	}
	if got := rules.lookup("other", "created_at", "datetime"); got != "timestamptz" {
		t.Fatalf("type fallback: got %q, want %q", got, "timestamptz")
	}
}

func TestParseCastRulesFirstTypeRuleWins(t *testing.T) {
	rules, err := parseCastRules([]string{
		"tinyint(1)=smallint",
		"tinyint=integer",
	})
	if err != nil {
		t.Fatalf("parseCastRules: %v", err)
	}
	if got := rules.lookup("t", "c", "tinyint(1)"); got != "smallint" {
		t.Fatalf("first matching rule: got %q, want %q", got, "smallint")
	}
	if got := rules.lookup("t", "c", "tinyint(4)"); got != "integer" {
		t.Fatalf("second rule fallback: got %q, want %q", got, "integer")
	}
}

func TestParseCastRulesIsCaseInsensitive(t *testing.T) {
	rules, err := parseCastRules([]string{"Jobs.ID=text", "DATETIME=timestamptz"})
	if err != nil {
		t.Fatalf("parseCastRules: %v", err)
	}
	if got := rules.lookup("JOBS", "id", "varchar(36)"); got != "text" {
		t.Fatalf("column case fold: got %q, want %q", got, "text")
	}
	if got := rules.lookup("t", "c", "DateTime"); got != "timestamptz" {
		t.Fatalf("type case fold: got %q, want %q", got, "timestamptz")
	}
}

func TestParseCastRulesRejectsMalformedRules(t *testing.T) {
	for _, rule := range []string{
		"no-equals",
		"=text",
		"datetime=",
		"jobs.=text",
		".id=text",
		"jobs.id=text; DROP TABLE users",
		"datetime=timestamptz'--",
	} {
		if _, err := parseCastRules([]string{rule}); err == nil {
			t.Fatalf("rule %q: expected error, got none", rule)
		}
	}
}

func TestParseCastRulesEmptyInputDisablesCasts(t *testing.T) {
	rules, err := parseCastRules(nil)
	if err != nil {
		t.Fatalf("parseCastRules(nil): %v", err)
	}
	if rules != nil {
		t.Fatalf("expected nil rules for empty input")
	}
	// A nil receiver must behave as "no rule matched".
	if got := rules.lookup("t", "c", "datetime"); got != "" {
		t.Fatalf("nil lookup: got %q, want empty", got)
	}
}

func TestBuildColumnDefinitionHonorsCastType(t *testing.T) {
	m := &Migrator{schemaName: "app"}

	def, pgType, err := m.buildColumnDefinition("jobs", ColumnInfo{
		Name:     "id",
		Type:     "varchar(36)",
		CastType: "text",
		IsUUID:   true, // cast overrides even a UUID-shaped column
	})
	if err != nil {
		t.Fatalf("buildColumnDefinition: %v", err)
	}
	if pgType != "TEXT" {
		t.Fatalf("pgType: got %q, want %q", pgType, "TEXT")
	}
	if !strings.Contains(def, `"id" TEXT`) {
		t.Fatalf("definition %q does not use the cast type", def)
	}
}

func TestBuildColumnDefinitionCastTimestamptzKeepsCurrentTimestampDefault(t *testing.T) {
	m := &Migrator{schemaName: "app"}

	def, pgType, err := m.buildColumnDefinition("audits", ColumnInfo{
		Name:         "created_at",
		Type:         "datetime",
		CastType:     "timestamptz",
		HasDefault:   true,
		DefaultValue: "CURRENT_TIMESTAMP",
	})
	if err != nil {
		t.Fatalf("buildColumnDefinition: %v", err)
	}
	if pgType != "TIMESTAMPTZ" {
		t.Fatalf("pgType: got %q, want %q", pgType, "TIMESTAMPTZ")
	}
	if !strings.Contains(def, "DEFAULT CURRENT_TIMESTAMP") {
		t.Fatalf("definition %q lost the CURRENT_TIMESTAMP default", def)
	}
}
