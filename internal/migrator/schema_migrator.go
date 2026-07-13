package migrator

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/jmoiron/sqlx"
)

// IndexInfo represents a database index
type IndexInfo struct {
	TableName    string
	IndexName    string
	Columns      []IndexColumnInfo
	IsUnique     bool
	IsPrimary    bool
	IndexType    string
	IndexComment string
}

type IndexColumnInfo struct {
	Name          string
	PrefixLength  int64
	SortDirection string
}

// ForeignKeyInfo represents a foreign key constraint
type ForeignKeyInfo struct {
	ConstraintName    string
	TableName         string
	ColumnNames       []string
	ReferencedTable   string
	ReferencedColumns []string
	OnDelete          string
	OnUpdate          string
}

// TriggerInfo represents a database trigger
type TriggerInfo struct {
	TriggerName string
}

// FunctionInfo represents a database function
type FunctionInfo struct {
	FunctionName string
}

// ViewInfo represents a database view
type ViewInfo struct {
	ViewName string
}

// SchemaMigrator handles migration of database schema objects
type SchemaMigrator struct {
	mysql    *sqlx.DB
	postgres *sqlx.DB
	schema   string
}

// NewSchemaMigrator creates a new schema migrator
func NewSchemaMigrator(mysql, postgres *sqlx.DB, schema string) *SchemaMigrator {
	return &SchemaMigrator{
		mysql:    mysql,
		postgres: postgres,
		schema:   schema,
	}
}

// MigrateAllSchemaObjects migrates all schema objects for a table
func (sm *SchemaMigrator) MigrateAllSchemaObjects(ctx context.Context, tableName string) error {
	fmt.Printf("🔧 Starting comprehensive schema migration for table: %s\n", tableName)
	var migrationErrors []error

	// 1. Migrate indexes
	if err := sm.migrateIndexes(ctx, tableName); err != nil {
		migrationErrors = append(migrationErrors, fmt.Errorf("failed to migrate indexes for %s: %w", tableName, err))
	}

	// 2. Migrate foreign keys
	if err := sm.migrateForeignKeys(ctx, tableName); err != nil {
		migrationErrors = append(migrationErrors, fmt.Errorf("failed to migrate foreign keys for %s: %w", tableName, err))
	}

	// 3. Migrate triggers
	if err := sm.migrateTriggers(ctx, tableName); err != nil {
		migrationErrors = append(migrationErrors, fmt.Errorf("failed to migrate triggers for %s: %w", tableName, err))
	}

	if len(migrationErrors) == 0 {
		fmt.Printf("✅ Completed schema migration for table: %s\n", tableName)
	}
	return errors.Join(migrationErrors...)
}

// MigrateAllFunctions migrates all functions from MySQL to PostgreSQL
func (sm *SchemaMigrator) MigrateAllFunctions(ctx context.Context) error {
	fmt.Printf("🔧 Starting function migration\n")

	functions, err := sm.getMySQLFunctions(ctx)
	if err != nil {
		return fmt.Errorf("failed to get MySQL functions: %w", err)
	}

	for _, function := range functions {
		fmt.Printf("⚠️  Skipping function %s: automatic MySQL procedural SQL conversion is not yet safe\n", function.FunctionName)
	}

	return nil
}

// MigrateAllViews migrates all views from MySQL to PostgreSQL
func (sm *SchemaMigrator) MigrateAllViews(ctx context.Context) error {
	fmt.Printf("🔧 Discovering views\n")

	views, err := sm.getMySQLViews(ctx)
	if err != nil {
		return fmt.Errorf("failed to get MySQL views: %w", err)
	}

	for _, view := range views {
		fmt.Printf("⚠️  Skipping view %s: automatic MySQL SQL conversion is not yet safe\n", view.ViewName)
	}
	return nil
}

