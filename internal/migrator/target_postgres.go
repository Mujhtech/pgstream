package migrator

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/google/uuid"
)

type postgresColumnInfo struct {
	name       string
	dataType   string
	udtName    string
	isNullable bool
}

type tableValidationMetadata struct {
	columns            map[string]postgresColumnInfo
	enumValuesByColumn map[string]map[string]struct{}
}

func (m *Migrator) validateFreshTargetTables(ctx context.Context, tables []string) error {
	for _, table := range tables {
		var rowCount int64
		query := fmt.Sprintf(
			"SELECT COUNT(*) FROM %s.%s",
			quotePostgresIdentifier(m.schemaName),
			quotePostgresIdentifier(table),
		)
		if err := m.postgres.GetDB().QueryRowContext(ctx, query).Scan(&rowCount); err != nil {
			return fmt.Errorf("count target rows in %s.%s: %w", m.schemaName, table, err)
		}
		if rowCount != 0 {
			return fmt.Errorf("target table %s.%s contains %d rows; a fresh session requires empty target tables", m.schemaName, table, rowCount)
		}
	}
	return nil
}

func (m *Migrator) ensureSchemaExists(ctx context.Context) error {
	// Use schema from migrator configuration

	// Check if schema exists
	var exists bool
	err := m.postgres.GetDB().QueryRowContext(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM information_schema.schemata 
			WHERE schema_name = $1
		)
	`, m.schemaName).Scan(&exists)

	if err != nil {
		return fmt.Errorf("failed to check if schema exists: %w", err)
	}

	if !exists {
		// Create schema
		_, err = m.postgres.GetDB().ExecContext(ctx, fmt.Sprintf("CREATE SCHEMA IF NOT EXISTS %s", quotePostgresIdentifier(m.schemaName)))
		if err != nil {
			return fmt.Errorf("failed to create schema %s: %w", m.schemaName, err)
		}
		m.logf("✅ Created schema: %s\n", m.schemaName)
	} else {
		m.logf("✅ Schema exists: %s\n", m.schemaName)
	}

	return nil
}

func (m *Migrator) tableExistsInPostgres(ctx context.Context, table string) (bool, error) {

	var exists bool
	err := m.postgres.GetDB().QueryRowContext(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM information_schema.tables 
			WHERE table_schema = $1 AND table_name = $2
		)
	`, m.schemaName, table).Scan(&exists)

	return exists, err
}

