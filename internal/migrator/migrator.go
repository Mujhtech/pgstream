package migrator

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	"github.com/mujhtech/pgstream/internal/database"
	"github.com/mujhtech/pgstream/internal/storage"
	"github.com/mujhtech/pgstream/internal/utils/views"
)

type Migrator struct {
	mysql          *database.Database
	postgres       *database.Database
	storage        *storage.Storage
	sessionId      string
	schemaMigrator *SchemaMigrator
	schemaName     string
	batchSize      int
	freshSession   bool
	sourceTx       *sqlx.Tx
	metadataCache  map[string]*tableValidationMetadata
}

const defaultBatchSize = 5000

type Option func(*Migrator) error

func WithBatchSize(batchSize int) Option {
	return func(migrator *Migrator) error {
		if batchSize <= 0 {
			return fmt.Errorf("batch size must be greater than zero")
		}
		migrator.batchSize = batchSize
		return nil
	}
}

func WithFreshSession(fresh bool) Option {
	return func(migrator *Migrator) error {
		migrator.freshSession = fresh
		return nil
	}
}

type mysqlQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

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

func New(mysql *database.Database, postgres *database.Database, storage *storage.Storage, sessionId string, options ...Option) (*Migrator, error) {
	if mysql == nil || postgres == nil || storage == nil {
		return nil, fmt.Errorf("MySQL, PostgreSQL, and metadata storage connections are required")
	}
	if sessionId == "" {
		return nil, fmt.Errorf("session ID cannot be empty")
	}

	schemaName := postgres.GetSchema()
	if schemaName == "" {
		schemaName = "public"
	}
	if err := validatePostgresIdentifier(schemaName, "target schema name"); err != nil {
		return nil, err
	}

	schemaMigrator := NewSchemaMigrator(mysql.GetDB(), postgres.GetDB(), schemaName)
	migrator := &Migrator{
		mysql:          mysql,
		postgres:       postgres,
		storage:        storage,
		sessionId:      sessionId,
		schemaMigrator: schemaMigrator,
		schemaName:     schemaName,
		batchSize:      defaultBatchSize,
		metadataCache:  make(map[string]*tableValidationMetadata),
	}
	for _, option := range options {
		if err := option(migrator); err != nil {
			return nil, err
		}
	}
	return migrator, nil
}

func (m *Migrator) Start(ctx context.Context) error {
	fmt.Println("🚀 Starting MySQL to PostgreSQL migration...")

	// Step 1: Ensure schema exists
	if err := m.ensureSchemaExists(ctx); err != nil {
		return fmt.Errorf("failed to ensure schema exists: %w", err)
	}

	// Step 2: Get all tables from MySQL
	tables, err := m.getMySQLTables(ctx)
	if err != nil {
		return fmt.Errorf("failed to get MySQL tables: %w", err)
	}
	for _, table := range tables {
		if err := validatePostgresIdentifier(table, "source table name"); err != nil {
			return err
		}
	}
	if err := m.validateSourceForeignKeyScope(ctx); err != nil {
		return err
	}

	fmt.Printf("📋 Found %d tables to migrate\n", len(tables))

	// Step 3: Create all tables in PostgreSQL (structure only)
	fmt.Println("🏗️  Step 1: Creating table structures...")
	if err := m.createAllTables(ctx, tables); err != nil {
		return fmt.Errorf("failed to create tables: %w", err)
	}
	if m.freshSession {
		if err := m.validateFreshTargetTables(ctx, tables); err != nil {
			return fmt.Errorf("validate fresh target: %w", err)
		}
	}

	// Step 4: Migrate data before secondary indexes, foreign keys, and triggers.
	// This keeps bulk loading fast and prevents target-side behavior from mutating source data.
	fmt.Println("📦 Step 2: Migrating data...")
	if err := m.beginSourceSnapshot(ctx); err != nil {
		return fmt.Errorf("begin consistent MySQL snapshot: %w", err)
	}
	if err := m.migrateAllData(ctx, tables); err != nil {
		m.rollbackSourceSnapshot()
		return fmt.Errorf("failed to migrate data: %w", err)
	}
	if err := m.commitSourceSnapshot(); err != nil {
		return fmt.Errorf("finish consistent MySQL snapshot: %w", err)
	}

	// Step 5: Create schema objects after the data is in place.
	fmt.Println("🔧 Step 3: Migrating schema objects...")
	if err := m.migrateAllSchemaObjects(ctx, tables); err != nil {
		return fmt.Errorf("failed to migrate schema objects: %w", err)
	}

	fmt.Println("✅ Migration completed successfully!")
	return nil
}

func (m *Migrator) sourceQueryer() mysqlQueryer {
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
		fmt.Printf("✅ Created schema: %s\n", m.schemaName)
	} else {
		fmt.Printf("✅ Schema exists: %s\n", m.schemaName)
	}

	return nil
}

