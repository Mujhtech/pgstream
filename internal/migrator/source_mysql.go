package migrator

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

type mysqlQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// sourceQueryerKey carries a per-worker snapshot connection through the
// context so concurrently migrating tables each read from their own aligned
// consistent snapshot.
type sourceQueryerKey struct{}

func withSourceQueryer(ctx context.Context, queryer mysqlQueryer) context.Context {
	return context.WithValue(ctx, sourceQueryerKey{}, queryer)
}

func (m *Migrator) sourceQueryer(ctx context.Context) mysqlQueryer {
	if queryer, ok := ctx.Value(sourceQueryerKey{}).(mysqlQueryer); ok && queryer != nil {
		return queryer
	}
	if m.sourceTx != nil {
		return m.sourceTx
	}
	return m.mysql.GetDB()
}

func (m *Migrator) beginSourceSnapshot(ctx context.Context) error {
	if m.sourceTx != nil {
		return fmt.Errorf("source snapshot is already active")
	}
	tx, err := m.mysql.StartTx(ctx, &sql.TxOptions{
		Isolation: sql.LevelRepeatableRead,
		ReadOnly:  true,
	})
	if err != nil {
		return err
	}
	m.sourceTx = tx
	return nil
}

func (m *Migrator) commitSourceSnapshot() error {
	if m.sourceTx == nil {
		return nil
	}
	tx := m.sourceTx
	m.sourceTx = nil
	return tx.Commit()
}

func (m *Migrator) rollbackSourceSnapshot() {
	if m.sourceTx == nil {
		return
	}
	tx := m.sourceTx
	m.sourceTx = nil
	_ = tx.Rollback()
}

