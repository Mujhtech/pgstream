package migrator

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// unsignedBigintCheck holds the group-wise decisions for MySQL BIGINT
// UNSIGNED columns. The lossless PostgreSQL mapping is NUMERIC(20) (the type
// spans 0..2^64-1), but numeric cannot back an identity sequence and cannot
// join a foreign key against integer columns, so real schemas — where
// unsigned bigint keys never approach 2^63 — are better served by BIGINT.
// The decision is data-driven, like UUID conversion: a column (and every
// column its foreign keys connect it to) demotes to BIGINT only when a scan
// proves the data fits the signed range.
type unsignedBigintCheck struct {
	resolved bool
	// demote maps "table\x00column" (lower) -> true when the column becomes
	// BIGINT instead of NUMERIC(20).
	demote map[string]bool
	// overflowMax records, per kept-as-numeric column, the largest observed
	// value for actionable reporting.
	overflowMax map[string]string
}

func unsignedKey(table, column string) string {
	return strings.ToLower(table) + "\x00" + strings.ToLower(column)
}

// isUnsignedBigintType reports MySQL's bigint unsigned in both spellings
// ("bigint(20) unsigned" pre-8.0.19, "bigint unsigned" after display widths
// were dropped).
func isUnsignedBigintType(mysqlType string) bool {
	lower := strings.ToLower(mysqlType)
	return strings.HasPrefix(lower, "bigint") && strings.Contains(lower, "unsigned")
}

// mapsToPostgresIntegerFamily reports MySQL integer types whose built-in
// mapping lands in PostgreSQL's integer operator family (smallint, integer,
// bigint). A foreign key between one of these and a NUMERIC column cannot be
// created, so an edge to such a column anchors its group to BIGINT.
func mapsToPostgresIntegerFamily(mysqlType string) bool {
	lower := strings.ToLower(mysqlType)
	if !strings.Contains(lower, "int") {
		return false
	}
	// bigint unsigned is the undecided case itself.
	return !isUnsignedBigintType(lower)
}

// demoted reports whether resolveUnsignedBigintConversions decided this
// column maps to BIGINT. False (the lossless NUMERIC(20) mapping) when the
// resolver has not run.
func (m *Migrator) demotedUnsignedBigint(table, column string) bool {
	return m.unsignedCheck.demote[unsignedKey(table, column)]
}