func (m *Migrator) MigrateTable(ctx context.Context, table string, batchSize int) error {
	if batchSize <= 0 {
		return fmt.Errorf("batch size must be greater than zero")
	}
	if err := validatePostgresIdentifier(table, "source table name"); err != nil {
		return err
	}

	// Check if table exists in PostgreSQL
	tableExists, err := m.tableExistsInPostgres(ctx, table)
	if err != nil {
		return fmt.Errorf("failed to check if table exists: %w", err)
	}

	if !tableExists {
		// Create the table if it doesn't exist
		err := m.createTableInPostgres(ctx, table)
		if err != nil {
			return fmt.Errorf("failed to create table %s: %w", table, err)
		}
		tableExists = true // Now it exists
	}

	// Get MySQL column names
	mysqlColumns, err := m.getColumnNames(ctx, table)
	if err != nil {
		return err
	}

	// Get PostgreSQL column names
	postgresColumns, err := m.getPostgresColumnNames(ctx, table)
	if err != nil {
		return err
	}

	// Map MySQL columns to PostgreSQL columns
	mappedColumns, err := m.mapColumnsToPostgres(mysqlColumns, postgresColumns)
	if err != nil {
		return fmt.Errorf("failed to map columns for table %s: %w", table, err)
	}

	// InnoDB COUNT(*) scans the whole table. Use its metadata estimate for progress
	// and let the keyset loop determine when the copy is complete.
	var estimatedRowCount sql.NullInt64
	row := m.sourceQueryer().QueryRowContext(ctx, `
		SELECT TABLE_ROWS
		FROM INFORMATION_SCHEMA.TABLES
		WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ?
	`, table)
	if err := row.Scan(&estimatedRowCount); err != nil {
		return fmt.Errorf("estimate rows in %s: %w", table, err)
	}
	rowCount := estimatedRowCount.Int64

	fmt.Println(lipgloss.Place(0, 0, 0, 0,
		views.BasicLayout.Render(fmt.Sprintf("📦 Migrating table %s (approximately %d rows)...", table, rowCount)),
	))

	// Get current migration state to resume from last offset
	state, err := m.storage.GetMigration(ctx, m.sessionId, table)
	if err != nil {
		return fmt.Errorf("failed to get migration state: %w", err)
	}

	processedRows := int64(0)
	var cursor []any
	if state != nil {
		processedRows = state.LastOffset
		cursor, err = decodeCursor(state.LastCursor)
		if err != nil {
			return fmt.Errorf("decode resume cursor for %s: %w", table, err)
		}
		if processedRows > 0 {
			fmt.Printf("🔄 Resuming migration after %d rows\n", processedRows)
		}
	}

	primaryKeyColumns, err := m.getMySQLPrimaryKeyColumns(ctx, table)
	if err != nil {
		return fmt.Errorf("get primary key for %s: %w", table, err)
	}
	mappedPrimaryKeyColumns, err := m.mapColumnsToPostgres(primaryKeyColumns, postgresColumns)
	if err != nil {
		return fmt.Errorf("map primary key columns for %s: %w", table, err)
	}
	if len(primaryKeyColumns) == 0 {
		if processedRows > 0 {
			return fmt.Errorf("table %s has no primary key and cannot be resumed safely after %d rows; start a fresh session against an empty target", table, processedRows)
		}
		fmt.Printf("⚠️  Table %s has no primary key; using one streaming source scan with bounded insert batches. This table cannot be resumed after interruption.\n", table)
		if err := m.migrateKeylessTable(ctx, table, mysqlColumns, mappedColumns, rowCount, batchSize); err != nil {
			return err
		}
		if err := m.syncIdentitySequences(ctx, table); err != nil {
			return fmt.Errorf("synchronize identity sequences for %s: %w", table, err)
		}
		return nil
	}

	for {
		query, queryArgs, err := buildMySQLBatchQuery(table, mysqlColumns, primaryKeyColumns, cursor, processedRows, batchSize)
		if err != nil {
			return fmt.Errorf("build source query for %s: %w", table, err)
		}
		rows, err := m.sourceQueryer().QueryContext(ctx, query, queryArgs...)
		if err != nil {
			return fmt.Errorf("read source batch from %s: %w", table, err)
		}

		values := make([][]any, 0, batchSize)
		for rows.Next() {
			cols := make([]any, len(mysqlColumns))
			colPtrs := make([]any, len(mysqlColumns))
			for i := range cols {
				colPtrs[i] = &cols[i]
			}
			if err := rows.Scan(colPtrs...); err != nil {
				_ = rows.Close()
				return fmt.Errorf("scan source row from %s: %w", table, err)
			}
			values = append(values, cols)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return fmt.Errorf("iterate source rows from %s: %w", table, err)
		}
		if err := rows.Close(); err != nil {
			return fmt.Errorf("close source rows for %s: %w", table, err)
		}
		if len(values) == 0 {
			break
		}

		// Build INSERT using mapped PostgreSQL column names
		transformedValues, err := m.validateAndTransformData(ctx, table, mappedColumns, values)
		if err != nil {
			return fmt.Errorf("validate/transform data for %s: %w", table, err)
		}

		if err := m.bulkInsertPostgres(ctx, table, mappedColumns, mappedPrimaryKeyColumns, transformedValues); err != nil {
			return err
		}

		processedRows += int64(len(values))
		lastCursor := ""
		if len(primaryKeyColumns) > 0 {
			cursor, err = extractCursor(values[len(values)-1], mysqlColumns, primaryKeyColumns)
			if err != nil {
				return fmt.Errorf("capture resume cursor for %s: %w", table, err)
			}
			lastCursor, err = encodeCursor(cursor)
			if err != nil {
				return fmt.Errorf("encode resume cursor for %s: %w", table, err)
			}
		}

		if err := m.storage.UpsertMigration(ctx, storage.MigrationRecord{
			SessionId:    m.sessionId,
			TableName:    table,
			Status:       "in_progress",
			LastOffset:   processedRows,
			LastCursor:   lastCursor,
			RowCount:     rowCount,
			ErrorMessage: "",
		}); err != nil {
			return fmt.Errorf("checkpoint migration for %s: %w", table, err)
		}

		progress := fmt.Sprintf("%d rows", processedRows)
		if rowCount > 0 {
			progress = fmt.Sprintf("%d/~%d rows", processedRows, rowCount)
		}
		fmt.Println(lipgloss.Place(0, 0, 0, 0,
			views.BasicLayout.Render(fmt.Sprintf("✅ Inserted %d rows into %s (progress: %s)", len(values), table, progress)),
		))

		if len(values) < batchSize {
			break
		}
	}

	if err := m.syncIdentitySequences(ctx, table); err != nil {
		return fmt.Errorf("synchronize identity sequences for %s: %w", table, err)
	}
	return nil
}

