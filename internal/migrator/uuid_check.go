package migrator

import (
	"context"
	"fmt"
	"strings"
	"sync"
)

// uuidPattern matches the canonical dashed UUID form. Values in any other
// shape keep their original VARCHAR type: converting is an optimization,
// storing the data losslessly is the requirement.
const uuidPattern = "^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$"

type uuidDataCheck struct {
	mu     sync.Mutex
	counts map[string]int64 // "table\x00column" -> rows that cannot convert
	warned map[string]bool  // demotion warned once per column
}

func uuidCacheKey(table, column string) string {
	return table + "\x00" + column
}

// uuidInvalidPredicate returns the SQL expression counting rows of the given
// MySQL type that cannot convert to a PostgreSQL UUID.
func uuidInvalidPredicate(column string, mysqlType string) string {
	quoted := quoteMySQLIdentifier(column)
	lower := strings.ToLower(mysqlType)
	if strings.HasPrefix(lower, "binary(16)") || strings.HasPrefix(lower, "varbinary(16)") {
		// Any 16-byte value converts; shorter varbinary values cannot.
		return fmt.Sprintf("COALESCE(SUM(%s IS NOT NULL AND LENGTH(%s) <> 16), 0)", quoted, quoted)
	}
	return fmt.Sprintf("COALESCE(SUM(%s IS NOT NULL AND %s NOT REGEXP '%s'), 0)", quoted, quoted, uuidPattern)
}

// uuidInvalidCounts counts, in one table scan, the rows of each candidate
// column that cannot convert to a native UUID. Results are cached: many
// tables referencing the same primary key trigger a single scan.
func (m *Migrator) uuidInvalidCounts(ctx context.Context, table string, candidates []ColumnInfo) (map[string]int64, error) {
	result := make(map[string]int64, len(candidates))

	m.uuidCheck.mu.Lock()
	if m.uuidCheck.counts == nil {
		m.uuidCheck.counts = make(map[string]int64)
		m.uuidCheck.warned = make(map[string]bool)
	}
	var missing []ColumnInfo
	for _, candidate := range candidates {
		if count, cached := m.uuidCheck.counts[uuidCacheKey(table, candidate.Name)]; cached {
			result[candidate.Name] = count
		} else {
			missing = append(missing, candidate)
		}
	}
	m.uuidCheck.mu.Unlock()

	if len(missing) == 0 {
		return result, nil
	}

	selects := make([]string, len(missing))
	for i, candidate := range missing {
		selects[i] = uuidInvalidPredicate(candidate.Name, candidate.Type)
	}
	query := fmt.Sprintf("SELECT %s FROM %s", strings.Join(selects, ", "), quoteMySQLIdentifier(table))
	counts := make([]int64, len(missing))
	dests := make([]any, len(missing))
	for i := range counts {
		dests[i] = &counts[i]
	}
	if err := m.sourceQueryer(ctx).QueryRowContext(ctx, query).Scan(dests...); err != nil {
		return nil, fmt.Errorf("inspect UUID candidate columns of %s: %w", table, err)
	}

	m.uuidCheck.mu.Lock()
	for i, candidate := range missing {
		m.uuidCheck.counts[uuidCacheKey(table, candidate.Name)] = counts[i]
		result[candidate.Name] = counts[i]
	}
	m.uuidCheck.mu.Unlock()
	return result, nil
}

type uuidReference struct {
	table      string
	column     string
	columnType string
}