func (m *Migrator) getColumnNames(ctx context.Context, table string) ([]string, error) {
	rows, err := m.sourceQueryer(ctx).QueryContext(ctx, fmt.Sprintf("SHOW COLUMNS FROM %s", quoteMySQLIdentifier(table)))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var cols []string
	for rows.Next() {
		var name, colType, isNull, key, extra string
		var defaultValue sql.NullString
		if err := rows.Scan(&name, &colType, &isNull, &key, &defaultValue, &extra); err != nil {
			return nil, err
		}
		if err := validatePostgresIdentifier(name, "source column name"); err != nil {
			return nil, fmt.Errorf("table %s: %w", table, err)
		}
		cols = append(cols, name)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return cols, nil
}

func (m *Migrator) getMySQLPrimaryKeyColumns(ctx context.Context, table string) ([]string, error) {
	rows, err := m.sourceQueryer(ctx).QueryContext(ctx, `
		SELECT COLUMN_NAME
		FROM INFORMATION_SCHEMA.STATISTICS
		WHERE TABLE_SCHEMA = DATABASE()
		AND TABLE_NAME = ?
		AND INDEX_NAME = 'PRIMARY'
		ORDER BY SEQ_IN_INDEX
	`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var columns []string
	for rows.Next() {
		var column string
		if err := rows.Scan(&column); err != nil {
			return nil, err
		}
		columns = append(columns, column)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return columns, nil
}

func (m *Migrator) getMySQLAutoIncrementMetadata(ctx context.Context, table string) (string, sql.NullInt64, error) {
	var column string
	var nextValue sql.NullInt64
	err := m.sourceQueryer(ctx).QueryRowContext(ctx, `
		SELECT column_info.COLUMN_NAME, table_info.AUTO_INCREMENT
		FROM INFORMATION_SCHEMA.COLUMNS column_info
		JOIN INFORMATION_SCHEMA.TABLES table_info
			ON table_info.TABLE_SCHEMA = column_info.TABLE_SCHEMA
			AND table_info.TABLE_NAME = column_info.TABLE_NAME
		WHERE column_info.TABLE_SCHEMA = DATABASE()
		AND column_info.TABLE_NAME = ?
		AND LOWER(column_info.EXTRA) LIKE '%auto_increment%'
	`, table).Scan(&column, &nextValue)
	if errors.Is(err, sql.ErrNoRows) {
		return "", sql.NullInt64{}, nil
	}
	if err != nil {
		return "", sql.NullInt64{}, fmt.Errorf("read MySQL AUTO_INCREMENT metadata for %s: %w", table, err)
	}
	return column, nextValue, nil
}

// sourceEnumEmptyMarkerCounts counts rows holding MySQL's empty-string
// marker for invalid enum values (internal index 0, stored when an invalid
// value was inserted outside strict SQL mode or an ALTER removed a member).
// All requested columns are counted in one table scan instead of one full
// scan per column.
func (m *Migrator) sourceEnumEmptyMarkerCounts(ctx context.Context, table string, columns []string) (map[string]int64, error) {
	if len(columns) == 0 {
		return nil, nil
	}
	selects := make([]string, len(columns))
	for i, column := range columns {
		selects[i] = fmt.Sprintf("COALESCE(SUM(%s = ''), 0)", quoteMySQLIdentifier(column))
	}
	query := fmt.Sprintf("SELECT %s FROM %s", strings.Join(selects, ", "), quoteMySQLIdentifier(table))

	counts := make([]int64, len(columns))
	dests := make([]any, len(columns))
	for i := range counts {
		dests[i] = &counts[i]
	}
	if err := m.sourceQueryer(ctx).QueryRowContext(ctx, query).Scan(dests...); err != nil {
		return nil, err
	}

	result := make(map[string]int64, len(columns))
	for i, column := range columns {
		result[column] = counts[i]
	}
	return result, nil
}

// sourceZeroDateCounts counts, in one table scan, the rows of each temporal
// column that hold MySQL's zero date. CAST keeps the comparison valid under
// any session sql_mode.
func (m *Migrator) sourceZeroDateCounts(ctx context.Context, table string, columns []ColumnInfo) (map[string]int64, error) {
	var temporal []string
	for _, col := range columns {
		lower := strings.ToLower(col.Type)
		if strings.HasPrefix(lower, "date") || strings.HasPrefix(lower, "datetime") || strings.HasPrefix(lower, "timestamp") {
			temporal = append(temporal, col.Name)
		}
	}
	if len(temporal) == 0 {
		return nil, nil
	}

	selects := make([]string, len(temporal))
	for i, column := range temporal {
		quoted := quoteMySQLIdentifier(column)
		selects[i] = fmt.Sprintf("COALESCE(SUM(CAST(%s AS CHAR) LIKE '0000-00-00%%'), 0)", quoted)
	}
	query := fmt.Sprintf("SELECT %s FROM %s", strings.Join(selects, ", "), quoteMySQLIdentifier(table))

	counts := make([]int64, len(temporal))
	dests := make([]any, len(temporal))
	for i := range counts {
		dests[i] = &counts[i]
	}
	if err := m.sourceQueryer(ctx).QueryRowContext(ctx, query).Scan(dests...); err != nil {
		return nil, err
	}

	result := make(map[string]int64, len(temporal))
	for i, column := range temporal {
		result[column] = counts[i]
	}
	return result, nil
}

func (m *Migrator) extractEnumValues(mysqlEnumType string) []string {

	// Parse MySQL enum definitions without splitting commas inside quoted values.
	start := strings.Index(mysqlEnumType, "(")
	end := strings.LastIndex(mysqlEnumType, ")")
	if start == -1 || end <= start {
		return nil
	}

	input := mysqlEnumType[start+1 : end]
	values := make([]string, 0)
	for position := 0; position < len(input); {
		for position < len(input) && (input[position] == ' ' || input[position] == '\t' || input[position] == ',') {
			position++
		}
		if position >= len(input) {
			break
		}
		if input[position] != '\'' {
			return nil
		}
		position++
		var value strings.Builder
		closed := false
		for position < len(input) {
			character := input[position]
			position++
			if character == '\\' && position < len(input) {
				value.WriteByte(input[position])
				position++
				continue
			}
			if character == '\'' {
				if position < len(input) && input[position] == '\'' {
					value.WriteByte('\'')
					position++
					continue
				}
				closed = true
				break
			}
			value.WriteByte(character)
		}
		if !closed {
			return nil
		}
		values = append(values, value.String())
	}
	return values
}

func (m *Migrator) getMySQLTableStructure(ctx context.Context, table string) ([]ColumnInfo, error) {
	rows, err := m.sourceQueryer(ctx).QueryContext(ctx, `
		SELECT
			column_info.COLUMN_NAME,
			column_info.COLUMN_TYPE,
			column_info.IS_NULLABLE,
			column_info.COLUMN_DEFAULT,
			column_info.EXTRA,
			column_info.COLUMN_KEY,
			column_info.GENERATION_EXPRESSION,
			EXISTS(
				SELECT 1
				FROM INFORMATION_SCHEMA.KEY_COLUMN_USAGE key_info
				JOIN INFORMATION_SCHEMA.COLUMNS referenced_column
					ON referenced_column.TABLE_SCHEMA = key_info.REFERENCED_TABLE_SCHEMA
					AND referenced_column.TABLE_NAME = key_info.REFERENCED_TABLE_NAME
					AND referenced_column.COLUMN_NAME = key_info.REFERENCED_COLUMN_NAME
				WHERE key_info.TABLE_SCHEMA = column_info.TABLE_SCHEMA
				AND key_info.TABLE_NAME = column_info.TABLE_NAME
				AND key_info.COLUMN_NAME = column_info.COLUMN_NAME
				AND (
					LOWER(referenced_column.COLUMN_TYPE) LIKE 'char(36)%'
					OR LOWER(referenced_column.COLUMN_TYPE) LIKE 'varchar(36)%'
					OR LOWER(referenced_column.COLUMN_TYPE) LIKE 'binary(16)%'
					OR LOWER(referenced_column.COLUMN_TYPE) LIKE 'varbinary(16)%'
				)
			) AS REFERENCES_UUID
		FROM INFORMATION_SCHEMA.COLUMNS column_info
		WHERE column_info.TABLE_SCHEMA = DATABASE() AND column_info.TABLE_NAME = ?
		ORDER BY column_info.ORDINAL_POSITION
	`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var columns []ColumnInfo
	for rows.Next() {
		var column ColumnInfo
		var nullable, columnKey string
		var referencesUUID bool
		var defaultValue, generationExpression sql.NullString
		if err := rows.Scan(&column.Name, &column.Type, &nullable, &defaultValue, &column.Extra, &columnKey, &generationExpression, &referencesUUID); err != nil {
			return nil, err
		}
		column.Nullable = nullable == "YES"
		column.HasDefault = defaultValue.Valid
		column.DefaultValue = defaultValue.String
		column.GenerationExpression = generationExpression.String
		column.IsUUID = isUUIDStorageType(column.Type) && (columnKey == "PRI" || referencesUUID)
		if err := validatePostgresIdentifier(column.Name, "source column name"); err != nil {
			return nil, fmt.Errorf("table %s: %w", table, err)
		}
		columns = append(columns, column)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(columns) == 0 {
		return nil, fmt.Errorf("table %q has no columns or does not exist", table)
	}
	// User cast rules are authoritative: they replace the built-in mapping
	// and exempt the column from UUID conversion.
	for i := range columns {
		if target := m.casts.lookup(table, columns[i].Name, columns[i].Type); target != "" {
			columns[i].CastType = target
			columns[i].IsUUID = false
		}
	}

	// UUID conversion is a schema-shape heuristic; the data decides whether
	// it is actually safe. Columns whose values cannot convert keep their
	// original lossless type.
	if err := m.applyUUIDDataCheck(ctx, table, columns); err != nil {
		return nil, err
	}
	return columns, nil
}

type ColumnInfo struct {
	Name         string
	Type         string
	Nullable     bool
	HasDefault   bool
	DefaultValue string
	Extra        string
	IsUUID       bool
	// GenerationExpression holds the MySQL expression for VIRTUAL/STORED
	// generated columns. It is reported for manual translation; the column
	// itself migrates as a plain column carrying the computed values.
	GenerationExpression string
	// UUIDDemotionReason is set when the column matched the UUID storage
	// shape but its data (or its referenced column's data) cannot convert,
	// so it keeps its original type.
	UUIDDemotionReason string
	// CastType carries a user cast rule's target type; it replaces the
	// built-in mapping and exempts the column from UUID conversion.
	CastType string
}

// isGeneratedColumn reports whether EXTRA marks a MySQL VIRTUAL or STORED
// generated column (as opposed to DEFAULT_GENERATED expression defaults).
func isGeneratedColumn(extra string) bool {
	lower := strings.ToLower(extra)
	return strings.Contains(lower, "virtual generated") || strings.Contains(lower, "stored generated")
}

type sourceTable struct {
	name   string
	engine string
}

// getMySQLTables retrieves all base table names and storage engines from MySQL.
// Engine validation happens in selectTables so that excluded tables may use
// any storage engine.
func (m *Migrator) getMySQLTables(ctx context.Context) ([]sourceTable, error) {
	rows, err := m.sourceQueryer(ctx).QueryContext(ctx, `
		SELECT TABLE_NAME, ENGINE
		FROM INFORMATION_SCHEMA.TABLES
		WHERE TABLE_SCHEMA = DATABASE()
		AND TABLE_TYPE = 'BASE TABLE'
		ORDER BY TABLE_NAME
	`)
	if err != nil {
		return nil, fmt.Errorf("failed to get MySQL tables: %w", err)
	}
	defer rows.Close()

	var tables []sourceTable
	for rows.Next() {
		var tableName string
		var engine sql.NullString
		if err := rows.Scan(&tableName, &engine); err != nil {
			return nil, fmt.Errorf("failed to scan table name: %w", err)
		}
		tables = append(tables, sourceTable{name: tableName, engine: engine.String})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate MySQL tables: %w", err)
	}
	return tables, nil
}

// selectTables applies the configured table filter and validates only the
// tables that will actually be migrated.
func (m *Migrator) selectTables(sourceTables []sourceTable) ([]string, error) {
	names := make([]string, len(sourceTables))
	enginesByName := make(map[string]string, len(sourceTables))
	for i, table := range sourceTables {
		names[i] = table.name
		enginesByName[strings.ToLower(table.name)] = table.engine
	}

	tables, err := m.filter.apply(names)
	if err != nil {
		return nil, err
	}
	for _, table := range tables {
		if err := validatePostgresIdentifier(table, "source table name"); err != nil {
			return nil, err
		}
		engine := enginesByName[strings.ToLower(table)]
		if !strings.EqualFold(engine, "InnoDB") {
			return nil, fmt.Errorf("source table %s uses storage engine %q; a consistent resumable snapshot currently requires InnoDB", table, engine)
		}
	}
	return tables, nil
}

func (m *Migrator) validateSourceForeignKeyScope(ctx context.Context) error {
	rows, err := m.sourceQueryer(ctx).QueryContext(ctx, `
		SELECT TABLE_NAME, CONSTRAINT_NAME, REFERENCED_TABLE_SCHEMA
		FROM INFORMATION_SCHEMA.KEY_COLUMN_USAGE
		WHERE TABLE_SCHEMA = DATABASE()
		AND REFERENCED_TABLE_SCHEMA IS NOT NULL
		AND REFERENCED_TABLE_SCHEMA <> DATABASE()
	`)
	if err != nil {
		return fmt.Errorf("inspect cross-database foreign keys: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var tableName, constraintName, referencedSchema string
		if err := rows.Scan(&tableName, &constraintName, &referencedSchema); err != nil {
			return fmt.Errorf("scan cross-database foreign key metadata: %w", err)
		}
		// Cross-database references only block the migration when the
		// dependent table is actually selected.
		if !m.filter.selects(tableName) {
			continue
		}
		return fmt.Errorf("foreign key %s on table %s references MySQL database %s; cross-database references cannot be mapped safely into one target schema", constraintName, tableName, referencedSchema)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate cross-database foreign keys: %w", err)
	}
	return nil
}