// getMySQLIndexes retrieves all indexes for a table from MySQL
func (sm *SchemaMigrator) getMySQLIndexes(ctx context.Context, tableName string) ([]IndexInfo, error) {
	query := `
		SELECT 
			INDEX_NAME,
			COLUMN_NAME,
			NON_UNIQUE,
				SEQ_IN_INDEX,
				INDEX_TYPE,
				INDEX_COMMENT,
				SUB_PART,
				COLLATION
		FROM INFORMATION_SCHEMA.STATISTICS 
		WHERE TABLE_SCHEMA = DATABASE() 
		AND TABLE_NAME = ?
		ORDER BY INDEX_NAME, SEQ_IN_INDEX
	`

	rows, err := sm.mysql.QueryContext(ctx, query, tableName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var indexes []IndexInfo
	indexPositions := make(map[string]int)

	for rows.Next() {
		var indexName, indexType string
		var columnName, indexComment, collation sql.NullString
		var nonUnique, seqInIndex int
		var prefixLength sql.NullInt64
		if err := rows.Scan(&indexName, &columnName, &nonUnique, &seqInIndex, &indexType, &indexComment, &prefixLength, &collation); err != nil {
			return nil, err
		}
		if !columnName.Valid || columnName.String == "" {
			return nil, fmt.Errorf("index %s on %s uses a functional key part that cannot be migrated safely", indexName, tableName)
		}

		// Check if this is a primary key
		isPrimary := indexName == "PRIMARY"
		isUnique := nonUnique == 0 || isPrimary
		column := IndexColumnInfo{
			Name:          columnName.String,
			SortDirection: collation.String,
		}
		if prefixLength.Valid {
			column.PrefixLength = prefixLength.Int64
		}

		if position, exists := indexPositions[indexName]; exists {
			indexes[position].Columns = append(indexes[position].Columns, column)
		} else {
			indexPositions[indexName] = len(indexes)
			indexes = append(indexes, IndexInfo{
				TableName:    tableName,
				IndexName:    indexName,
				Columns:      []IndexColumnInfo{column},
				IsUnique:     isUnique,
				IsPrimary:    isPrimary,
				IndexType:    indexType,
				IndexComment: indexComment.String,
			})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	return indexes, nil
}

// migrateIndexes migrates indexes for a table
func (sm *SchemaMigrator) migrateIndexes(ctx context.Context, tableName string) error {
	indexes, err := sm.getMySQLIndexes(ctx, tableName)
	if err != nil {
		return err
	}

	var migrationErrors []error
	for _, index := range indexes {
		if index.IsPrimary {
			// Primary keys are handled during table creation
			continue
		}

		if index.IndexType != "" && !strings.EqualFold(index.IndexType, "BTREE") {
			migrationErrors = append(migrationErrors, fmt.Errorf("index %s on %s uses unsupported MySQL index type %s", index.IndexName, tableName, index.IndexType))
			continue
		}

		indexColumns := make([]string, len(index.Columns))
		for i, column := range index.Columns {
			columnSQL, err := postgresIndexColumnSQL(column)
			if err != nil {
				migrationErrors = append(migrationErrors, fmt.Errorf("convert index %s column %s: %w", index.IndexName, column.Name, err))
				continue
			}
			indexColumns[i] = columnSQL
		}
		if len(indexColumns) == 0 || slicesContainEmpty(indexColumns) {
			continue
		}
		postgresName := postgresIndexName(tableName, index.IndexName)

		// Create index SQL
		var indexSQL string
		if index.IsUnique {
			indexSQL = fmt.Sprintf(`CREATE UNIQUE INDEX IF NOT EXISTS %s ON %s.%s (%s)`,
				quotePostgresIdentifier(postgresName), quotePostgresIdentifier(sm.schema), quotePostgresIdentifier(tableName), strings.Join(indexColumns, ", "))
		} else {
			indexSQL = fmt.Sprintf(`CREATE INDEX IF NOT EXISTS %s ON %s.%s (%s)`,
				quotePostgresIdentifier(postgresName), quotePostgresIdentifier(sm.schema), quotePostgresIdentifier(tableName), strings.Join(indexColumns, ", "))
		}

		if _, err := sm.postgres.ExecContext(ctx, indexSQL); err != nil {
			fmt.Printf("⚠️  Failed to create index %s: %v\n", index.IndexName, err)
			migrationErrors = append(migrationErrors, fmt.Errorf("create index %s: %w", index.IndexName, err))
			continue
		}
		if index.IndexComment != "" {
			commentSQL := fmt.Sprintf(`COMMENT ON INDEX %s.%s IS %s`,
				quotePostgresIdentifier(sm.schema), quotePostgresIdentifier(postgresName), quotePostgresLiteral(index.IndexComment))
			if _, err := sm.postgres.ExecContext(ctx, commentSQL); err != nil {
				migrationErrors = append(migrationErrors, fmt.Errorf("comment on index %s: %w", postgresName, err))
				continue
			}
		}

		fmt.Printf("✅ Created index: %s on %s.%s\n", postgresName, sm.schema, tableName)
	}

	return errors.Join(migrationErrors...)
}

func postgresIndexColumnSQL(column IndexColumnInfo) (string, error) {
	if column.Name == "" {
		return "", fmt.Errorf("column name cannot be empty")
	}
	columnSQL := quotePostgresIdentifier(column.Name)
	if column.PrefixLength < 0 {
		return "", fmt.Errorf("prefix length cannot be negative")
	}
	if column.PrefixLength > 0 {
		columnSQL = fmt.Sprintf("substring(%s FROM 1 FOR %d)", columnSQL, column.PrefixLength)
	}

	switch strings.ToUpper(column.SortDirection) {
	case "", "A":
	case "D":
		columnSQL += " DESC"
	default:
		return "", fmt.Errorf("unsupported sort direction %q", column.SortDirection)
	}
	return columnSQL, nil
}

func postgresIndexName(tableName, indexName string) string {
	candidate := tableName + "_" + indexName
	return hashedPostgresObjectName(candidate, tableName+"\x00"+indexName)
}

func slicesContainEmpty(values []string) bool {
	for _, value := range values {
		if value == "" {
			return true
		}
	}
	return false
}

// getMySQLForeignKeys retrieves foreign keys for a table from MySQL
func (sm *SchemaMigrator) getMySQLForeignKeys(ctx context.Context, tableName string) ([]ForeignKeyInfo, error) {
	// First, get foreign key information from KEY_COLUMN_USAGE
	query := `
		SELECT 
			CONSTRAINT_NAME,
			COLUMN_NAME,
			REFERENCED_TABLE_SCHEMA,
			REFERENCED_TABLE_NAME,
			REFERENCED_COLUMN_NAME,
			DATABASE()
		FROM INFORMATION_SCHEMA.KEY_COLUMN_USAGE 
		WHERE TABLE_SCHEMA = DATABASE() 
		AND TABLE_NAME = ?
		AND REFERENCED_TABLE_NAME IS NOT NULL
		ORDER BY CONSTRAINT_NAME, ORDINAL_POSITION
	`

	rows, err := sm.mysql.QueryContext(ctx, query, tableName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var foreignKeys []ForeignKeyInfo
	foreignKeyPositions := make(map[string]int)
	for rows.Next() {
		var constraintName, columnName, referencedSchema, referencedTable, referencedColumn, sourceSchema string
		if err := rows.Scan(&constraintName, &columnName, &referencedSchema, &referencedTable, &referencedColumn, &sourceSchema); err != nil {
			return nil, err
		}
		if !strings.EqualFold(referencedSchema, sourceSchema) {
			return nil, fmt.Errorf("foreign key %s on %s references MySQL database %s; cross-database references are unsupported", constraintName, tableName, referencedSchema)
		}

		if position, exists := foreignKeyPositions[constraintName]; exists {
			foreignKeys[position].ColumnNames = append(foreignKeys[position].ColumnNames, columnName)
			foreignKeys[position].ReferencedColumns = append(foreignKeys[position].ReferencedColumns, referencedColumn)
			continue
		}

		deleteRule, updateRule, err := sm.getForeignKeyRules(ctx, constraintName)
		if err != nil {
			return nil, err
		}
		foreignKeyPositions[constraintName] = len(foreignKeys)
		foreignKeys = append(foreignKeys, ForeignKeyInfo{
			ConstraintName:    constraintName,
			TableName:         tableName,
			ColumnNames:       []string{columnName},
			ReferencedTable:   referencedTable,
			ReferencedColumns: []string{referencedColumn},
			OnDelete:          deleteRule,
			OnUpdate:          updateRule,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	return foreignKeys, nil
}

// getForeignKeyRules retrieves the DELETE and UPDATE rules for a foreign key constraint
func (sm *SchemaMigrator) getForeignKeyRules(ctx context.Context, constraintName string) (string, string, error) {
	query := `
		SELECT 
			DELETE_RULE,
			UPDATE_RULE
		FROM INFORMATION_SCHEMA.REFERENTIAL_CONSTRAINTS 
		WHERE CONSTRAINT_SCHEMA = DATABASE() 
		AND CONSTRAINT_NAME = ?
	`

	var deleteRule, updateRule string
	err := sm.mysql.QueryRowContext(ctx, query, constraintName).Scan(&deleteRule, &updateRule)
	if err != nil {
		return "", "", fmt.Errorf("get rules for foreign key %s: %w", constraintName, err)
	}

	deleteRule, err = normalizeForeignKeyRule(deleteRule)
	if err != nil {
		return "", "", fmt.Errorf("foreign key %s delete rule: %w", constraintName, err)
	}
	updateRule, err = normalizeForeignKeyRule(updateRule)
	if err != nil {
		return "", "", fmt.Errorf("foreign key %s update rule: %w", constraintName, err)
	}
	return deleteRule, updateRule, nil
}

func normalizeForeignKeyRule(rule string) (string, error) {
	normalized := strings.ToUpper(strings.TrimSpace(rule))
	switch normalized {
	case "NO ACTION", "RESTRICT", "CASCADE", "SET NULL", "SET DEFAULT":
		return normalized, nil
	default:
		return "", fmt.Errorf("unsupported rule %q", rule)
	}
}

// migrateForeignKeys migrates foreign keys for a table
func (sm *SchemaMigrator) migrateForeignKeys(ctx context.Context, tableName string) error {
	foreignKeys, err := sm.getMySQLForeignKeys(ctx, tableName)
	if err != nil {
		return err
	}

	if len(foreignKeys) == 0 {
		fmt.Printf("ℹ️  No foreign keys found for table: %s\n", tableName)
		return nil
	}

	fmt.Printf("🔧 Migrating %d foreign keys for table: %s\n", len(foreignKeys), tableName)
	var migrationErrors []error

	for _, fk := range foreignKeys {
		constraintName := boundedPostgresObjectName(fk.ConstraintName, fk.TableName+"\x00"+fk.ConstraintName)
		// Check if the referenced table exists in PostgreSQL
		referencedTableExists, err := sm.tableExistsInPostgres(ctx, fk.ReferencedTable)
		if err != nil {
			fmt.Printf("⚠️  Could not check if referenced table %s exists: %v\n", fk.ReferencedTable, err)
			migrationErrors = append(migrationErrors, fmt.Errorf("check referenced table %s: %w", fk.ReferencedTable, err))
			continue
		}

		if !referencedTableExists {
			fmt.Printf("⚠️  Referenced table %s does not exist, skipping foreign key %s\n", fk.ReferencedTable, fk.ConstraintName)
			migrationErrors = append(migrationErrors, fmt.Errorf("referenced table %s does not exist for foreign key %s", fk.ReferencedTable, fk.ConstraintName))
			continue
		}

		var constraintExists bool
		err = sm.postgres.QueryRowContext(ctx, `
			SELECT EXISTS(
				SELECT 1
				FROM pg_constraint constraint_info
				JOIN pg_class table_info ON table_info.oid = constraint_info.conrelid
				JOIN pg_namespace schema_info ON schema_info.oid = table_info.relnamespace
				WHERE constraint_info.conname = $1
				AND schema_info.nspname = $2
				AND table_info.relname = $3
			)
		`, constraintName, sm.schema, fk.TableName).Scan(&constraintExists)
		if err != nil {
			return fmt.Errorf("check foreign key %s: %w", constraintName, err)
		}
		if constraintExists {
			fmt.Printf("⏭️  Foreign key %s already exists\n", constraintName)
			continue
		}

		quotedColumns := make([]string, len(fk.ColumnNames))
		quotedReferencedColumns := make([]string, len(fk.ReferencedColumns))
		for i, column := range fk.ColumnNames {
			quotedColumns[i] = quotePostgresIdentifier(column)
		}
		for i, column := range fk.ReferencedColumns {
			quotedReferencedColumns[i] = quotePostgresIdentifier(column)
		}

		// Build foreign key constraint SQL
		constraintSQL := fmt.Sprintf(`ALTER TABLE %s.%s ADD CONSTRAINT %s FOREIGN KEY (%s) REFERENCES %s.%s (%s)`,
			quotePostgresIdentifier(sm.schema), quotePostgresIdentifier(fk.TableName), quotePostgresIdentifier(constraintName), strings.Join(quotedColumns, ", "),
			quotePostgresIdentifier(sm.schema), quotePostgresIdentifier(fk.ReferencedTable), strings.Join(quotedReferencedColumns, ", "))

		// Add ON DELETE and ON UPDATE clauses
		if fk.OnDelete != "NO ACTION" && fk.OnDelete != "" {
			constraintSQL += fmt.Sprintf(" ON DELETE %s", fk.OnDelete)
		}
		if fk.OnUpdate != "NO ACTION" && fk.OnUpdate != "" {
			constraintSQL += fmt.Sprintf(" ON UPDATE %s", fk.OnUpdate)
		}

		if _, err := sm.postgres.ExecContext(ctx, constraintSQL); err != nil {
			fmt.Printf("⚠️  Failed to create foreign key %s: %v\n", constraintName, err)
			migrationErrors = append(migrationErrors, fmt.Errorf("create foreign key %s: %w", constraintName, err))
			continue
		}

		fmt.Printf("✅ Created foreign key: %s on %s.%s\n", constraintName, sm.schema, fk.TableName)
	}

	return errors.Join(migrationErrors...)
}

// getMySQLTriggers retrieves triggers for a table from MySQL
func (sm *SchemaMigrator) getMySQLTriggers(ctx context.Context, tableName string) ([]TriggerInfo, error) {
	query := `
		SELECT TRIGGER_NAME
		FROM INFORMATION_SCHEMA.TRIGGERS 
		WHERE TRIGGER_SCHEMA = DATABASE() 
		AND EVENT_OBJECT_TABLE = ?
	`

	rows, err := sm.mysql.QueryContext(ctx, query, tableName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var triggers []TriggerInfo
	for rows.Next() {
		var triggerName string
		if err := rows.Scan(&triggerName); err != nil {
			return nil, err
		}

		triggers = append(triggers, TriggerInfo{
			TriggerName: triggerName,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	return triggers, nil
}

// migrateTriggers migrates triggers for a table
func (sm *SchemaMigrator) migrateTriggers(ctx context.Context, tableName string) error {
	triggers, err := sm.getMySQLTriggers(ctx, tableName)
	if err != nil {
		return err
	}

	for _, trigger := range triggers {
		fmt.Printf("⚠️  Skipping trigger %s on %s: automatic MySQL procedural SQL conversion is not yet safe\n", trigger.TriggerName, tableName)
	}

	return nil
}

// getMySQLFunctions retrieves all functions from MySQL
func (sm *SchemaMigrator) getMySQLFunctions(ctx context.Context) ([]FunctionInfo, error) {
	query := `
		SELECT ROUTINE_NAME
		FROM INFORMATION_SCHEMA.ROUTINES 
		WHERE ROUTINE_SCHEMA = DATABASE() 
		AND ROUTINE_TYPE = 'FUNCTION'
	`

	rows, err := sm.mysql.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var functions []FunctionInfo
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}

		functions = append(functions, FunctionInfo{
			FunctionName: name,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	return functions, nil
}

// getMySQLViews retrieves all views from MySQL
func (sm *SchemaMigrator) getMySQLViews(ctx context.Context) ([]ViewInfo, error) {
	query := `
		SELECT TABLE_NAME
		FROM INFORMATION_SCHEMA.VIEWS 
		WHERE TABLE_SCHEMA = DATABASE()
	`

	rows, err := sm.mysql.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var views []ViewInfo
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}

		views = append(views, ViewInfo{
			ViewName: name,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	return views, nil
}

// tableExistsInPostgres checks if a table exists in PostgreSQL
func (sm *SchemaMigrator) tableExistsInPostgres(ctx context.Context, tableName string) (bool, error) {

	var exists bool
	err := sm.postgres.QueryRowContext(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM information_schema.tables 
			WHERE table_schema = $1 AND table_name = $2
		)
	`, sm.schema, tableName).Scan(&exists)

	if err != nil {
		return false, fmt.Errorf("failed to check if table %s exists: %w", tableName, err)
	}

	return exists, nil
}
