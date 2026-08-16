package migrator

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/jmoiron/sqlx"
	"golang.org/x/sync/errgroup"
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

// ManualStatement is DDL that pgstream generated but deliberately did not
// execute, together with the reason it needs a human decision.
type ManualStatement struct {
	Kind   string `json:"kind"`
	Table  string `json:"table"`
	Name   string `json:"name"`
	SQL    string `json:"sql"`
	Reason string `json:"reason"`
}

// SchemaMigrator handles migration of database schema objects
type SchemaMigrator struct {
	mysql    *sqlx.DB
	postgres *sqlx.DB
	schema   string
	sink     EventSink
	manualMu sync.Mutex
	manual   []ManualStatement
}

// appendManual records DDL for manual execution; safe from concurrent
// per-table schema workers.
func (sm *SchemaMigrator) appendManual(statement ManualStatement) {
	sm.manualMu.Lock()
	sm.manual = append(sm.manual, statement)
	sm.manualMu.Unlock()
}

// ManualStatements returns the DDL collected for manual execution during
// schema object migration.
func (sm *SchemaMigrator) ManualStatements() []ManualStatement {
	sm.manualMu.Lock()
	defer sm.manualMu.Unlock()
	return slices.Clone(sm.manual)
}

// NewSchemaMigrator creates a new schema migrator
func NewSchemaMigrator(mysql, postgres *sqlx.DB, schema string) *SchemaMigrator {
	return &SchemaMigrator{
		mysql:    mysql,
		postgres: postgres,
		schema:   schema,
	}
}

// MigrateAllFunctions migrates all functions from MySQL to PostgreSQL
func (sm *SchemaMigrator) MigrateAllFunctions(ctx context.Context) error {
	sm.logf("🔧 Starting function migration\n")

	functions, err := sm.getMySQLFunctions(ctx)
	if err != nil {
		return fmt.Errorf("failed to get MySQL functions: %w", err)
	}

	for _, function := range functions {
		sm.warnf("⚠️  Skipping function %s: automatic MySQL procedural SQL conversion is not yet safe\n", function.FunctionName)
	}

	return nil
}