func (m *Migrator) getPostgresColumnNames(ctx context.Context, table string) ([]string, error) {

	rows, err := m.postgres.GetDB().QueryContext(ctx, `
		SELECT column_name 
		FROM information_schema.columns 
		WHERE table_schema = $1 AND table_name = $2
		ORDER BY ordinal_position
	`, m.schemaName, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var cols []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		// check if name is in the list of columns
		if slices.Contains(cols, name) {
			continue
		}

		cols = append(cols, name)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return cols, nil
}

func (m *Migrator) mapColumnsToPostgres(mysqlColumns []string, postgresColumns []string) ([]string, error) {
	// Create a map of lowercase column names to actual PostgreSQL column names
	postgresColMap := make(map[string]string)
	for _, col := range postgresColumns {
		postgresColMap[strings.ToLower(col)] = col
	}

	// Map MySQL columns to PostgreSQL columns
	var mappedColumns []string
	for _, mysqlCol := range mysqlColumns {
		lowercaseCol := strings.ToLower(mysqlCol)
		if postgresCol, exists := postgresColMap[lowercaseCol]; exists {
			mappedColumns = append(mappedColumns, postgresCol)
		} else {
			return nil, fmt.Errorf("could not map MySQL column %q to an exact PostgreSQL column", mysqlCol)
		}
	}

	return mappedColumns, nil
}

func (m *Migrator) syncIdentitySequences(ctx context.Context, table string) error {
	sourceColumn, sourceNextValue, err := m.getMySQLAutoIncrementMetadata(ctx, table)
	if err != nil {
		return err
	}

	rows, err := m.postgres.GetDB().QueryContext(ctx, `
		SELECT column_name
		FROM information_schema.columns
		WHERE table_schema = $1
		AND table_name = $2
		AND (is_identity = 'YES' OR column_default LIKE 'nextval(%')
		ORDER BY ordinal_position
	`, m.schemaName, table)
	if err != nil {
		return err
	}
	defer rows.Close()

	var columns []string
	for rows.Next() {
		var column string
		if err := rows.Scan(&column); err != nil {
			return err
		}
		columns = append(columns, column)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if sourceColumn == "" {
		if len(columns) > 0 {
			return fmt.Errorf("target identity columns (%s) have no matching MySQL AUTO_INCREMENT column", strings.Join(columns, ", "))
		}
		return nil
	}
	if len(columns) != 1 || !strings.EqualFold(columns[0], sourceColumn) {
		return fmt.Errorf("target identity columns (%s) do not match MySQL AUTO_INCREMENT column %s", strings.Join(columns, ", "), sourceColumn)
	}

	qualifiedTable := quotePostgresIdentifier(m.schemaName) + "." + quotePostgresIdentifier(table)
	for _, column := range columns {
		var sequenceName sql.NullString
		if err := m.postgres.GetDB().QueryRowContext(
			ctx,
			`SELECT pg_get_serial_sequence($1, $2)`,
			qualifiedTable,
			column,
		).Scan(&sequenceName); err != nil {
			return fmt.Errorf("resolve sequence for column %s: %w", column, err)
		}
		if !sequenceName.Valid || sequenceName.String == "" {
			continue
		}

		query := fmt.Sprintf(
			`SELECT MAX(%s)::bigint FROM %s`,
			quotePostgresIdentifier(column),
			qualifiedTable,
		)
		var targetMaximum sql.NullInt64
		if err := m.postgres.GetDB().QueryRowContext(ctx, query).Scan(&targetMaximum); err != nil {
			return fmt.Errorf("read maximum value for identity column %s: %w", column, err)
		}
		sequenceValue, isCalled, err := sequenceState(targetMaximum, sourceNextValue)
		if err != nil {
			return fmt.Errorf("calculate sequence state for column %s: %w", column, err)
		}
		if err := m.postgres.GetDB().QueryRowContext(
			ctx,
			`SELECT setval($1::regclass, $2, $3)`,
			sequenceName.String,
			sequenceValue,
			isCalled,
		).Scan(&sequenceValue); err != nil {
			return fmt.Errorf("synchronize sequence %s for column %s: %w", sequenceName.String, column, err)
		}
	}

	return nil
}

func sequenceState(targetMaximum sql.NullInt64, sourceNextValue sql.NullInt64) (int64, bool, error) {
	const maxInt64 = int64(^uint64(0) >> 1)
	desiredNextValue := int64(1)
	if targetMaximum.Valid {
		if targetMaximum.Int64 == maxInt64 {
			return 0, false, fmt.Errorf("target maximum has exhausted the PostgreSQL sequence range")
		}
		if targetMaximum.Int64 >= desiredNextValue {
			desiredNextValue = targetMaximum.Int64 + 1
		}
	}
	if sourceNextValue.Valid {
		if sourceNextValue.Int64 < 1 {
			return 0, false, fmt.Errorf("source AUTO_INCREMENT next value must be positive")
		}
		if sourceNextValue.Int64 > desiredNextValue {
			desiredNextValue = sourceNextValue.Int64
		}
	}
	if desiredNextValue == 1 {
		return 1, false, nil
	}
	return desiredNextValue - 1, true, nil
}

func (m *Migrator) isEnumColumn(dataType string, udtName string) bool {
	// Check if it's a USER-DEFINED type (which includes enums)
	if dataType == "USER-DEFINED" {
		// Check if it's not an array (arrays start with _)
		if !strings.HasPrefix(udtName, "_") {
			return true
		}
	}
	return false
}

func (m *Migrator) getPostgresEnumValues(ctx context.Context, table string, column string) ([]string, error) {

	// Get the enum type name for the column
	var enumType string
	err := m.postgres.GetDB().QueryRowContext(ctx, `
		SELECT udt_name 
		FROM information_schema.columns 
		WHERE table_schema = $1 AND table_name = $2 AND column_name = $3
	`, m.schemaName, table, column).Scan(&enumType)

	if err != nil {
		return nil, err
	}

	return m.getPostgresEnumValuesByType(ctx, enumType)
}

func (m *Migrator) getPostgresEnumValuesByType(ctx context.Context, enumType string) ([]string, error) {
	rows, err := m.postgres.GetDB().QueryContext(ctx, `
		SELECT enumlabel 
		FROM pg_enum 
		WHERE enumtypid = (
			SELECT type_info.oid
			FROM pg_type type_info
			JOIN pg_namespace schema_info ON schema_info.oid = type_info.typnamespace
			WHERE type_info.typname = $1 AND schema_info.nspname = $2
		)
		ORDER BY enumsortorder
	`, enumType, m.schemaName)

	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var values []string
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	return values, nil
}

func (m *Migrator) loadValidationMetadata(ctx context.Context, table string) (*tableValidationMetadata, error) {
	cacheKey := strings.ToLower(m.schemaName + "." + table)
	m.metadataMu.RLock()
	metadata, exists := m.metadataCache[cacheKey]
	m.metadataMu.RUnlock()
	if exists {
		return metadata, nil
	}
	rows, err := m.postgres.GetDB().QueryContext(ctx, `
		SELECT column_name, data_type, udt_name, is_nullable
		FROM information_schema.columns 
		WHERE table_schema = $1 AND table_name = $2
		ORDER BY ordinal_position
	`, m.schemaName, table)
	if err != nil {
		return nil, err
	}

	columnInfo := make(map[string]postgresColumnInfo)
	for rows.Next() {
		var name, dataType, udtName, isNullable string
		if err := rows.Scan(&name, &dataType, &udtName, &isNullable); err != nil {
			_ = rows.Close()
			return nil, err
		}
		columnInfo[strings.ToLower(name)] = postgresColumnInfo{
			name:       name,
			dataType:   dataType,
			udtName:    udtName,
			isNullable: isNullable == "YES",
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}

	enumValuesByColumn := make(map[string]map[string]struct{})
	for column, info := range columnInfo {
		if !m.isEnumColumn(info.dataType, info.udtName) {
			continue
		}
		values, err := m.getPostgresEnumValues(ctx, table, info.name)
		if err != nil {
			return nil, fmt.Errorf("get enum values for column %s: %w", info.name, err)
		}
		valueSet := make(map[string]struct{}, len(values))
		for _, value := range values {
			valueSet[value] = struct{}{}
		}
		enumValuesByColumn[column] = valueSet
	}

	metadata = &tableValidationMetadata{
		columns:            columnInfo,
		enumValuesByColumn: enumValuesByColumn,
	}
	m.metadataMu.Lock()
	m.metadataCache[cacheKey] = metadata
	m.metadataMu.Unlock()
	return metadata, nil
}

func (m *Migrator) createTableInPostgres(ctx context.Context, table string) error {

	// Get MySQL table structure
	mysqlColumns, err := m.getMySQLTableStructure(ctx, table)
	if err != nil {
		return fmt.Errorf("failed to get MySQL table structure: %w", err)
	}

	// Create enum types first if needed
	err = m.createEnumTypes(ctx, table, mysqlColumns)
	if err != nil {
		return fmt.Errorf("failed to create enum types: %w", err)
	}

	// Build CREATE TABLE statement
	createSQL, err := m.buildCreateTableSQL(table, mysqlColumns)
	if err != nil {
		return fmt.Errorf("build target table %s: %w", table, err)
	}

	// Execute CREATE TABLE
	_, err = m.postgres.GetDB().ExecContext(ctx, createSQL)
	if err != nil {
		return fmt.Errorf("failed to create table %s: %w", table, err)
	}
	if err := m.ensurePrimaryKey(ctx, table); err != nil {
		return fmt.Errorf("failed to create primary key for %s: %w", table, err)
	}

	commentCount, err := m.applyComments(ctx, table, mysqlColumns)
	if err != nil {
		return fmt.Errorf("failed to migrate comments for %s: %w", table, err)
	}

	commentNote := ""
	if commentCount > 0 {
		commentNote = fmt.Sprintf(", %d comments", commentCount)
	}
	m.logf("✅ Created table %s.%s (%d columns%s)\n", m.schemaName, table, len(mysqlColumns), commentNote)
	return nil
}

// applyComments carries MySQL table and column comments to the target as
// COMMENT ON statements, returning how many were applied. Comments only
// accompany table creation; pre-existing target tables are never modified.
func (m *Migrator) applyComments(ctx context.Context, table string, columns []ColumnInfo) (int, error) {
	applied := 0
	qualified := quotePostgresIdentifier(m.schemaName) + "." + quotePostgresIdentifier(table)

	tableComment, err := m.getMySQLTableComment(ctx, table)
	if err != nil {
		return applied, err
	}
	if tableComment != "" {
		statement := fmt.Sprintf("COMMENT ON TABLE %s IS %s", qualified, quotePostgresLiteral(tableComment))
		if _, err := m.postgres.GetDB().ExecContext(ctx, statement); err != nil {
			return applied, fmt.Errorf("comment on table %s: %w", table, err)
		}
		applied++
	}

	for _, col := range columns {
		if col.Comment == "" {
			continue
		}
		statement := fmt.Sprintf("COMMENT ON COLUMN %s.%s IS %s", qualified, quotePostgresIdentifier(col.Name), quotePostgresLiteral(col.Comment))
		if _, err := m.postgres.GetDB().ExecContext(ctx, statement); err != nil {
			return applied, fmt.Errorf("comment on column %s.%s: %w", table, col.Name, err)
		}
		applied++
	}
	return applied, nil
}

func (m *Migrator) ensurePrimaryKey(ctx context.Context, table string) error {
	sourcePrimaryKeyColumns, err := m.getMySQLPrimaryKeyColumns(ctx, table)
	if err != nil {
		return err
	}
	postgresColumns, err := m.getPostgresColumnNames(ctx, table)
	if err != nil {
		return err
	}
	mappedPrimaryKeyColumns, err := m.mapColumnsToPostgres(sourcePrimaryKeyColumns, postgresColumns)
	if err != nil {
		return err
	}
	targetPrimaryKeyColumns, err := m.getPostgresPrimaryKeyColumns(ctx, table)
	if err != nil {
		return err
	}

	if len(mappedPrimaryKeyColumns) == 0 {
		if len(targetPrimaryKeyColumns) > 0 {
			return fmt.Errorf("source table has no primary key but target primary key is (%s)", strings.Join(targetPrimaryKeyColumns, ", "))
		}
		return nil
	}
	if len(targetPrimaryKeyColumns) > 0 {
		if !equalIdentifierLists(mappedPrimaryKeyColumns, targetPrimaryKeyColumns) {
			return fmt.Errorf(
				"target primary key (%s) does not match source primary key (%s)",
				strings.Join(targetPrimaryKeyColumns, ", "),
				strings.Join(mappedPrimaryKeyColumns, ", "),
			)
		}
		return nil
	}

	quotedColumns := make([]string, len(mappedPrimaryKeyColumns))
	for i, column := range mappedPrimaryKeyColumns {
		quotedColumns[i] = quotePostgresIdentifier(column)
	}
	query := fmt.Sprintf(
		"ALTER TABLE %s.%s ADD CONSTRAINT %s PRIMARY KEY (%s)",
		quotePostgresIdentifier(m.schemaName),
		quotePostgresIdentifier(table),
		quotePostgresIdentifier(postgresPrimaryKeyName(table)),
		strings.Join(quotedColumns, ", "),
	)
	_, err = m.postgres.GetDB().ExecContext(ctx, query)
	return err
}

func (m *Migrator) getPostgresPrimaryKeyColumns(ctx context.Context, table string) ([]string, error) {
	rows, err := m.postgres.GetDB().QueryContext(ctx, `
		SELECT key_column.column_name
		FROM information_schema.table_constraints constraint_info
		JOIN information_schema.key_column_usage key_column
			ON key_column.constraint_catalog = constraint_info.constraint_catalog
			AND key_column.constraint_schema = constraint_info.constraint_schema
			AND key_column.constraint_name = constraint_info.constraint_name
			AND key_column.table_schema = constraint_info.table_schema
			AND key_column.table_name = constraint_info.table_name
		WHERE constraint_info.constraint_type = 'PRIMARY KEY'
		AND constraint_info.table_schema = $1
		AND constraint_info.table_name = $2
		ORDER BY key_column.ordinal_position
	`, m.schemaName, table)
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

func equalIdentifierLists(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if !strings.EqualFold(left[index], right[index]) {
			return false
		}
	}
	return true
}

func (m *Migrator) createEnumTypes(ctx context.Context, table string, columns []ColumnInfo) error {
	type enumColumn struct {
		name   string
		values []string
	}
	var enums []enumColumn
	for _, col := range columns {
		if strings.Contains(strings.ToLower(col.Type), "enum") {
			enums = append(enums, enumColumn{name: col.Name, values: m.extractEnumValues(col.Type)})
		}
	}
	if len(enums) == 0 {
		return nil
	}

	// Non-strict MySQL stores invalid enum inserts as the special
	// empty-string value. If the data contains it, mirror it as a real ''
	// label so every row copies losslessly.
	names := make([]string, len(enums))
	for i, enum := range enums {
		names[i] = enum.name
	}
	markerCounts, err := m.sourceEnumEmptyMarkerCounts(ctx, table, names)
	if err != nil {
		return fmt.Errorf("inspect enum columns of %s for the empty-string marker: %w", table, err)
	}

	for _, enum := range enums {
		enumValues := enum.values
		if markerCounts[enum.name] > 0 && !slices.Contains(enumValues, "") {
			m.warnf("⚠️  Column %s.%s has %d rows holding MySQL's empty-string invalid-enum marker (find them with: SELECT * FROM %s WHERE %s = ''); adding '' to the PostgreSQL enum type so the data copies losslessly. Clean these rows up after migration.", table, enum.name, markerCounts[enum.name], quoteMySQLIdentifier(table), quoteMySQLIdentifier(enum.name))
			enumValues = append([]string{""}, enumValues...)
		}

		if len(enumValues) > 0 {
			enumTypeName := postgresEnumTypeName(table, enum.name)
			if err := m.createPostgresEnumType(ctx, enumTypeName, enumValues); err != nil {
				return fmt.Errorf("failed to create enum type %s: %w", enumTypeName, err)
			}
		} else {
			m.warnf("⚠️  No enum values extracted for column %s\n", enum.name)
		}
	}
	return nil
}

func (m *Migrator) createPostgresEnumType(ctx context.Context, enumTypeName string, values []string) error {

	// Check if enum type already exists
	var exists bool
	err := m.postgres.GetDB().QueryRowContext(ctx, `
		SELECT EXISTS(
			SELECT 1
			FROM pg_type type_info
			JOIN pg_namespace schema_info ON schema_info.oid = type_info.typnamespace
			WHERE type_info.typname = $1 AND schema_info.nspname = $2
		)
	`, enumTypeName, m.schemaName).Scan(&exists)

	if err != nil {
		return fmt.Errorf("failed to check if enum type exists: %w", err)
	}

	if exists {
		existingValues, err := m.getPostgresEnumValuesByType(ctx, enumTypeName)
		if err != nil {
			return fmt.Errorf("inspect existing enum type %s: %w", enumTypeName, err)
		}
		if !slices.Equal(existingValues, values) {
			return fmt.Errorf("existing enum type %s has values %v; source requires %v", enumTypeName, existingValues, values)
		}
		m.logf("ℹ️  Enum type %s already exists with matching values\n", enumTypeName)
		return nil
	}

	// Build the enum values string
	var quotedValues []string
	for _, value := range values {
		quotedValues = append(quotedValues, quotePostgresLiteral(value))
	}
	valuesStr := strings.Join(quotedValues, ", ")

	// Create the enum type
	createEnumSQL := fmt.Sprintf(`CREATE TYPE %s.%s AS ENUM (%s)`, quotePostgresIdentifier(m.schemaName), quotePostgresIdentifier(enumTypeName), valuesStr)

	_, err = m.postgres.GetDB().ExecContext(ctx, createEnumSQL)
	if err != nil {
		return fmt.Errorf("failed to create enum type %s: %w", enumTypeName, err)
	}

	m.logf("✅ Created enum type %s.%s (%s)\n", m.schemaName, enumTypeName, valuesStr)
	return nil
}

func (m *Migrator) buildCreateTableSQL(table string, columns []ColumnInfo) (string, error) {

	var columnDefs []string
	for _, col := range columns {
		def, _, err := m.buildColumnDefinition(table, col)
		if err != nil {
			return "", err
		}
		columnDefs = append(columnDefs, def)
	}

	return fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s.%s (%s)`,
		quotePostgresIdentifier(m.schemaName), quotePostgresIdentifier(table), strings.Join(columnDefs, ", ")), nil
}

// buildColumnDefinition converts one MySQL column into its PostgreSQL column
// definition, returning the resolved PostgreSQL type alongside the DDL
// fragment.
func (m *Migrator) buildColumnDefinition(table string, col ColumnInfo) (string, string, error) {
	var pgType string
	if col.CastType != "" {
		pgType = strings.ToUpper(col.CastType)
	} else {
		var err error
		pgType, err = m.convertMySQLTypeToPostgres(col.Type, col.Name, table)
		if err != nil {
			return "", "", fmt.Errorf("column %s: %w", col.Name, err)
		}
		if col.IsUUID {
			pgType = "UUID"
		}
	}

	def := fmt.Sprintf(`%s %s`, quotePostgresIdentifier(col.Name), pgType)

	if !col.Nullable {
		def += " NOT NULL"
	}

	extra := strings.ToLower(col.Extra)
	// ON UPDATE CURRENT_TIMESTAMP (the only ON UPDATE MySQL supports) has no
	// PostgreSQL column equivalent; the column itself migrates normally and
	// the auto-update behavior is emitted as trigger DDL for manual review
	// during schema object migration.
	if strings.Contains(extra, "on update") && !strings.Contains(extra, "on update current_timestamp") {
		return "", pgType, fmt.Errorf("column %s uses unsupported MySQL ON UPDATE behavior %q", col.Name, col.Extra)
	}
	// VIRTUAL/STORED generated columns become plain columns carrying the
	// values MySQL had computed at snapshot time; the untranslatable
	// generation expression is reported as manual work during schema object
	// migration. Their MySQL defaults never apply, so stop here.
	if isGeneratedColumn(extra) {
		return def, pgType, nil
	}
	if strings.Contains(extra, "auto_increment") {
		if !isPostgresIntegerType(pgType) {
			return "", pgType, fmt.Errorf("AUTO_INCREMENT column %s maps to %s, which cannot use a PostgreSQL identity sequence safely", col.Name, pgType)
		}
		def += " GENERATED BY DEFAULT AS IDENTITY"
	} else if col.HasDefault {
		defaultValue, err := postgresDefaultValue(col.DefaultValue, pgType, col.Extra)
		if err != nil {
			return "", pgType, fmt.Errorf("column %s default: %w", col.Name, err)
		}
		def += " DEFAULT " + defaultValue
	}

	return def, pgType, nil
}

func isPostgresIntegerType(postgresType string) bool {
	switch postgresType {
	case "SMALLINT", "INTEGER", "BIGINT":
		return true
	default:
		return false
	}
}

var numericDefaultPattern = regexp.MustCompile(`^[+-]?(?:[0-9]+(?:\.[0-9]*)?|\.[0-9]+)(?:[eE][+-]?[0-9]+)?$`)

func postgresDefaultValue(value string, postgresType string, extra string) (string, error) {
	trimmed := strings.TrimSpace(value)
	upper := strings.ToUpper(trimmed)
	temporalType := strings.HasPrefix(postgresType, "DATE") || strings.HasPrefix(postgresType, "TIME")
	if temporalType && (upper == "CURRENT_TIMESTAMP" || strings.HasPrefix(upper, "CURRENT_TIMESTAMP(")) {
		return "CURRENT_TIMESTAMP", nil
	}
	if temporalType && (upper == "CURRENT_DATE" || upper == "CURRENT_TIME") {
		return upper, nil
	}
	if strings.Contains(strings.ToLower(extra), "default_generated") {
		return "", fmt.Errorf("unsupported generated default expression %q", value)
	}

	if postgresType == "BOOLEAN" {
		switch strings.ToLower(trimmed) {
		case "1", "true", "b'1'":
			return "TRUE", nil
		case "0", "false", "b'0'":
			return "FALSE", nil
		default:
			return "", fmt.Errorf("invalid boolean default %q", value)
		}
	}
	if isPostgresIntegerType(postgresType) || strings.HasPrefix(postgresType, "NUMERIC") || strings.HasPrefix(postgresType, "DECIMAL") || postgresType == "REAL" || postgresType == "DOUBLE PRECISION" {
		if numericDefaultPattern.MatchString(trimmed) {
			return trimmed, nil
		}
		return "", fmt.Errorf("invalid numeric default %q", value)
	}
	if postgresType == "UUID" {
		parsed, err := uuid.Parse(trimmed)
		if err != nil {
			return "", fmt.Errorf("unsupported UUID default %q", value)
		}
		return quotePostgresLiteral(parsed.String()) + "::uuid", nil
	}
	if postgresType == "JSONB" {
		if !json.Valid([]byte(value)) {
			return "", fmt.Errorf("unsupported JSON default %q", value)
		}
		return quotePostgresLiteral(value) + "::jsonb", nil
	}
	if postgresType == "BYTEA" {
		return "", fmt.Errorf("binary defaults require explicit manual conversion")
	}

	return quotePostgresLiteral(value), nil
}

func (m *Migrator) convertMySQLTypeToPostgres(mysqlType string, columnName string, tableName string) (string, error) {
	// Convert MySQL types to PostgreSQL types

	mysqlTypeLower := strings.ToLower(strings.TrimSpace(mysqlType))

	switch {
	case strings.Contains(mysqlTypeLower, "enum"):
		// Use the created enum type
		enumTypeName := postgresEnumTypeName(tableName, columnName)
		return fmt.Sprintf(`%s.%s`, quotePostgresIdentifier(m.schemaName), quotePostgresIdentifier(enumTypeName)), nil
	case strings.HasPrefix(mysqlTypeLower, "tinyint(1)") || mysqlTypeLower == "boolean" || mysqlTypeLower == "bool" || mysqlTypeLower == "bit(1)":
		return "BOOLEAN", nil
	case strings.HasPrefix(mysqlTypeLower, "bit("):
		return "BYTEA", nil
	case strings.HasPrefix(mysqlTypeLower, "year"):
		return "SMALLINT", nil
	case strings.Contains(mysqlTypeLower, "geometry") || strings.Contains(mysqlTypeLower, "point") || strings.Contains(mysqlTypeLower, "linestring") || strings.Contains(mysqlTypeLower, "polygon"):
		return "BYTEA", nil
	case strings.Contains(mysqlTypeLower, "int"):
		if strings.Contains(mysqlTypeLower, "bigint") {
			if strings.Contains(mysqlTypeLower, "unsigned") {
				// NUMERIC(20) is the lossless mapping (0..2^64-1), but the
				// resolver demotes to BIGINT when a data scan proved the
				// column's foreign-key group fits the signed range — numeric
				// can neither back an identity sequence nor join a foreign
				// key against integer columns.
				if m.demotedUnsignedBigint(tableName, columnName) {
					return "BIGINT", nil
				}
				return "NUMERIC(20)", nil
			}
			return "BIGINT", nil
		} else if strings.Contains(mysqlTypeLower, "smallint") {
			if strings.Contains(mysqlTypeLower, "unsigned") {
				return "INTEGER", nil
			}
			return "SMALLINT", nil
		} else if strings.Contains(mysqlTypeLower, "tinyint") {
			return "SMALLINT", nil
		} else {
			if strings.Contains(mysqlTypeLower, "unsigned") {
				return "BIGINT", nil
			}
			return "INTEGER", nil
		}
	case strings.Contains(mysqlTypeLower, "varchar"):
		// Extract size from VARCHAR(n)
		if strings.Contains(mysqlTypeLower, "(") {
			return strings.ToUpper(mysqlType), nil
		}
		return "VARCHAR(255)", nil
	case strings.Contains(mysqlTypeLower, "text"):
		return "TEXT", nil
	case strings.HasPrefix(mysqlTypeLower, "set("):
		return "TEXT", nil
	case strings.Contains(mysqlTypeLower, "datetime"):
		return "TIMESTAMP", nil
	case strings.Contains(mysqlTypeLower, "timestamp"):
		return "TIMESTAMP", nil
	case strings.Contains(mysqlTypeLower, "date"):
		return "DATE", nil
	case strings.Contains(mysqlTypeLower, "decimal"):
		return strings.ToUpper(strings.ReplaceAll(mysqlTypeLower, " unsigned", "")), nil
	case strings.Contains(mysqlTypeLower, "float"):
		return "REAL", nil
	case strings.Contains(mysqlTypeLower, "double"):
		return "DOUBLE PRECISION", nil
	case strings.Contains(mysqlTypeLower, "blob"):
		return "BYTEA", nil
	case strings.Contains(mysqlTypeLower, "binary"):
		return "BYTEA", nil
	case strings.Contains(mysqlTypeLower, "json"):
		return "JSONB", nil
	case strings.Contains(mysqlTypeLower, "char"):
		return strings.ToUpper(mysqlType), nil
	case strings.Contains(mysqlTypeLower, "time"):
		return "TIME", nil
	default:
		return "", fmt.Errorf("unsupported MySQL type %q", mysqlType)
	}
}

func isUUIDStorageType(mysqlType string) bool {
	mysqlType = strings.ToLower(mysqlType)
	return strings.HasPrefix(mysqlType, "char(36)") ||
		strings.HasPrefix(mysqlType, "varchar(36)") ||
		strings.HasPrefix(mysqlType, "binary(16)") ||
		strings.HasPrefix(mysqlType, "varbinary(16)")
}

func postgresEnumTypeName(tableName string, columnName string) string {
	candidate := fmt.Sprintf("%s_%s_enum", tableName, columnName)
	return hashedPostgresObjectName(candidate, tableName+"\x00"+columnName+"\x00enum")
}

func postgresPrimaryKeyName(tableName string) string {
	return hashedPostgresObjectName(tableName+"_pkey", tableName+"\x00pkey")
}

// createAllTables creates all tables in PostgreSQL (structure only, no data)
func (m *Migrator) createAllTables(ctx context.Context, tables []string) error {
	for i, table := range tables {
		m.logf("🏗️  Creating table %d/%d: %s\n", i+1, len(tables), table)

		// Check if table already exists
		exists, err := m.tableExistsInPostgres(ctx, table)
		if err != nil {
			return fmt.Errorf("failed to check if table %s exists: %w", table, err)
		}

		if exists {
			if err := m.verifyExistingTable(ctx, table); err != nil {
				return err
			}
			m.logf("⏭️  Table %s already exists, verified primary key\n", table)
			continue
		}

		// Create table structure
		if err := m.createTableInPostgres(ctx, table); err != nil {
			return fmt.Errorf("failed to create table %s: %w", table, err)
		}
	}

	return nil
}

// verifyExistingTable checks a pre-existing target table against this run's
// expectations: empty when the session is fresh, primary key matching the
// source, and UUID decisions consistent with this run's data-driven
// resolution. Surfacing a mismatch now costs seconds; discovering it at the
// foreign-key stage costs the whole copy.
func (m *Migrator) verifyExistingTable(ctx context.Context, table string) error {
	if m.freshSession {
		targetRows, err := m.targetTableRowCount(ctx, table)
		if err != nil {
			return fmt.Errorf("count existing target table %s: %w", table, err)
		}
		if targetRows != 0 {
			return fmt.Errorf("target table %s.%s contains %d rows; a fresh session requires empty target tables", m.schemaName, table, targetRows)
		}
	}
	if err := m.ensurePrimaryKey(ctx, table); err != nil {
		return fmt.Errorf("failed to ensure primary key for %s: %w", table, err)
	}
	if err := m.verifyExistingUUIDDecisions(ctx, table); err != nil {
		return err
	}
	return m.verifyExistingUnsignedDecisions(ctx, table)
}

// verifyExistingUnsignedDecisions catches BIGINT UNSIGNED type drift on
// pre-existing target tables: a target created by an earlier run (or another
// tool) may hold NUMERIC where this run resolved BIGINT, or vice versa,
// which would surface as a foreign-key failure after the whole copy.
func (m *Migrator) verifyExistingUnsignedDecisions(ctx context.Context, table string) error {
	if !m.unsignedCheck.resolved {
		return nil
	}
	prefix := strings.ToLower(table) + "\x00"
	hasCandidates := false
	for key := range m.unsignedCheck.demote {
		if strings.HasPrefix(key, prefix) {
			hasCandidates = true
			break
		}
	}
	if !hasCandidates {
		for key := range m.unsignedCheck.overflowMax {
			if strings.HasPrefix(key, prefix) {
				hasCandidates = true
				break
			}
		}
	}
	if !hasCandidates {
		return nil
	}

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
		if !isUnsignedBigintType(col.Type) || col.CastType != "" {
			continue
		}
		info, exists := metadata.columns[strings.ToLower(col.Name)]
		if !exists {
			continue
		}
		demote := m.demotedUnsignedBigint(table, col.Name)
		quotedTarget := fmt.Sprintf("%s.%s", quotePostgresIdentifier(m.schemaName), quotePostgresIdentifier(table))
		quotedColumn := quotePostgresIdentifier(col.Name)
		switch {
		case demote && info.udtName == "numeric":
			problems = append(problems, fmt.Sprintf(
				"%s is numeric in the target but this run maps it to bigint (all data verified within the signed range); reconcile with: ALTER TABLE %s ALTER COLUMN %s TYPE bigint USING %s::bigint",
				col.Name, quotedTarget, quotedColumn, quotedColumn))
		case !demote && info.udtName == "int8":
			problems = append(problems, fmt.Sprintf(
				"%s is bigint in the target but this run keeps it NUMERIC(20) (values exceed the signed 63-bit range); reconcile with: ALTER TABLE %s ALTER COLUMN %s TYPE numeric(20) USING %s::numeric",
				col.Name, quotedTarget, quotedColumn, quotedColumn))
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf(
			"existing target table %s.%s was created with different BIGINT UNSIGNED type decisions than this run resolved: %s",
			m.schemaName, table, strings.Join(problems, "; "))
	}
	return nil
}

// verifyTablesForDataOnly replaces table creation when the run is data-only:
// every selected table must already exist on the target (from a prior full or
// schema-only run) and pass the same verification a resume performs.
func (m *Migrator) verifyTablesForDataOnly(ctx context.Context, tables []string) error {
	for _, table := range tables {
		exists, err := m.tableExistsInPostgres(ctx, table)
		if err != nil {
			return fmt.Errorf("failed to check if table %s exists: %w", table, err)
		}
		if !exists {
			return fmt.Errorf("target table %s.%s does not exist; run a full migration or --schema-only first", m.schemaName, table)
		}
		if err := m.verifyExistingTable(ctx, table); err != nil {
			return err
		}
	}

	// A full run creates foreign keys after the copy, so load order never
	// matters. A data-only run inherits whatever constraints the target
	// already carries; loading child tables before their parents would then
	// fail, so the hazard is called out up front.
	fkCount, err := m.targetForeignKeyCount(ctx)
	if err != nil {
		return fmt.Errorf("count target foreign keys: %w", err)
	}
	if fkCount > 0 {
		m.warnf("⚠️  Target schema %s already has %d foreign key constraints; a data-only load copies tables in discovery order and may violate them. If the load fails, drop the constraints, re-run, and restore them afterwards (a full run creates them after the data for this reason)", m.schemaName, fkCount)
	}
	return nil
}

func (m *Migrator) targetForeignKeyCount(ctx context.Context) (int, error) {
	var count int
	err := m.postgres.GetDB().QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM information_schema.table_constraints
		WHERE constraint_schema = $1 AND constraint_type = 'FOREIGN KEY'
	`, m.schemaName).Scan(&count)
	return count, err
}

func (m *Migrator) targetTableRowCount(ctx context.Context, table string) (int64, error) {
	query := fmt.Sprintf(
		"SELECT COUNT(*) FROM %s.%s",
		quotePostgresIdentifier(m.schemaName),
		quotePostgresIdentifier(table),
	)
	var rowCount int64
	if err := m.postgres.GetDB().QueryRowContext(ctx, query).Scan(&rowCount); err != nil {
		return 0, err
	}
	return rowCount, nil
}
