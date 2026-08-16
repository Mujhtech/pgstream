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

type uuidDecision struct {
	convert bool
	reason  string
}

type uuidDataCheck struct {
	mu     sync.Mutex
	counts map[string]int64 // "table\x00column" -> rows that cannot convert
	warned map[string]bool  // demotion warned once per column
	// decisions holds group-wise conversion outcomes computed by
	// resolveUUIDConversions; when present it overrides per-table checks.
	decisions map[string]uuidDecision
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

	// Group-wise decisions from resolveUUIDConversions are authoritative:
	// they account for every foreign-key-connected column, not just this
	// table's data.
	m.uuidCheck.mu.Lock()
	decisions := m.uuidCheck.decisions
	m.uuidCheck.mu.Unlock()
	if decisions != nil {
		for i := range columns {
			col := &columns[i]
			if !col.IsUUID {
				continue
			}
			decision, known := decisions[uuidCacheKey(table, col.Name)]
			if known && decision.convert {
				continue
			}
			col.IsUUID = false
			if known {
				col.UUIDDemotionReason = decision.reason
			} else {
				col.UUIDDemotionReason = "column was not part of the resolved conversion set; keeping the original type"
			}
			m.warnUUIDDemotion(table, col.Name, col.UUIDDemotionReason)
		}
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

// resolveUUIDConversions decides UUID conversion for every candidate column
// across the selected tables as foreign-key-connected groups: two columns
// joined by a foreign key must share one type, so a group converts to native
// UUID only when every member's data converts. A single dirty column keeps
// its whole key group on VARCHAR, which is lossless and keeps constraints
// creatable.
func (m *Migrator) resolveUUIDConversions(ctx context.Context, tables []string) error {
	selected := make(map[string]bool, len(tables))
	for _, table := range tables {
		selected[strings.ToLower(table)] = true
	}

	// Candidate columns: UUID storage shape, and either a primary key or
	// referencing a UUID-storage column (the same rule the per-column
	// heuristic uses).
	type candidate struct {
		table, column, columnType string
		primary                   bool
	}
	var candidates []candidate
	rows, err := m.sourceQueryer(ctx).QueryContext(ctx, `
		SELECT TABLE_NAME, COLUMN_NAME, COLUMN_TYPE, COLUMN_KEY
		FROM INFORMATION_SCHEMA.COLUMNS
		WHERE TABLE_SCHEMA = DATABASE()
	`)
	if err != nil {
		return fmt.Errorf("list UUID candidate columns: %w", err)
	}
	for rows.Next() {
		var table, column, columnType, columnKey string
		if err := rows.Scan(&table, &column, &columnType, &columnKey); err != nil {
			_ = rows.Close()
			return err
		}
		if selected[strings.ToLower(table)] && isUUIDStorageType(columnType) && m.casts.lookup(table, column, columnType) == "" {
			candidates = append(candidates, candidate{table: table, column: column, columnType: columnType, primary: columnKey == "PRI"})
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	_ = rows.Close()

	// Foreign-key edges between UUID-storage columns of selected tables.
	type edge struct{ from, to string }
	var edges []edge
	rows, err = m.sourceQueryer(ctx).QueryContext(ctx, `
		SELECT key_info.TABLE_NAME, key_info.COLUMN_NAME,
			key_info.REFERENCED_TABLE_NAME, key_info.REFERENCED_COLUMN_NAME,
			referencing_column.COLUMN_TYPE, referenced_column.COLUMN_TYPE
		FROM INFORMATION_SCHEMA.KEY_COLUMN_USAGE key_info
		JOIN INFORMATION_SCHEMA.COLUMNS referencing_column
			ON referencing_column.TABLE_SCHEMA = key_info.TABLE_SCHEMA
			AND referencing_column.TABLE_NAME = key_info.TABLE_NAME
			AND referencing_column.COLUMN_NAME = key_info.COLUMN_NAME
		JOIN INFORMATION_SCHEMA.COLUMNS referenced_column
			ON referenced_column.TABLE_SCHEMA = key_info.REFERENCED_TABLE_SCHEMA
			AND referenced_column.TABLE_NAME = key_info.REFERENCED_TABLE_NAME
			AND referenced_column.COLUMN_NAME = key_info.REFERENCED_COLUMN_NAME
		WHERE key_info.TABLE_SCHEMA = DATABASE()
		AND key_info.REFERENCED_TABLE_NAME IS NOT NULL
	`)
	if err != nil {
		return fmt.Errorf("list UUID foreign-key edges: %w", err)
	}
	referencesUUID := make(map[string]bool)
	for rows.Next() {
		var fromTable, fromColumn, toTable, toColumn, fromType, toType string
		if err := rows.Scan(&fromTable, &fromColumn, &toTable, &toColumn, &fromType, &toType); err != nil {
			_ = rows.Close()
			return err
		}
		if !selected[strings.ToLower(fromTable)] || !selected[strings.ToLower(toTable)] {
			continue
		}
		if !isUUIDStorageType(fromType) || !isUUIDStorageType(toType) {
			continue
		}
		if m.casts.lookup(fromTable, fromColumn, fromType) != "" || m.casts.lookup(toTable, toColumn, toType) != "" {
			continue
		}
		referencesUUID[uuidCacheKey(fromTable, fromColumn)] = true
		edges = append(edges, edge{from: uuidCacheKey(fromTable, fromColumn), to: uuidCacheKey(toTable, toColumn)})
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	_ = rows.Close()

	// Keep only real candidates (primary keys or FK columns referencing
	// UUID-storage keys) as group members.
	members := make(map[string]candidate)
	for _, cand := range candidates {
		key := uuidCacheKey(cand.table, cand.column)
		if cand.primary || referencesUUID[key] {
			members[key] = cand
		}
	}
	if len(members) == 0 {
		return nil
	}

	// Union-find over the FK edges.
	parent := make(map[string]string, len(members))
	var find func(string) string
	find = func(node string) string {
		if parent[node] != node {
			parent[node] = find(parent[node])
		}
		return parent[node]
	}
	for key := range members {
		parent[key] = key
	}
	for _, e := range edges {
		if _, ok := members[e.from]; !ok {
			continue
		}
		if _, ok := members[e.to]; !ok {
			continue
		}
		parent[find(e.from)] = find(e.to)
	}

	// Data-check every member, batched per table.
	byTable := make(map[string][]ColumnInfo)
	for _, cand := range members {
		byTable[cand.table] = append(byTable[cand.table], ColumnInfo{Name: cand.column, Type: cand.columnType})
	}
	invalid := make(map[string]int64, len(members))
	for table, cols := range byTable {
		counts, err := m.uuidInvalidCounts(ctx, table, cols)
		if err != nil {
			return err
		}
		for column, count := range counts {
			invalid[uuidCacheKey(table, column)] = count
		}
	}

	// A group converts only when every member is clean; otherwise every
	// member keeps VARCHAR with a reason naming the dirty column.
	dirtyByRoot := make(map[string]string)
	for key, cand := range members {
		if invalid[key] > 0 {
			root := find(key)
			if _, exists := dirtyByRoot[root]; !exists {
				dirtyByRoot[root] = fmt.Sprintf("%s.%s", cand.table, cand.column)
			}
		}
	}

	decisions := make(map[string]uuidDecision, len(members))
	for key, cand := range members {
		root := find(key)
		dirty, groupDirty := dirtyByRoot[root]
		switch {
		case !groupDirty:
			decisions[key] = uuidDecision{convert: true}
		case invalid[key] > 0:
			decisions[key] = uuidDecision{convert: false, reason: fmt.Sprintf(
				"%d rows contain values that are not canonical UUIDs (inspect with: SELECT %s FROM %s WHERE %s IS NOT NULL AND %s NOT REGEXP '%s' LIMIT 5); keeping the original type",
				invalid[key], quoteMySQLIdentifier(cand.column), quoteMySQLIdentifier(cand.table), quoteMySQLIdentifier(cand.column), quoteMySQLIdentifier(cand.column), uuidPattern,
			)}
		default:
			decisions[key] = uuidDecision{convert: false, reason: fmt.Sprintf(
				"foreign-key-connected column %s contains non-UUID values, so the whole key group keeps its original type to stay constraint-compatible", dirty,
			)}
		}
	}

	m.uuidCheck.mu.Lock()
	if m.uuidCheck.counts == nil {
		m.uuidCheck.counts = make(map[string]int64)
		m.uuidCheck.warned = make(map[string]bool)
	}
	m.uuidCheck.decisions = decisions
	m.uuidCheck.mu.Unlock()
	return nil
}

// verifyExistingUUIDDecisions compares a pre-existing target table's
// uuid/varchar columns against this run's resolved conversion decisions and
// fails with exact reconciliation DDL when they diverge — before any data
// copies, instead of at foreign-key creation after it.
func (m *Migrator) verifyExistingUUIDDecisions(ctx context.Context, table string) error {
	columns, err := m.getMySQLTableStructure(ctx, table)
	if err != nil {
		return fmt.Errorf("read source structure for existing table %s: %w", table, err)
	}
	metadata, err := m.loadValidationMetadata(ctx, table)
	if err != nil {
		return fmt.Errorf("read target structure for existing table %s: %w", table, err)
	}

	var problems []string
	for _, col := range columns {
		info, exists := metadata.columns[strings.ToLower(col.Name)]
		if !exists {
			continue
		}
		actualUUID := info.udtName == "uuid"
		if col.IsUUID == actualUUID {
			continue
		}
		if !col.IsUUID && !isUUIDStorageType(col.Type) {
			// Not a UUID-shaped column at all; other drift is out of scope.
			continue
		}

		quotedTarget := fmt.Sprintf("%s.%s", quotePostgresIdentifier(m.schemaName), quotePostgresIdentifier(table))
		quotedColumn := quotePostgresIdentifier(col.Name)
		lowerType := strings.ToLower(col.Type)
		switch {
		case actualUUID && (strings.HasPrefix(lowerType, "char(36)") || strings.HasPrefix(lowerType, "varchar(36)")):
			problems = append(problems, fmt.Sprintf(
				"%s is uuid in the target but this run keeps it varchar (its key group contains non-UUID data); reconcile with: ALTER TABLE %s ALTER COLUMN %s TYPE varchar(36) USING %s::text",
				col.Name, quotedTarget, quotedColumn, quotedColumn))
		case !actualUUID && col.IsUUID:
			problems = append(problems, fmt.Sprintf(
				"%s is %s in the target but this run converts it to native uuid (all data verified convertible); reconcile with: ALTER TABLE %s ALTER COLUMN %s TYPE uuid USING %s::uuid",
				col.Name, info.udtName, quotedTarget, quotedColumn, quotedColumn))
		default:
			problems = append(problems, fmt.Sprintf(
				"%s diverges from this run's decision (target %s, source %s); drop and recreate the table or convert the column manually",
				col.Name, info.udtName, col.Type))
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf(
			"existing target table %s.%s was created with different UUID type decisions than this run resolved: %s",
			m.schemaName, table, strings.Join(problems, "; "))
	}
	return nil
}
