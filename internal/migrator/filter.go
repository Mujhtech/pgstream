package migrator

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// tableFilter restricts a migration to an explicit subset of source tables.
// Matching is case-insensitive to mirror the identifier comparisons used
// elsewhere in the migrator.
type tableFilter struct {
	include map[string]struct{}
	exclude map[string]struct{}
}

func newTableFilter(include, exclude []string) (*tableFilter, error) {
	if len(include) > 0 && len(exclude) > 0 {
		return nil, fmt.Errorf("include and exclude table selections cannot be combined")
	}
	if len(include) == 0 && len(exclude) == 0 {
		return nil, nil
	}

	buildSet := func(names []string) (map[string]struct{}, error) {
		set := make(map[string]struct{}, len(names))
		for _, name := range names {
			trimmed := strings.TrimSpace(name)
			if trimmed == "" {
				return nil, fmt.Errorf("table selection contains an empty table name")
			}
			set[strings.ToLower(trimmed)] = struct{}{}
		}
		return set, nil
	}

	filter := &tableFilter{}
	var err error
	if len(include) > 0 {
		if filter.include, err = buildSet(include); err != nil {
			return nil, err
		}
	}
	if len(exclude) > 0 {
		if filter.exclude, err = buildSet(exclude); err != nil {
			return nil, err
		}
	}
	return filter, nil
}

func (f *tableFilter) active() bool {
	return f != nil
}

func (f *tableFilter) selects(table string) bool {
	if f == nil {
		return true
	}
	key := strings.ToLower(table)
	if len(f.include) > 0 {
		_, selected := f.include[key]
		return selected
	}
	_, excluded := f.exclude[key]
	return !excluded
}

// apply reduces the source table list to the selected subset. Every name in
// the selection must exist in the source database so a typo cannot silently
// migrate the wrong subset.
func (f *tableFilter) apply(tables []string) ([]string, error) {
	if f == nil {
		return tables, nil
	}

	known := make(map[string]struct{}, len(tables))
	for _, table := range tables {
		known[strings.ToLower(table)] = struct{}{}
	}

	requested := f.include
	if len(requested) == 0 {
		requested = f.exclude
	}
	var missing []string
	for name := range requested {
		if _, exists := known[name]; !exists {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return nil, fmt.Errorf("selected tables do not exist in the source database: %s", strings.Join(missing, ", "))
	}

	selected := make([]string, 0, len(tables))
	for _, table := range tables {
		if f.selects(table) {
			selected = append(selected, table)
		}
	}
	if len(selected) == 0 {
		return nil, fmt.Errorf("table selection excludes every source table; nothing to migrate")
	}
	return selected, nil
}

// validateForeignKeyClosure fails when a selected table has a foreign key
// referencing a table outside the selection. Creating such a constraint would
// either fail or bind to a stale copy of the referenced table, so the
// migration stops with an actionable error instead.
func (m *Migrator) validateForeignKeyClosure(ctx context.Context, tables []string) error {
	selected := make(map[string]struct{}, len(tables))
	for _, table := range tables {
		selected[strings.ToLower(table)] = struct{}{}
	}

	// Foreign keys whose referenced table is missing from the source database
	// entirely (partial backups) are skipped during migration, so they do not
	// constrain the selection.
	rows, err := m.sourceQueryer(ctx).QueryContext(ctx, `
		SELECT DISTINCT key_info.TABLE_NAME, key_info.CONSTRAINT_NAME, key_info.REFERENCED_TABLE_NAME
		FROM INFORMATION_SCHEMA.KEY_COLUMN_USAGE key_info
		WHERE key_info.TABLE_SCHEMA = DATABASE()
		AND key_info.REFERENCED_TABLE_NAME IS NOT NULL
		AND EXISTS (
			SELECT 1 FROM INFORMATION_SCHEMA.TABLES table_info
			WHERE table_info.TABLE_SCHEMA = DATABASE()
			AND table_info.TABLE_NAME = key_info.REFERENCED_TABLE_NAME
			AND table_info.TABLE_TYPE = 'BASE TABLE'
		)
		ORDER BY key_info.TABLE_NAME, key_info.CONSTRAINT_NAME
	`)
	if err != nil {
		return fmt.Errorf("inspect foreign keys for table selection: %w", err)
	}
	defer rows.Close()

	var violations []string
	for rows.Next() {
		var tableName, constraintName, referencedTable string
		if err := rows.Scan(&tableName, &constraintName, &referencedTable); err != nil {
			return fmt.Errorf("scan foreign key metadata: %w", err)
		}
		if _, tableSelected := selected[strings.ToLower(tableName)]; !tableSelected {
			continue
		}
		if _, referencedSelected := selected[strings.ToLower(referencedTable)]; !referencedSelected {
			violations = append(violations, fmt.Sprintf("%s (%s) references %s", tableName, constraintName, referencedTable))
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate foreign key metadata: %w", err)
	}

	if len(violations) > 0 {
		return fmt.Errorf(
			"table selection breaks foreign key dependencies: %s; include the referenced tables or exclude the dependent tables",
			strings.Join(violations, "; "),
		)
	}
	return nil
}