// uuidCandidateReferences maps each foreign-key column of table to the
// UUID-storage-typed columns it references.
func (m *Migrator) uuidCandidateReferences(ctx context.Context, table string) (map[string][]uuidReference, error) {
	rows, err := m.sourceQueryer(ctx).QueryContext(ctx, `
		SELECT key_info.COLUMN_NAME, key_info.REFERENCED_TABLE_NAME, key_info.REFERENCED_COLUMN_NAME, referenced_column.COLUMN_TYPE
		FROM INFORMATION_SCHEMA.KEY_COLUMN_USAGE key_info
		JOIN INFORMATION_SCHEMA.COLUMNS referenced_column
			ON referenced_column.TABLE_SCHEMA = key_info.REFERENCED_TABLE_SCHEMA
			AND referenced_column.TABLE_NAME = key_info.REFERENCED_TABLE_NAME
			AND referenced_column.COLUMN_NAME = key_info.REFERENCED_COLUMN_NAME
		WHERE key_info.TABLE_SCHEMA = DATABASE()
		AND key_info.TABLE_NAME = ?
		AND key_info.REFERENCED_TABLE_NAME IS NOT NULL
	`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	references := make(map[string][]uuidReference)
	for rows.Next() {
		var column, refTable, refColumn, refType string
		if err := rows.Scan(&column, &refTable, &refColumn, &refType); err != nil {
			return nil, err
		}
		if isUUIDStorageType(refType) {
			references[column] = append(references[column], uuidReference{table: refTable, column: refColumn, columnType: refType})
		}
	}
	return references, rows.Err()
}

// applyUUIDDataCheck demotes UUID-candidate columns whose data (or whose
// referenced column's data) cannot convert to native UUIDs. Demoted columns
// keep their original VARCHAR/BINARY mapping, which stores every value
// losslessly; referencing columns follow their referenced column so foreign
// key types stay compatible.
func (m *Migrator) applyUUIDDataCheck(ctx context.Context, table string, columns []ColumnInfo) error {
	var candidates []ColumnInfo
	for _, col := range columns {
		if col.IsUUID {
			candidates = append(candidates, col)
		}
	}
	if len(candidates) == 0 {
		return nil
	}

	ownCounts, err := m.uuidInvalidCounts(ctx, table, candidates)
	if err != nil {
		return err
	}
	references, err := m.uuidCandidateReferences(ctx, table)
	if err != nil {
		return fmt.Errorf("inspect UUID references of %s: %w", table, err)
	}

	for i := range columns {
		col := &columns[i]
		if !col.IsUUID {
			continue
		}

		if invalid := ownCounts[col.Name]; invalid > 0 {
			col.IsUUID = false
			col.UUIDDemotionReason = fmt.Sprintf(
				"%d rows contain values that are not canonical UUIDs (inspect with: SELECT %s FROM %s WHERE %s IS NOT NULL AND %s NOT REGEXP '%s' LIMIT 5); keeping the original type instead of native UUID",
				invalid, quoteMySQLIdentifier(col.Name), quoteMySQLIdentifier(table), quoteMySQLIdentifier(col.Name), quoteMySQLIdentifier(col.Name), uuidPattern,
			)
			m.warnUUIDDemotion(table, col.Name, col.UUIDDemotionReason)
			continue
		}

		// A referencing column only converts when every referenced column's
		// data also converts, so both FK endpoints end up with one type.
		for _, reference := range references[col.Name] {
			refCounts, err := m.uuidInvalidCounts(ctx, reference.table, []ColumnInfo{{Name: reference.column, Type: reference.columnType}})
			if err != nil {
				return err
			}
			if refCounts[reference.column] > 0 {
				col.IsUUID = false
				col.UUIDDemotionReason = fmt.Sprintf(
					"referenced column %s.%s contains non-UUID values, so this foreign key column keeps the matching original type",
					reference.table, reference.column,
				)
				m.warnUUIDDemotion(table, col.Name, col.UUIDDemotionReason)
				break
			}
		}
	}
	return nil
}

// warnUUIDDemotion logs a demotion once per column per run.
func (m *Migrator) warnUUIDDemotion(table, column, reason string) {
	key := uuidCacheKey(table, column)
	m.uuidCheck.mu.Lock()
	alreadyWarned := m.uuidCheck.warned[key]
	m.uuidCheck.warned[key] = true
	m.uuidCheck.mu.Unlock()
	if !alreadyWarned {
		m.warnf("⚠️  Column %s.%s looks like a UUID column by shape, but %s", table, column, reason)
	}
}