// resolveUnsignedBigintConversions decides, before any table is created,
// which BIGINT UNSIGNED columns can map to BIGINT. Columns are grouped by
// foreign-key connectivity (union-find) so both ends of every constraint
// land on the same type; a group demotes only when MAX() proves every
// member's data fits the signed 63-bit range. Groups linked by foreign key
// to a column that maps to a PostgreSQL integer type must demote — if their
// data does not fit, the constraint is impossible and the run stops with
// guidance rather than failing at the foreign-key stage after the copy.
func (m *Migrator) resolveUnsignedBigintConversions(ctx context.Context, tables []string) error {
	selected := make(map[string]bool, len(tables))
	for _, table := range tables {
		selected[strings.ToLower(table)] = true
	}

	type candidate struct {
		table, column string
	}
	var candidates []candidate
	rows, err := m.sourceQueryer(ctx).QueryContext(ctx, `
		SELECT TABLE_NAME, COLUMN_NAME, COLUMN_TYPE
		FROM INFORMATION_SCHEMA.COLUMNS
		WHERE TABLE_SCHEMA = DATABASE()
	`)
	if err != nil {
		return fmt.Errorf("list unsigned bigint candidate columns: %w", err)
	}
	for rows.Next() {
		var table, column, columnType string
		if err := rows.Scan(&table, &column, &columnType); err != nil {
			_ = rows.Close()
			return err
		}
		if selected[strings.ToLower(table)] && isUnsignedBigintType(columnType) && m.casts.lookup(table, column, columnType) == "" {
			candidates = append(candidates, candidate{table: table, column: column})
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	_ = rows.Close()

	m.unsignedCheck.resolved = true
	m.unsignedCheck.demote = make(map[string]bool, len(candidates))
	m.unsignedCheck.overflowMax = make(map[string]string)
	if len(candidates) == 0 {
		return nil
	}

	// Union-find over foreign-key edges; edges to integer-family columns
	// anchor the group to BIGINT.
	parent := make(map[string]string, len(candidates))
	var find func(string) string
	find = func(key string) string {
		if parent[key] != key {
			parent[key] = find(parent[key])
		}
		return parent[key]
	}
	union := func(a, b string) { parent[find(a)] = find(b) }
	isCandidate := make(map[string]candidate, len(candidates))
	for _, cand := range candidates {
		key := unsignedKey(cand.table, cand.column)
		parent[key] = key
		isCandidate[key] = cand
	}

	anchored := make(map[string]string) // candidate key -> the integer-family peer, for messages
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
		return fmt.Errorf("list unsigned bigint foreign-key edges: %w", err)
	}
	for rows.Next() {
		var fromTable, fromColumn, toTable, toColumn, fromType, toType string
		if err := rows.Scan(&fromTable, &fromColumn, &toTable, &toColumn, &fromType, &toType); err != nil {
			_ = rows.Close()
			return err
		}
		if !selected[strings.ToLower(fromTable)] || !selected[strings.ToLower(toTable)] {
			continue
		}
		fromKey, toKey := unsignedKey(fromTable, fromColumn), unsignedKey(toTable, toColumn)
		_, fromIsCandidate := isCandidate[fromKey]
		_, toIsCandidate := isCandidate[toKey]
		switch {
		case fromIsCandidate && toIsCandidate:
			union(fromKey, toKey)
		case fromIsCandidate && mapsToPostgresIntegerFamily(toType):
			anchored[fromKey] = fmt.Sprintf("%s.%s (%s)", toTable, toColumn, toType)
		case toIsCandidate && mapsToPostgresIntegerFamily(fromType):
			anchored[toKey] = fmt.Sprintf("%s.%s (%s)", fromTable, fromColumn, fromType)
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	_ = rows.Close()

	// One MAX() scan per table covers every candidate column in it; foreign
	// keys require indexes in MySQL, so key columns resolve instantly.
	maxValues := make(map[string]string, len(candidates))
	byTable := make(map[string][]candidate)
	for _, cand := range candidates {
		byTable[cand.table] = append(byTable[cand.table], cand)
	}
	for table, tableCandidates := range byTable {
		selects := make([]string, len(tableCandidates))
		for i, cand := range tableCandidates {
			selects[i] = fmt.Sprintf("MAX(%s)", quoteMySQLIdentifier(cand.column))
		}
		query := fmt.Sprintf("SELECT %s FROM %s", strings.Join(selects, ", "), quoteMySQLIdentifier(table))
		values := make([]*string, len(tableCandidates))
		dests := make([]any, len(tableCandidates))
		for i := range values {
			dests[i] = &values[i]
		}
		if err := m.sourceQueryer(ctx).QueryRowContext(ctx, query).Scan(dests...); err != nil {
			return fmt.Errorf("scan unsigned bigint range for table %s: %w", table, err)
		}
		for i, cand := range tableCandidates {
			if values[i] != nil {
				maxValues[unsignedKey(cand.table, cand.column)] = *values[i]
			}
		}
	}

	// Group verdicts: fits -> BIGINT; overflows -> NUMERIC(20), unless the
	// group is anchored to an integer column, which is unbuildable.
	groupFits := make(map[string]bool)
	groupMax := make(map[string]string)
	groupAnchor := make(map[string]string)
	for key := range parent {
		root := find(key)
		if _, seen := groupFits[root]; !seen {
			groupFits[root] = true
		}
		if maxValue, ok := maxValues[key]; ok {
			if parsed, err := strconv.ParseUint(maxValue, 10, 64); err != nil || parsed > math.MaxInt64 {
				groupFits[root] = false
				if current, ok := groupMax[root]; !ok || len(maxValue) > len(current) || (len(maxValue) == len(current) && maxValue > current) {
					groupMax[root] = maxValue
				}
			}
		}
		if peer, ok := anchored[key]; ok {
			groupAnchor[root] = peer
		}
	}

	var kept []string
	for key, cand := range isCandidate {
		root := find(key)
		if groupFits[root] {
			m.unsignedCheck.demote[key] = true
			continue
		}
		if peer, isAnchored := groupAnchor[root]; isAnchored {
			return fmt.Errorf(
				"column %s.%s is BIGINT UNSIGNED with values above the signed 63-bit range (max %s) and is foreign-key-linked to %s, which maps to a PostgreSQL integer type; PostgreSQL cannot create a foreign key between numeric and integer columns. Fix the out-of-range values or override both columns with --cast",
				cand.table, cand.column, groupMax[root], peer)
		}
		m.unsignedCheck.overflowMax[key] = groupMax[root]
		kept = append(kept, fmt.Sprintf("%s.%s", cand.table, cand.column))
	}
	if len(kept) > 0 {
		m.warnf("⚠️  %d BIGINT UNSIGNED column(s) hold values above the signed 63-bit range and keep the lossless NUMERIC(20) mapping: %s", len(kept), strings.Join(kept, ", "))
	}
	return nil
}