// MigrateAllViews migrates all views from MySQL to PostgreSQL
func (sm *SchemaMigrator) MigrateAllViews(ctx context.Context) error {
	sm.logf("🔧 Discovering views\n")

	views, err := sm.getMySQLViews(ctx)
	if err != nil {
		return fmt.Errorf("failed to get MySQL views: %w", err)
	}

	for _, view := range views {
		sm.warnf("⚠️  Skipping view %s: automatic MySQL SQL conversion is not yet safe\n", view.ViewName)
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
			sm.warnf("⚠️  Failed to create index %s: %v\n", index.IndexName, err)
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

		sm.logf("✅ Created index: %s on %s.%s\n", postgresName, sm.schema, tableName)
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
		return nil
	}

	sm.logf("🔧 Migrating %d foreign keys for table: %s\n", len(foreignKeys), tableName)
	var migrationErrors []error

	for _, fk := range foreignKeys {
		constraintName := boundedPostgresObjectName(fk.ConstraintName, fk.TableName+"\x00"+fk.ConstraintName)

		// A foreign key whose target is missing from the SOURCE database
		// (a partial backup or restore) is unenforceable anywhere; MySQL
		// itself only holds it as metadata. Report it for manual work
		// instead of failing the migration.
		existsInSource, err := sm.tableExistsInMySQL(ctx, fk.ReferencedTable)
		if err != nil {
			migrationErrors = append(migrationErrors, fmt.Errorf("check source table %s: %w", fk.ReferencedTable, err))
			continue
		}
		if !existsInSource {
			sm.warnf("⚠️  Skipping foreign key %s on %s: referenced table %s does not exist in the source database (partial backup?); its DDL is saved for manual execution after restoring %s", fk.ConstraintName, fk.TableName, fk.ReferencedTable, fk.ReferencedTable)
			sm.appendManual(ManualStatement{
				Kind:   "foreign_key",
				Table:  fk.TableName,
				Name:   constraintName,
				SQL:    sm.buildForeignKeySQL(fk, constraintName),
				Reason: fmt.Sprintf("referenced table %s does not exist in the source database (partial backup); run after restoring and migrating %s", fk.ReferencedTable, fk.ReferencedTable),
			})
			continue
		}

		// Check if the referenced table exists in PostgreSQL
		referencedTableExists, err := sm.tableExistsInPostgres(ctx, fk.ReferencedTable)
		if err != nil {
			sm.warnf("⚠️  Could not check if referenced table %s exists: %v\n", fk.ReferencedTable, err)
			migrationErrors = append(migrationErrors, fmt.Errorf("check referenced table %s: %w", fk.ReferencedTable, err))
			continue
		}

		if !referencedTableExists {
			sm.warnf("⚠️  Referenced table %s does not exist, skipping foreign key %s\n", fk.ReferencedTable, fk.ConstraintName)
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
			sm.logf("⏭️  Foreign key %s already exists\n", constraintName)
			continue
		}

		// Incompatible column types make the constraint impossible to
		// create; surface an actionable error instead of PostgreSQL's
		// generic one. This typically happens when the referenced table is
		// restored and migrated after a partial migration already created
		// the referencing columns with a different type mapping.
		if err := sm.validateForeignKeyColumnTypes(ctx, fk); err != nil {
			sm.warnf("⚠️  Cannot create foreign key %s: %v", fk.ConstraintName, err)
			migrationErrors = append(migrationErrors, fmt.Errorf("foreign key %s: %w", fk.ConstraintName, err))
			continue
		}

		// Orphaned child rows (inserted with FOREIGN_KEY_CHECKS=0) make the
		// constraint uncreatable until the data is repaired; skip it with
		// its DDL saved for manual work instead of failing the migration.
		orphans, err := sm.countOrphanRows(ctx, fk)
		if err != nil {
			migrationErrors = append(migrationErrors, fmt.Errorf("check foreign key %s for orphaned rows: %w", fk.ConstraintName, err))
			continue
		}
		if orphans > 0 {
			sm.warnf("⚠️  Skipping foreign key %s on %s: %d child rows have no matching parent in %s (inserted with FOREIGN_KEY_CHECKS=0?); repair them, then run the saved DDL or resume the session", fk.ConstraintName, fk.TableName, orphans, fk.ReferencedTable)
			sm.appendManual(ManualStatement{
				Kind:  "foreign_key",
				Table: fk.TableName,
				Name:  constraintName,
				SQL:   sm.buildForeignKeySQL(fk, constraintName),
				Reason: fmt.Sprintf(
					"%d rows in %s reference values missing from %s (find them with: SELECT child.* FROM %s child LEFT JOIN %s parent ON child.%s = parent.%s WHERE parent.%s IS NULL AND child.%s IS NOT NULL); repair or delete them, then run this statement",
					orphans, fk.TableName, fk.ReferencedTable,
					quoteMySQLIdentifier(fk.TableName), quoteMySQLIdentifier(fk.ReferencedTable),
					quoteMySQLIdentifier(fk.ColumnNames[0]), quoteMySQLIdentifier(fk.ReferencedColumns[0]),
					quoteMySQLIdentifier(fk.ReferencedColumns[0]), quoteMySQLIdentifier(fk.ColumnNames[0]),
				),
			})
			continue
		}

		constraintSQL := sm.buildForeignKeySQL(fk, constraintName)

		if _, err := sm.postgres.ExecContext(ctx, constraintSQL); err != nil {
			sm.warnf("⚠️  Failed to create foreign key %s: %v\n", constraintName, err)
			migrationErrors = append(migrationErrors, fmt.Errorf("create foreign key %s: %w", constraintName, err))
			continue
		}

		sm.logf("✅ Created foreign key: %s on %s.%s\n", constraintName, sm.schema, fk.TableName)
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
		sm.warnf("⚠️  Skipping trigger %s on %s: automatic MySQL procedural SQL conversion is not yet safe\n", trigger.TriggerName, tableName)
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

// countOrphanRows counts child rows that violate the foreign key in the
// source itself (inserted under FOREIGN_KEY_CHECKS=0, common in restores).
// MySQL keeps the constraint as metadata over such rows, but PostgreSQL
// refuses to create it, so orphans must be found before attempting.
func (sm *SchemaMigrator) countOrphanRows(ctx context.Context, fk ForeignKeyInfo) (int64, error) {
	joins := make([]string, len(fk.ColumnNames))
	notNulls := make([]string, len(fk.ColumnNames))
	for i := range fk.ColumnNames {
		joins[i] = fmt.Sprintf("child.%s = parent.%s", quoteMySQLIdentifier(fk.ColumnNames[i]), quoteMySQLIdentifier(fk.ReferencedColumns[i]))
		notNulls[i] = fmt.Sprintf("child.%s IS NOT NULL", quoteMySQLIdentifier(fk.ColumnNames[i]))
	}
	query := fmt.Sprintf(
		"SELECT COUNT(*) FROM %s child LEFT JOIN %s parent ON %s WHERE parent.%s IS NULL AND %s",
		quoteMySQLIdentifier(fk.TableName), quoteMySQLIdentifier(fk.ReferencedTable),
		strings.Join(joins, " AND "),
		quoteMySQLIdentifier(fk.ReferencedColumns[0]),
		strings.Join(notNulls, " AND "),
	)
	var count int64
	if err := sm.mysql.QueryRowContext(ctx, query).Scan(&count); err != nil {
		return 0, err
	}
	return count, nil
}

// buildForeignKeySQL renders the ALTER TABLE statement that creates one
// foreign key constraint in the target schema.
func (sm *SchemaMigrator) buildForeignKeySQL(fk ForeignKeyInfo, constraintName string) string {
	quotedColumns := make([]string, len(fk.ColumnNames))
	quotedReferencedColumns := make([]string, len(fk.ReferencedColumns))
	for i, column := range fk.ColumnNames {
		quotedColumns[i] = quotePostgresIdentifier(column)
	}
	for i, column := range fk.ReferencedColumns {
		quotedReferencedColumns[i] = quotePostgresIdentifier(column)
	}

	constraintSQL := fmt.Sprintf(`ALTER TABLE %s.%s ADD CONSTRAINT %s FOREIGN KEY (%s) REFERENCES %s.%s (%s)`,
		quotePostgresIdentifier(sm.schema), quotePostgresIdentifier(fk.TableName), quotePostgresIdentifier(constraintName), strings.Join(quotedColumns, ", "),
		quotePostgresIdentifier(sm.schema), quotePostgresIdentifier(fk.ReferencedTable), strings.Join(quotedReferencedColumns, ", "))

	if fk.OnDelete != "NO ACTION" && fk.OnDelete != "" {
		constraintSQL += fmt.Sprintf(" ON DELETE %s", fk.OnDelete)
	}
	if fk.OnUpdate != "NO ACTION" && fk.OnUpdate != "" {
		constraintSQL += fmt.Sprintf(" ON UPDATE %s", fk.OnUpdate)
	}
	return constraintSQL
}

// validateForeignKeyColumnTypes checks that each referencing column's target
// type matches the referenced column's, and suggests the explicit cast when
// the mismatch is the common uuid/varchar split from a staged partial
// migration.
func (sm *SchemaMigrator) validateForeignKeyColumnTypes(ctx context.Context, fk ForeignKeyInfo) error {
	columnType := func(table, column string) (string, error) {
		var udtName string
		err := sm.postgres.QueryRowContext(ctx, `
			SELECT udt_name FROM information_schema.columns
			WHERE table_schema = $1 AND table_name = $2 AND LOWER(column_name) = LOWER($3)
		`, sm.schema, table, column).Scan(&udtName)
		if err != nil {
			return "", fmt.Errorf("read type of %s.%s: %w", table, column, err)
		}
		return udtName, nil
	}

	for i := range fk.ColumnNames {
		if i >= len(fk.ReferencedColumns) {
			break
		}
		referencingType, err := columnType(fk.TableName, fk.ColumnNames[i])
		if err != nil {
			return err
		}
		referencedType, err := columnType(fk.ReferencedTable, fk.ReferencedColumns[i])
		if err != nil {
			return err
		}
		if referencingType == referencedType {
			continue
		}

		message := fmt.Sprintf(
			"column %s.%s has type %s but referenced column %s.%s has type %s; the target tables were created with diverging type decisions (for example by an earlier pgstream version or a staged partial migration)",
			fk.TableName, fk.ColumnNames[i], referencingType,
			fk.ReferencedTable, fk.ReferencedColumns[i], referencedType,
		)
		if (referencingType == "varchar" && referencedType == "uuid") || (referencingType == "uuid" && referencedType == "varchar") {
			varcharTable, varcharColumn := fk.TableName, fk.ColumnNames[i]
			uuidTable, uuidColumn := fk.ReferencedTable, fk.ReferencedColumns[i]
			if referencingType == "uuid" {
				varcharTable, varcharColumn, uuidTable, uuidColumn = fk.ReferencedTable, fk.ReferencedColumns[i], fk.TableName, fk.ColumnNames[i]
			}
			message += fmt.Sprintf(
				`. Reconcile with one of: ALTER TABLE %s.%s ALTER COLUMN %s TYPE varchar(36) USING %s::text (always succeeds), or — only if every value is a canonical UUID — ALTER TABLE %s.%s ALTER COLUMN %s TYPE uuid USING %s::uuid`,
				quotePostgresIdentifier(sm.schema), quotePostgresIdentifier(uuidTable),
				quotePostgresIdentifier(uuidColumn), quotePostgresIdentifier(uuidColumn),
				quotePostgresIdentifier(sm.schema), quotePostgresIdentifier(varcharTable),
				quotePostgresIdentifier(varcharColumn), quotePostgresIdentifier(varcharColumn),
			)
		}
		return errors.New(message)
	}
	return nil
}

// reportAutoUpdateTimestampColumns finds MySQL ON UPDATE CURRENT_TIMESTAMP
// columns and emits the equivalent PostgreSQL trigger DDL for manual review.
// The trigger mirrors MySQL's semantics: the timestamp refreshes only when
// the UPDATE statement did not set the column explicitly.
func (sm *SchemaMigrator) reportAutoUpdateTimestampColumns(ctx context.Context, tableName string) error {
	rows, err := sm.mysql.QueryContext(ctx, `
		SELECT COLUMN_NAME
		FROM INFORMATION_SCHEMA.COLUMNS
		WHERE TABLE_SCHEMA = DATABASE()
		AND TABLE_NAME = ?
		AND LOWER(EXTRA) LIKE '%on update current_timestamp%'
		ORDER BY ORDINAL_POSITION
	`, tableName)
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

	for _, column := range columns {
		sm.warnf("⚠️  Column %s.%s uses MySQL ON UPDATE CURRENT_TIMESTAMP; PostgreSQL needs a trigger for that behavior. Its DDL was generated for manual review.", tableName, column)
		sm.appendManual(ManualStatement{
			Kind:   "on_update_timestamp",
			Table:  tableName,
			Name:   column,
			SQL:    sm.buildAutoUpdateTriggerSQL(tableName, column),
			Reason: fmt.Sprintf("MySQL refreshes %s.%s on every UPDATE (ON UPDATE CURRENT_TIMESTAMP); PostgreSQL expresses this with a trigger. Review and run to keep the behavior.", tableName, column),
		})
	}
	return nil
}

// buildAutoUpdateTriggerSQL renders the trigger function and trigger that
// reproduce MySQL's ON UPDATE CURRENT_TIMESTAMP for one column.
func (sm *SchemaMigrator) buildAutoUpdateTriggerSQL(tableName string, column string) string {
	functionName := hashedPostgresObjectName(
		fmt.Sprintf("pgstream_%s_%s_on_update_fn", tableName, column),
		tableName+"\x00"+column+"\x00on_update_fn",
	)
	triggerName := hashedPostgresObjectName(
		fmt.Sprintf("pgstream_%s_%s_on_update", tableName, column),
		tableName+"\x00"+column+"\x00on_update",
	)
	quotedSchema := quotePostgresIdentifier(sm.schema)
	quotedTable := quotePostgresIdentifier(tableName)
	quotedColumn := quotePostgresIdentifier(column)

	return fmt.Sprintf(`CREATE OR REPLACE FUNCTION %s.%s() RETURNS trigger LANGUAGE plpgsql AS $pgstream$
BEGIN
	IF NEW.%s IS NOT DISTINCT FROM OLD.%s THEN
		NEW.%s := CURRENT_TIMESTAMP;
	END IF;
	RETURN NEW;
END
$pgstream$;
CREATE TRIGGER %s BEFORE UPDATE ON %s.%s FOR EACH ROW EXECUTE FUNCTION %s.%s()`,
		quotedSchema, quotePostgresIdentifier(functionName),
		quotedColumn, quotedColumn, quotedColumn,
		quotePostgresIdentifier(triggerName), quotedSchema, quotedTable,
		quotedSchema, quotePostgresIdentifier(functionName),
	)
}

// tableExistsInMySQL checks if a base table exists in the source database.
func (sm *SchemaMigrator) tableExistsInMySQL(ctx context.Context, tableName string) (bool, error) {
	var exists bool
	err := sm.mysql.QueryRowContext(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM INFORMATION_SCHEMA.TABLES
			WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ? AND TABLE_TYPE = 'BASE TABLE'
		)
	`, tableName).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("failed to check if source table %s exists: %w", tableName, err)
	}
	return exists, nil
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

// migrateAllSchemaObjects migrates all schema objects (indexes, foreign keys, triggers, functions, views)
func (m *Migrator) migrateAllSchemaObjects(ctx context.Context, tables []string) error {
	m.logf("🔧 Migrating indexes and foreign keys for all tables...")
	var migrationErrors []error

	var errMu sync.Mutex
	collect := func(err error) {
		m.warnf("⚠️  %v\n", err)
		errMu.Lock()
		migrationErrors = append(migrationErrors, err)
		errMu.Unlock()
	}

	// Index creation dominates this phase and is safe to parallelize across
	// distinct tables, unlike foreign keys (concurrent ALTER TABLE on shared
	// referenced tables can deadlock). Errors are collected, not fail-fast,
	// matching the sequential behavior.
	indexGroup := &errgroup.Group{}
	indexGroup.SetLimit(max(m.workers, 1))
	for _, table := range tables {
		indexGroup.Go(func() error {
			if ctx.Err() != nil {
				return nil
			}
			if err := m.schemaMigrator.migrateIndexes(ctx, table); err != nil {
				collect(fmt.Errorf("failed to migrate indexes for %s: %w", table, err))
			}
			if err := m.schemaMigrator.migrateTriggers(ctx, table); err != nil {
				collect(fmt.Errorf("failed to migrate triggers for %s: %w", table, err))
			}
			if err := m.schemaMigrator.reportAutoUpdateTimestampColumns(ctx, table); err != nil {
				collect(fmt.Errorf("failed to inspect auto-update timestamp columns for %s: %w", table, err))
			}
			if err := m.reportGeneratedColumns(ctx, table); err != nil {
				collect(fmt.Errorf("failed to inspect generated columns for %s: %w", table, err))
			}
			return nil
		})
	}
	_ = indexGroup.Wait()

	// Foreign keys run sequentially: two concurrent ALTER TABLE ... ADD
	// CONSTRAINT statements referencing the same table can deadlock in
	// PostgreSQL.
	for _, table := range tables {
		if err := m.schemaMigrator.migrateForeignKeys(ctx, table); err != nil {
			collect(fmt.Errorf("failed to migrate foreign keys for %s: %w", table, err))
		}
	}

	// Migrate global objects (functions, views)
	m.logf("🔧 Migrating global schema objects (functions, views)...")
	if err := m.schemaMigrator.MigrateAllFunctions(ctx); err != nil {
		m.warnf("⚠️  Failed to migrate functions: %v\n", err)
		migrationErrors = append(migrationErrors, err)
	}

	if err := m.schemaMigrator.MigrateAllViews(ctx); err != nil {
		m.warnf("⚠️  Failed to migrate views: %v\n", err)
		migrationErrors = append(migrationErrors, err)
	}

	return errors.Join(migrationErrors...)
}

// reportGeneratedColumns emits manual-work DDL for MySQL generated columns.
// The migrated column is a plain column holding the computed snapshot
// values; restoring auto-computation requires translating the MySQL
// expression, so the template deliberately fails to parse until edited.
func (m *Migrator) reportGeneratedColumns(ctx context.Context, table string) error {
	columns, err := m.getMySQLTableStructure(ctx, table)
	if err != nil {
		return err
	}
	for _, col := range columns {
		if !isGeneratedColumn(col.Extra) {
			continue
		}
		pgType, err := m.convertMySQLTypeToPostgres(col.Type, col.Name, table)
		if err != nil {
			pgType = "/* TODO: choose type */"
		}
		if col.IsUUID {
			pgType = "UUID"
		}

		storage := "STORED"
		if strings.Contains(strings.ToLower(col.Extra), "virtual") {
			storage = "VIRTUAL"
		}
		m.warnf("⚠️  Column %s.%s is a MySQL %s generated column; it was migrated as a plain column holding the computed snapshot values. Translate its expression manually to restore auto-computation.", table, col.Name, storage)

		quotedSchema := quotePostgresIdentifier(m.schemaName)
		quotedTable := quotePostgresIdentifier(table)
		quotedColumn := quotePostgresIdentifier(col.Name)
		notNull := ""
		if !col.Nullable {
			notNull = " NOT NULL"
		}
		sql := fmt.Sprintf(`ALTER TABLE %s.%s DROP COLUMN %s;
ALTER TABLE %s.%s ADD COLUMN %s %s GENERATED ALWAYS AS (/* TODO translate from MySQL: %s */) STORED%s`,
			quotedSchema, quotedTable, quotedColumn,
			quotedSchema, quotedTable, quotedColumn, pgType,
			strings.ReplaceAll(col.GenerationExpression, "*/", "* /"), notNull,
		)
		m.schemaMigrator.appendManual(ManualStatement{
			Kind:  "generated_column",
			Table: table,
			Name:  col.Name,
			SQL:   sql,
			Reason: fmt.Sprintf(
				"MySQL computes %s.%s as a %s generated column (%s); the expression cannot be translated automatically. The migrated column holds the snapshot values; edit the expression below, then run to restore auto-computation. PostgreSQL only supports STORED generation.",
				table, col.Name, storage, col.GenerationExpression,
			),
		})
	}
	return nil
}
