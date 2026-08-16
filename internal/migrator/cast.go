package migrator

import (
	"fmt"
	"regexp"
	"strings"
)

// castRules holds user-supplied type-mapping overrides, inspired by
// pgloader's CAST clauses. Two rule forms exist:
//
//	table.column=PGTYPE   column override, highest precedence
//	mysqltype=PGTYPE      type override, prefix-matched in rule order
//
// A cast is authoritative: it replaces the built-in mapping and exempts the
// column from native-UUID conversion.
type castRules struct {
	columns map[string]string // "table\x00column" (lower) -> target type
	types   []typeCastRule
}

type typeCastRule struct {
	prefix string // lower-case MySQL type prefix
	target string
}

// castTargetPattern keeps targets to plain type syntax: identifiers,
// precision arguments, and array brackets.
var castTargetPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_ ]*(\([0-9, ]+\))?(\[\])?$`)

func parseCastRules(rules []string) (*castRules, error) {
	if len(rules) == 0 {
		return nil, nil
	}
	parsed := &castRules{columns: make(map[string]string)}
	for _, rule := range rules {
		left, target, found := strings.Cut(rule, "=")
		left = strings.TrimSpace(left)
		target = strings.TrimSpace(target)
		if !found || left == "" || target == "" {
			return nil, fmt.Errorf("cast rule %q must have the form table.column=TYPE or mysqltype=TYPE", rule)
		}
		if !castTargetPattern.MatchString(target) {
			return nil, fmt.Errorf("cast rule %q has an invalid target type %q", rule, target)
		}

		if table, column, isColumn := strings.Cut(left, "."); isColumn && !strings.Contains(left, "(") {
			table, column = strings.TrimSpace(table), strings.TrimSpace(column)
			if table == "" || column == "" {
				return nil, fmt.Errorf("cast rule %q has an empty table or column name", rule)
			}
			parsed.columns[strings.ToLower(table)+"\x00"+strings.ToLower(column)] = target
			continue
		}
		parsed.types = append(parsed.types, typeCastRule{prefix: strings.ToLower(left), target: target})
	}
	return parsed, nil
}

// lookup returns the cast target for a column, or "" when no rule applies.
// Column rules win over type rules; type rules match in declaration order.
func (c *castRules) lookup(table, column, mysqlType string) string {
	if c == nil {
		return ""
	}
	if target, ok := c.columns[strings.ToLower(table)+"\x00"+strings.ToLower(column)]; ok {
		return target
	}
	lower := strings.ToLower(strings.TrimSpace(mysqlType))
	for _, rule := range c.types {
		if strings.HasPrefix(lower, rule.prefix) {
			return rule.target
		}
	}
	return ""
}

// WithCastRules overrides built-in type mappings, in the spirit of
// pgloader's CAST clauses. Rules: "table.column=TYPE" (column-level, wins)
// or "mysqltype=TYPE" (prefix-matched against the MySQL column type, first
// rule wins). Cast columns are exempt from native-UUID conversion.
func WithCastRules(rules []string) Option {
	return func(migrator *Migrator) error {
		parsed, err := parseCastRules(rules)
		if err != nil {
			return err
		}
		migrator.casts = parsed
		return nil
	}
}