func (m *Migrator) migrateKeylessTable(ctx context.Context, table string, mysqlColumns, mappedColumns []string, rowCount int64, batchSize int) error {
	query, err := buildMySQLStreamingQuery(table, mysqlColumns)
	if err != nil {
		return fmt.Errorf("build streaming source query for %s: %w", table, err)
	}
	rows, err := m.sourceQueryer().QueryContext(ctx, query)
	if err != nil {
		return fmt.Errorf("stream keyless source table %s: %w", table, err)
	}
	defer rows.Close()

	processedRows := int64(0)
	batch := make([][]any, 0, batchSize)
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		transformedValues, err := m.validateAndTransformData(ctx, table, mappedColumns, batch)
		if err != nil {
			return fmt.Errorf("validate/transform data for %s: %w", table, err)
		}
		if err := m.bulkInsertPostgres(ctx, table, mappedColumns, nil, transformedValues); err != nil {
			return err
		}
		processedRows += int64(len(batch))
		if err := m.storage.UpsertMigration(ctx, storage.MigrationRecord{
			SessionId:    m.sessionId,
			TableName:    table,
			Status:       "in_progress",
			LastOffset:   processedRows,
			LastCursor:   "",
			RowCount:     rowCount,
			ErrorMessage: "",
		}); err != nil {
			return fmt.Errorf("checkpoint keyless migration for %s: %w", table, err)
		}
		progress := fmt.Sprintf("%d rows", processedRows)
		if rowCount > 0 {
			progress = fmt.Sprintf("%d/~%d rows", processedRows, rowCount)
		}
		fmt.Println(lipgloss.Place(0, 0, 0, 0,
			views.BasicLayout.Render(fmt.Sprintf("✅ Inserted %d rows into %s (progress: %s)", len(batch), table, progress)),
		))
		batch = batch[:0]
		return nil
	}

	for rows.Next() {
		values := make([]any, len(mysqlColumns))
		valuePointers := make([]any, len(mysqlColumns))
		for index := range values {
			valuePointers[index] = &values[index]
		}
		if err := rows.Scan(valuePointers...); err != nil {
			return fmt.Errorf("scan keyless source row from %s: %w", table, err)
		}
		batch = append(batch, values)
		if len(batch) == batchSize {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate keyless source rows from %s: %w", table, err)
	}
	if err := flush(); err != nil {
		return err
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close keyless source rows for %s: %w", table, err)
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

func (m *Migrator) getColumnNames(ctx context.Context, table string) ([]string, error) {
	rows, err := m.sourceQueryer().QueryContext(ctx, fmt.Sprintf("SHOW COLUMNS FROM %s", quoteMySQLIdentifier(table)))
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
	rows, err := m.sourceQueryer().QueryContext(ctx, `
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

func (m *Migrator) bulkInsertPostgres(ctx context.Context, table string, columns, conflictColumns []string, data [][]any) error {
	if len(data) == 0 {
		return nil
	}

	chunkSize := postgresInsertChunkSize(len(columns))
	if chunkSize == 0 {
		return fmt.Errorf("cannot insert into %s.%s without columns", m.schemaName, table)
	}

	tx, err := m.postgres.GetDB().BeginTxx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin insert transaction for %s.%s: %w", m.schemaName, table, err)
	}
	defer func() { _ = tx.Rollback() }()

	for start := 0; start < len(data); start += chunkSize {
		end := min(start+chunkSize, len(data))
		query, args, err := buildPostgresInsert(m.schemaName, table, columns, conflictColumns, data[start:end])
		if err != nil {
			return fmt.Errorf("build insert for %s.%s: %w", m.schemaName, table, err)
		}
		result, err := tx.ExecContext(ctx, query, args...)
		if err != nil {
			return fmt.Errorf("insert batch into %s.%s: %w", m.schemaName, table, err)
		}
		rowsAffected, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("read affected row count for %s.%s: %w", m.schemaName, table, err)
		}
		expectedRows := int64(end - start)
		if rowsAffected != expectedRows {
			return fmt.Errorf("insert batch into %s.%s affected %d rows; expected %d", m.schemaName, table, rowsAffected, expectedRows)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit insert batch into %s.%s: %w", m.schemaName, table, err)
	}
	return nil
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

func (m *Migrator) getMySQLAutoIncrementMetadata(ctx context.Context, table string) (string, sql.NullInt64, error) {
	var column string
	var nextValue sql.NullInt64
	err := m.sourceQueryer().QueryRowContext(ctx, `
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
	if m.metadataCache == nil {
		m.metadataCache = make(map[string]*tableValidationMetadata)
	}
	cacheKey := strings.ToLower(m.schemaName + "." + table)
	if metadata, exists := m.metadataCache[cacheKey]; exists {
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

	metadata := &tableValidationMetadata{
		columns:            columnInfo,
		enumValuesByColumn: enumValuesByColumn,
	}
	m.metadataCache[cacheKey] = metadata
	return metadata, nil
}

func (m *Migrator) validateAndTransformData(ctx context.Context, table string, columns []string, data [][]any) ([][]any, error) {
	metadata, err := m.loadValidationMetadata(ctx, table)
	if err != nil {
		return nil, err
	}

	// Transform the data
	transformedData := make([][]any, len(data))
	for i, row := range data {
		if len(row) != len(columns) {
			return nil, fmt.Errorf("row %d has %d values for %d columns", i, len(row), len(columns))
		}
		transformedRow := make([]any, len(row))
		copy(transformedRow, row)

		for j, value := range row {
			columnKey := strings.ToLower(columns[j])
			info, exists := metadata.columns[columnKey]
			if !exists {
				return nil, fmt.Errorf("PostgreSQL column metadata not found for %s.%s", table, columns[j])
			}
			if value == nil {
				if !info.isNullable {
					return nil, fmt.Errorf("row %d column %s.%s is NULL but the target column is NOT NULL", i, table, info.name)
				}
				continue
			}

			if m.isEnumColumn(info.dataType, info.udtName) {
				strValue, ok := stringValue(value)
				if !ok {
					return nil, fmt.Errorf("enum column %s.%s has unsupported value type %T", table, info.name, value)
				}
				if _, valid := metadata.enumValuesByColumn[columnKey][strValue]; valid {
					transformedRow[j] = strValue
					continue
				}
				return nil, fmt.Errorf("enum column %s.%s contains invalid value %q", table, info.name, strValue)
			}

			transformedValue, err := m.validateAndTransformValue(value, info.dataType)
			if err != nil {
				return nil, fmt.Errorf("row %d column %s.%s: %w", i, table, info.name, err)
			}
			transformedRow[j] = transformedValue
		}

		transformedData[i] = transformedRow
	}

	return transformedData, nil
}

func stringValue(value any) (string, bool) {
	switch value := value.(type) {
	case string:
		return value, true
	case []byte:
		return string(value), true
	default:
		return "", false
	}
}

func (m *Migrator) validateAndTransformValue(value any, dataType string) (any, error) {
	if value == nil {
		return nil, nil
	}

	if dataType == "bytea" {
		return value, nil
	}
	if dataType == "uuid" {
		if bytes, ok := value.([]byte); ok && len(bytes) == 16 {
			if parsed, err := uuid.FromBytes(bytes); err == nil {
				return parsed.String(), nil
			}
		}
	}
	if dataType == "boolean" {
		if booleanValue, ok := booleanValue(value); ok {
			return booleanValue, nil
		}
		return nil, fmt.Errorf("invalid boolean value %v", value)
	}

	strValue, ok := stringValue(value)
	if !ok {
		return value, nil
	}

	// Text is copied exactly. Whitespace can be meaningful data.
	if dataType == "text" || strings.Contains(dataType, "character") || dataType == "json" || dataType == "jsonb" {
		return strValue, nil
	}

	strValue = strings.TrimSpace(strValue)
	if strValue == "" {
		return nil, fmt.Errorf("empty value cannot be represented as %s", dataType)
	}

	switch dataType {
	case "time", "time without time zone", "time with time zone":
		if m.isValidTime(strValue) {
			return strValue, nil
		}
		return nil, fmt.Errorf("invalid time value %q", strValue)
	case "date":
		if m.isValidDate(strValue) {
			return strValue, nil
		}
		return nil, fmt.Errorf("invalid date value %q", strValue)
	case "timestamp", "timestamp without time zone", "timestamp with time zone":
		if m.isValidTimestamp(strValue) {
			return strValue, nil
		}
		return nil, fmt.Errorf("invalid timestamp value %q", strValue)
	case "integer", "bigint", "smallint":
		if m.isValidInteger(strValue) {
			return strValue, nil
		}
		return nil, fmt.Errorf("invalid integer value %q", strValue)
	case "numeric", "decimal", "real", "double precision":
		if m.isValidNumeric(strValue) {
			return strValue, nil
		}
		return nil, fmt.Errorf("invalid numeric value %q", strValue)
	case "boolean":
		if parsed, ok := booleanStringValue(strValue); ok {
			return parsed, nil
		}
		return nil, fmt.Errorf("invalid boolean value %q", strValue)
	case "uuid":
		parsed, err := uuid.Parse(strValue)
		if err != nil {
			return nil, fmt.Errorf("invalid UUID value %q: %w", strValue, err)
		}
		return parsed.String(), nil
	default:
		if strings.Contains(strings.ToLower(dataType), "time") {
			if !m.isValidTime(strValue) {
				return nil, fmt.Errorf("invalid time value %q for type %s", strValue, dataType)
			}
		}
	}

	return strValue, nil
}

func booleanValue(value any) (bool, bool) {
	switch value := value.(type) {
	case bool:
		return value, true
	case int:
		return numericBoolean(int64(value))
	case int8:
		return numericBoolean(int64(value))
	case int16:
		return numericBoolean(int64(value))
	case int32:
		return numericBoolean(int64(value))
	case int64:
		return numericBoolean(value)
	case uint:
		return unsignedBoolean(uint64(value))
	case uint8:
		return unsignedBoolean(uint64(value))
	case uint16:
		return unsignedBoolean(uint64(value))
	case uint32:
		return unsignedBoolean(uint64(value))
	case uint64:
		return unsignedBoolean(value)
	case []byte:
		if len(value) == 1 && (value[0] == 0 || value[0] == 1) {
			return value[0] == 1, true
		}
		return booleanStringValue(string(value))
	case string:
		return booleanStringValue(value)
	default:
		return false, false
	}
}

func numericBoolean(value int64) (bool, bool) {
	if value == 0 || value == 1 {
		return value == 1, true
	}
	return false, false
}

func unsignedBoolean(value uint64) (bool, bool) {
	if value == 0 || value == 1 {
		return value == 1, true
	}
	return false, false
}

func booleanStringValue(value string) (bool, bool) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "true", "1", "yes", "t", "y":
		return true, true
	case "false", "0", "no", "f", "n":
		return false, true
	default:
		return false, false
	}
}

func (m *Migrator) isValidTime(value string) bool {
	timeFormats := []string{
		"15:04:05",
		"15:04",
	}

	for _, format := range timeFormats {
		if _, err := time.Parse(format, value); err == nil {
			return true
		}
	}

	return false
}

func (m *Migrator) isValidDate(value string) bool {
	_, err := time.Parse("2006-01-02", value)
	return err == nil
}

func (m *Migrator) isValidTimestamp(value string) bool {
	// Check if it's a valid timestamp format
	timestampFormats := []string{
		"2006-01-02 15:04:05",
		"2006-01-02T15:04:05",
	}

	for _, format := range timestampFormats {
		if _, err := time.Parse(format, value); err == nil {
			return true
		}
	}

	return false
}

func (m *Migrator) isValidInteger(value string) bool {
	// Remove any leading/trailing whitespace
	value = strings.TrimSpace(value)

	// Check if it's a valid integer
	if value == "" {
		return false
	}

	// Check for valid integer patterns
	if _, err := strconv.ParseInt(value, 10, 64); err == nil {
		return true
	}

	return false
}

func (m *Migrator) isValidNumeric(value string) bool {
	// Remove any leading/trailing whitespace
	value = strings.TrimSpace(value)

	// Check if it's a valid numeric
	if value == "" {
		return false
	}

	// Check for valid numeric patterns
	if _, err := strconv.ParseFloat(value, 64); err == nil {
		return true
	}

	return false
}

func (m *Migrator) createTableInPostgres(ctx context.Context, table string) error {

	fmt.Printf("🔍 Creating table %s in schema %s\n", table, m.schemaName)

	// Get MySQL table structure
	mysqlColumns, err := m.getMySQLTableStructure(ctx, table)
	if err != nil {
		return fmt.Errorf("failed to get MySQL table structure: %w", err)
	}

	fmt.Printf("🔍 Found %d columns in MySQL table\n", len(mysqlColumns))

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

	fmt.Printf("✅ Created table %s.%s\n", m.schemaName, table)
	return nil
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

	fmt.Printf("🔍 Creating enum types for table %s in schema %s\n", table, m.schemaName)

	for _, col := range columns {
		// fmt.Printf("🔍 Checking column %s with type: %s\n", col.Name, col.Type)
		// fmt.Printf("🔍 Contains 'enum': %v\n", strings.Contains(strings.ToLower(col.Type), "enum"))

		if strings.Contains(strings.ToLower(col.Type), "enum") {
			// Extract enum values from MySQL enum definition
			enumValues := m.extractEnumValues(col.Type)
			fmt.Printf("🔍 Extracted enum values for %s: %v\n", col.Name, enumValues)

			if len(enumValues) > 0 {
				// Create the enum type
				enumTypeName := postgresEnumTypeName(table, col.Name)
				fmt.Printf("🔍 Creating enum type: %s.%s\n", m.schemaName, enumTypeName)

				err := m.createPostgresEnumType(ctx, enumTypeName, enumValues)
				if err != nil {
					return fmt.Errorf("failed to create enum type %s: %w", enumTypeName, err)
				}
				fmt.Printf("✅ Created enum type %s with values: %v\n", enumTypeName, enumValues)
			} else {
				fmt.Printf("⚠️  No enum values extracted for column %s\n", col.Name)
			}
		} else {
			fmt.Printf("ℹ️  Column %s is not an enum (type: %s)\n", col.Name, col.Type)
		}
	}
	return nil
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

func (m *Migrator) createPostgresEnumType(ctx context.Context, enumTypeName string, values []string) error {

	fmt.Printf("🔍 Creating enum type %s.%s with values: %v\n", m.schemaName, enumTypeName, values)

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
		fmt.Printf("ℹ️  Enum type %s already exists with matching values\n", enumTypeName)
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
	fmt.Printf("🔍 Executing SQL: %s\n", createEnumSQL)

	_, err = m.postgres.GetDB().ExecContext(ctx, createEnumSQL)
	if err != nil {
		return fmt.Errorf("failed to create enum type %s: %w", enumTypeName, err)
	}

	fmt.Printf("✅ Successfully created enum type %s.%s\n", m.schemaName, enumTypeName)
	return nil
}

func (m *Migrator) getMySQLTableStructure(ctx context.Context, table string) ([]ColumnInfo, error) {
	rows, err := m.sourceQueryer().QueryContext(ctx, `
		SELECT
			column_info.COLUMN_NAME,
			column_info.COLUMN_TYPE,
			column_info.IS_NULLABLE,
			column_info.COLUMN_DEFAULT,
			column_info.EXTRA,
			column_info.COLUMN_KEY,
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
		var defaultValue sql.NullString
		if err := rows.Scan(&column.Name, &column.Type, &nullable, &defaultValue, &column.Extra, &columnKey, &referencesUUID); err != nil {
			return nil, err
		}
		column.Nullable = nullable == "YES"
		column.HasDefault = defaultValue.Valid
		column.DefaultValue = defaultValue.String
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
}

func (m *Migrator) parseCreateTableStatement(createStatement string) ([]ColumnInfo, error) {
	// This is a simplified parser - you might want to use a proper SQL parser
	// For now, we'll extract basic column information
	var columns []ColumnInfo

	// Find the column definitions between the first ( and the last )
	start := strings.Index(createStatement, "(")
	end := strings.LastIndex(createStatement, ")")
	if start == -1 || end == -1 {
		return nil, fmt.Errorf("invalid CREATE TABLE statement")
	}

	columnDefs := createStatement[start+1 : end]

	// Split by comma, but be careful about commas inside parentheses
	lines := strings.Split(columnDefs, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "PRIMARY KEY") ||
			strings.HasPrefix(line, "KEY") || strings.HasPrefix(line, "UNIQUE") ||
			strings.HasPrefix(line, "FOREIGN KEY") || strings.HasPrefix(line, "CONSTRAINT") {
			continue
		}

		// Parse column definition
		parts := strings.Fields(line)
		if len(parts) < 2 {
			continue
		}

		columnName := strings.Trim(parts[0], "`")

		// Extract the full column type (including enum definitions)
		columnType := m.extractColumnType(line)

		// Determine nullable
		nullable := true
		if strings.Contains(line, "NOT NULL") {
			nullable = false
		}

		// Extract default value
		defaultValue := ""
		if strings.Contains(line, "DEFAULT") {
			start := strings.Index(line, "DEFAULT")
			end := strings.Index(line[start:], " ")
			if end == -1 {
				end = len(line)
			} else {
				end = start + end
			}
			defaultValue = strings.Trim(line[start:end], "DEFAULT ")
		}

		columns = append(columns, ColumnInfo{
			Name:         columnName,
			Type:         columnType,
			Nullable:     nullable,
			DefaultValue: defaultValue,
		})
	}

	return columns, nil
}

func (m *Migrator) extractColumnType(line string) string {
	// Find the column name (first word)
	parts := strings.Fields(line)
	if len(parts) < 2 {
		return ""
	}

	// Start after the column name
	typeStart := len(parts[0]) + 1 // +1 for the space

	// For enum types, we need to find the complete enum definition
	if strings.Contains(strings.ToLower(line), "enum") {
		// Find the opening parenthesis after ENUM
		enumStart := strings.Index(strings.ToUpper(line[typeStart:]), "ENUM")
		if enumStart == -1 {
			return ""
		}

		// Find the actual start of the enum definition
		actualStart := typeStart + enumStart

		// Find the opening parenthesis
		openParen := strings.Index(line[actualStart:], "(")
		if openParen == -1 {
			return ""
		}

		// Find the matching closing parenthesis
		parenCount := 0
		closeParen := -1
		for i := actualStart + openParen; i < len(line); i++ {
			if line[i] == '(' {
				parenCount++
			} else if line[i] == ')' {
				parenCount--
				if parenCount == 0 {
					closeParen = i
					break
				}
			}
		}

		if closeParen != -1 {
			// Extract the complete enum definition
			enumType := line[actualStart : closeParen+1]
			return enumType
		}
	}

	// For DECIMAL types, we need to find the complete precision/scale definition
	if strings.Contains(strings.ToLower(line), "decimal") {
		// Find the opening parenthesis after DECIMAL
		decimalStart := strings.Index(strings.ToUpper(line[typeStart:]), "DECIMAL")
		if decimalStart == -1 {
			return ""
		}

		// Find the actual start of the decimal definition
		actualStart := typeStart + decimalStart

		// Find the opening parenthesis
		openParen := strings.Index(line[actualStart:], "(")
		if openParen == -1 {
			return ""
		}

		// Find the matching closing parenthesis
		parenCount := 0
		closeParen := -1
		for i := actualStart + openParen; i < len(line); i++ {
			if line[i] == '(' {
				parenCount++
			} else if line[i] == ')' {
				parenCount--
				if parenCount == 0 {
					closeParen = i
					break
				}
			}
		}

		if closeParen != -1 {
			// Extract the complete decimal definition
			decimalType := line[actualStart : closeParen+1]
			return decimalType
		}
	}

	// For other types, find the end of the type definition
	typeEnd := len(line)

	// Look for common type endings
	endings := []string{" NOT NULL", " NULL", " DEFAULT", " AUTO_INCREMENT", " COMMENT", ","}
	for _, ending := range endings {
		if idx := strings.Index(line[typeStart:], ending); idx != -1 {
			if idx < typeEnd-typeStart {
				typeEnd = typeStart + idx
			}
		}
	}

	// Extract the type
	columnType := strings.TrimSpace(line[typeStart:typeEnd])
	return columnType
}

func (m *Migrator) buildCreateTableSQL(table string, columns []ColumnInfo) (string, error) {

	var columnDefs []string
	for _, col := range columns {
		// Convert MySQL type to PostgreSQL type
		pgType, err := m.convertMySQLTypeToPostgres(col.Type, col.Name, table)
		if err != nil {
			return "", fmt.Errorf("column %s: %w", col.Name, err)
		}
		if col.IsUUID {
			pgType = "UUID"
		}

		// Build column definition
		def := fmt.Sprintf(`%s %s`, quotePostgresIdentifier(col.Name), pgType)

		if !col.Nullable {
			def += " NOT NULL"
		}

		extra := strings.ToLower(col.Extra)
		if strings.Contains(extra, "on update") {
			return "", fmt.Errorf("column %s uses unsupported MySQL ON UPDATE behavior", col.Name)
		}
		if strings.Contains(extra, "generated") && !strings.Contains(extra, "default_generated") {
			return "", fmt.Errorf("column %s is a generated column; its generation expression cannot be migrated safely", col.Name)
		}
		if strings.Contains(extra, "auto_increment") {
			if !isPostgresIntegerType(pgType) {
				return "", fmt.Errorf("AUTO_INCREMENT column %s maps to %s, which cannot use a PostgreSQL identity sequence safely", col.Name, pgType)
			}
			def += " GENERATED BY DEFAULT AS IDENTITY"
		} else if col.HasDefault {
			defaultValue, err := postgresDefaultValue(col.DefaultValue, pgType, col.Extra)
			if err != nil {
				return "", fmt.Errorf("column %s default: %w", col.Name, err)
			}
			def += " DEFAULT " + defaultValue
		}

		columnDefs = append(columnDefs, def)
	}

	return fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s.%s (%s)`,
		quotePostgresIdentifier(m.schemaName), quotePostgresIdentifier(table), strings.Join(columnDefs, ", ")), nil
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
	temporalType := postgresType == "DATE" || postgresType == "TIME" || postgresType == "TIMESTAMP"
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

// getMySQLTables retrieves all table names from MySQL
func (m *Migrator) getMySQLTables(ctx context.Context) ([]string, error) {
	rows, err := m.sourceQueryer().QueryContext(ctx, `
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

	var tables []string
	for rows.Next() {
		var tableName string
		var engine sql.NullString
		if err := rows.Scan(&tableName, &engine); err != nil {
			return nil, fmt.Errorf("failed to scan table name: %w", err)
		}
		if !engine.Valid || !strings.EqualFold(engine.String, "InnoDB") {
			return nil, fmt.Errorf("source table %s uses storage engine %q; a consistent resumable snapshot currently requires InnoDB", tableName, engine.String)
		}
		tables = append(tables, tableName)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate MySQL tables: %w", err)
	}
	return tables, nil
}

func (m *Migrator) validateSourceForeignKeyScope(ctx context.Context) error {
	var tableName, constraintName, referencedSchema string
	err := m.sourceQueryer().QueryRowContext(ctx, `
		SELECT TABLE_NAME, CONSTRAINT_NAME, REFERENCED_TABLE_SCHEMA
		FROM INFORMATION_SCHEMA.KEY_COLUMN_USAGE
		WHERE TABLE_SCHEMA = DATABASE()
		AND REFERENCED_TABLE_SCHEMA IS NOT NULL
		AND REFERENCED_TABLE_SCHEMA <> DATABASE()
		LIMIT 1
	`).Scan(&tableName, &constraintName, &referencedSchema)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect cross-database foreign keys: %w", err)
	}
	return fmt.Errorf("foreign key %s on table %s references MySQL database %s; cross-database references cannot be mapped safely into one target schema", constraintName, tableName, referencedSchema)
}

// createAllTables creates all tables in PostgreSQL (structure only, no data)
func (m *Migrator) createAllTables(ctx context.Context, tables []string) error {
	for i, table := range tables {
		fmt.Printf("🏗️  Creating table %d/%d: %s\n", i+1, len(tables), table)

		// Check if table already exists
		exists, err := m.tableExistsInPostgres(ctx, table)
		if err != nil {
			return fmt.Errorf("failed to check if table %s exists: %w", table, err)
		}

		if exists {
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
			fmt.Printf("⏭️  Table %s already exists, verified primary key\n", table)
			continue
		}

		// Create table structure
		if err := m.createTableInPostgres(ctx, table); err != nil {
			return fmt.Errorf("failed to create table %s: %w", table, err)
		}

		fmt.Printf("✅ Created table: %s\n", table)
	}

	return nil
}

// migrateAllSchemaObjects migrates all schema objects (indexes, foreign keys, triggers, functions, views)
func (m *Migrator) migrateAllSchemaObjects(ctx context.Context, tables []string) error {
	fmt.Println("🔧 Migrating indexes and foreign keys for all tables...")
	var migrationErrors []error

	// Migrate schema objects for each table
	for i, table := range tables {
		fmt.Printf("🔧 Migrating schema objects for table %d/%d: %s\n", i+1, len(tables), table)

		if err := m.schemaMigrator.MigrateAllSchemaObjects(ctx, table); err != nil {
			fmt.Printf("⚠️  Failed to migrate schema objects for table %s: %v\n", table, err)
			migrationErrors = append(migrationErrors, err)
		}
	}

	// Migrate global objects (functions, views)
	fmt.Println("🔧 Migrating global schema objects (functions, views)...")
	if err := m.schemaMigrator.MigrateAllFunctions(ctx); err != nil {
		fmt.Printf("⚠️  Failed to migrate functions: %v\n", err)
		migrationErrors = append(migrationErrors, err)
	}

	if err := m.schemaMigrator.MigrateAllViews(ctx); err != nil {
		fmt.Printf("⚠️  Failed to migrate views: %v\n", err)
		migrationErrors = append(migrationErrors, err)
	}

	return errors.Join(migrationErrors...)
}

// migrateAllData migrates all data from MySQL to PostgreSQL
func (m *Migrator) migrateAllData(ctx context.Context, tables []string) error {
	for i, table := range tables {
		fmt.Printf("📦 Migrating data for table %d/%d: %s\n", i+1, len(tables), table)

		// Check migration state
		state, err := m.storage.GetMigration(ctx, m.sessionId, table)
		if err != nil {
			return fmt.Errorf("get migration state for %s: %w", table, err)
		}

		if state == nil {
			state = &storage.MigrationRecord{
				SessionId:    m.sessionId,
				TableName:    table,
				Status:       "pending",
				LastOffset:   0,
				ErrorMessage: "",
			}
			if err := m.storage.UpsertMigration(ctx, *state); err != nil {
				return fmt.Errorf("create migration state for %s: %w", table, err)
			}
		}

		if state.Status == "done" {
			targetRows, err := m.targetTableRowCount(ctx, table)
			if err != nil {
				return fmt.Errorf("verify completed target table %s: %w", table, err)
			}
			if targetRows != state.LastOffset {
				return fmt.Errorf("completed migration state for %s records %d rows but the target contains %d; start a fresh session against an empty target", table, state.LastOffset, targetRows)
			}
			if err := m.syncIdentitySequences(ctx, table); err != nil {
				return fmt.Errorf("synchronize identity sequences for completed table %s: %w", table, err)
			}
			fmt.Printf("⏭️  Skipping %s (already done)\n", table)
			continue
		}

		if state.Status == "in_progress" || state.Status == "error" {
			primaryKeyColumns, err := m.getMySQLPrimaryKeyColumns(ctx, table)
			if err != nil {
				return fmt.Errorf("inspect resume key for %s: %w", table, err)
			}
			if len(primaryKeyColumns) == 0 {
				targetRows, err := m.targetTableRowCount(ctx, table)
				if err != nil {
					return fmt.Errorf("inspect keyless target table %s: %w", table, err)
				}
				if targetRows > 0 {
					return fmt.Errorf("table %s has no primary key and its previous attempt left %d target rows; truncate the target table and start a fresh session", table, targetRows)
				}
			}
			fmt.Printf("🔄 Resuming %s from its last successful checkpoint\n", table)
		}

		// Update status to in_progress
		state.Status = "in_progress"
		state.ErrorMessage = ""
		if err := m.storage.UpsertMigration(ctx, *state); err != nil {
			return fmt.Errorf("mark migration in progress for %s: %w", table, err)
		}

		// Migrate table data
		if err := m.MigrateTable(ctx, table, m.batchSize); err != nil {
			fmt.Printf("❌ Migration failed for %s: %v\n", table, err)
			latest, stateErr := m.storage.GetMigration(ctx, m.sessionId, table)
			if stateErr != nil {
				return fmt.Errorf("migration failed for %s: %w; also failed to read checkpoint: %w", table, err, stateErr)
			}
			if latest == nil {
				latest = state
			}
			latest.Status = "error"
			latest.ErrorMessage = err.Error()
			if stateErr := m.storage.UpsertMigration(ctx, *latest); stateErr != nil {
				return fmt.Errorf("migration failed for %s: %w; also failed to save error state: %w", table, err, stateErr)
			}
			return err
		}

		// Mark as done
		latest, err := m.storage.GetMigration(ctx, m.sessionId, table)
		if err != nil {
			return fmt.Errorf("read final migration checkpoint for %s: %w", table, err)
		}
		if latest == nil {
			latest = state
		}
		targetRows, err := m.targetTableRowCount(ctx, table)
		if err != nil {
			return fmt.Errorf("verify final target table %s: %w", table, err)
		}
		if targetRows != latest.LastOffset {
			return fmt.Errorf("migration copied %d source rows for %s but the target contains %d; refusing to mark the table complete", latest.LastOffset, table, targetRows)
		}
		latest.Status = "done"
		latest.ErrorMessage = ""
		if err := m.storage.UpsertMigration(ctx, *latest); err != nil {
			return fmt.Errorf("mark migration done for %s: %w", table, err)
		}

		fmt.Printf("✅ Completed data migration for: %s\n", table)
	}

	return nil
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
