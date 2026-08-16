package migrator

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	"github.com/lib/pq"
	"github.com/mujhtech/pgstream/internal/database"
	"github.com/mujhtech/pgstream/internal/storage"
	"golang.org/x/sync/errgroup"
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
	filter         *tableFilter
	loadMethod     LoadMethod
	sink           EventSink
	workers        int
	skipSnapLock   bool
	sourceTx       *sqlx.Tx
	metadataMu     sync.RWMutex
	metadataCache  map[string]*tableValidationMetadata
	uuidCheck      uuidDataCheck
	zeroDateMu     sync.Mutex
	zeroDateWarned map[string]bool
	// rowsCopiedThisRun counts rows loaded by the current Start invocation
	// across all workers, for run-level throughput reporting.
	rowsCopiedThisRun atomic.Int64
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

// WithTableFilter restricts the migration to an explicit table subset.
// Include and exclude selections are mutually exclusive.
func WithTableFilter(include, exclude []string) Option {
	return func(migrator *Migrator) error {
		filter, err := newTableFilter(include, exclude)
		if err != nil {
			return err
		}
		migrator.filter = filter
		return nil
	}
}

// LoadMethod selects how batches are written to PostgreSQL.
type LoadMethod string

const (
	// LoadMethodCopy streams batches through the PostgreSQL COPY protocol.
	// Batches with replay risk after a resume still use conflict-targeted
	// inserts, because COPY cannot upsert.
	LoadMethodCopy LoadMethod = "copy"
	// LoadMethodInsert uses transactional multi-row INSERT statements only.
	LoadMethodInsert LoadMethod = "insert"
)

const maxWorkers = 16

// WithWorkers migrates up to n tables concurrently. Each worker holds its
// own MySQL connection with a consistent snapshot; snapshots are aligned
// under a brief FLUSH TABLES WITH READ LOCK (requires the RELOAD or
// FLUSH_TABLES privilege) unless WithSkipSnapshotLock is set.
func WithWorkers(n int) Option {
	return func(migrator *Migrator) error {
		if n < 1 || n > maxWorkers {
			return fmt.Errorf("workers must be between 1 and %d", maxWorkers)
		}
		migrator.workers = n
		return nil
	}
}

// WithSkipSnapshotLock skips the FLUSH TABLES WITH READ LOCK used to align
// multi-worker snapshots. Only safe when the source receives no writes
// during the migration; the caller is asserting that.
func WithSkipSnapshotLock(skip bool) Option {
	return func(migrator *Migrator) error {
		migrator.skipSnapLock = skip
		return nil
	}
}

func WithLoadMethod(method LoadMethod) Option {
	return func(migrator *Migrator) error {
		switch method {
		case LoadMethodCopy, LoadMethodInsert:
			migrator.loadMethod = method
			return nil
		default:
			return fmt.Errorf("unsupported load method %q; use %q or %q", method, LoadMethodCopy, LoadMethodInsert)
		}
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

// New builds a migrator. storage may be nil only for DryRun; Start requires it.
func New(mysql *database.Database, postgres *database.Database, storage *storage.Storage, sessionId string, options ...Option) (*Migrator, error) {
	if mysql == nil || postgres == nil {
		return nil, fmt.Errorf("MySQL and PostgreSQL connections are required")
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
		loadMethod:     LoadMethodCopy,
		workers:        1,
		metadataCache:  make(map[string]*tableValidationMetadata),
	}
	for _, option := range options {
		if err := option(migrator); err != nil {
			return nil, err
		}
	}
	// One serialized sink shared with schema object migration, so parallel
	// table workers never interleave inside a sink call.
	migrator.sink = lockedSink(migrator.sink)
	schemaMigrator.sink = migrator.sink
	return migrator, nil
}

func (m *Migrator) Start(ctx context.Context) error {
	if m.storage == nil {
		return fmt.Errorf("session metadata storage is required to run a migration")
	}
	runStart := time.Now()
	m.rowsCopiedThisRun.Store(0)
	m.logf("🚀 Starting MySQL to PostgreSQL migration...")

	// Step 1: Ensure schema exists
	if err := m.ensureSchemaExists(ctx); err != nil {
		return fmt.Errorf("failed to ensure schema exists: %w", err)
	}

	// Step 2: Get all tables from MySQL
	sourceTables, err := m.getMySQLTables(ctx)
	if err != nil {
		return fmt.Errorf("failed to get MySQL tables: %w", err)
	}
	tables, err := m.selectTables(sourceTables)
	if err != nil {
		return err
	}
	if err := m.validateSourceForeignKeyScope(ctx); err != nil {
		return err
	}
	if m.filter.active() {
		if err := m.validateForeignKeyClosure(ctx, tables); err != nil {
			return err
		}
		m.logf("📋 Selected %d of %d tables to migrate\n", len(tables), len(sourceTables))
	} else {
		m.logf("📋 Found %d tables to migrate\n", len(tables))
	}

	// Native-UUID conversion is decided once, per foreign-key-connected
	// group, before any table is created.
	if err := m.resolveUUIDConversions(ctx, tables); err != nil {
		return fmt.Errorf("resolve UUID conversions: %w", err)
	}

	// Step 3: Create all tables in PostgreSQL (structure only)
	m.logf("🏗️  Step 1: Creating table structures...")
	schemaPhaseStart := time.Now()
	if err := m.createAllTables(ctx, tables); err != nil {
		return fmt.Errorf("failed to create tables: %w", err)
	}
	if m.freshSession {
		if err := m.validateFreshTargetTables(ctx, tables); err != nil {
			return fmt.Errorf("validate fresh target: %w", err)
		}
	}
	m.logf("🏗️  Step 1 finished in %s", formatDuration(time.Since(schemaPhaseStart)))

	// Step 4: Migrate data before secondary indexes, foreign keys, and triggers.
	// This keeps bulk loading fast and prevents target-side behavior from mutating source data.
	workers := m.workers
	if workers > len(tables) {
		workers = len(tables)
	}
	dataPhaseStart := time.Now()
	if workers > 1 {
		m.logf("📦 Step 2: Migrating data (%d tables, %d workers)...", len(tables), workers)
		snapshots, err := m.beginAlignedSnapshots(ctx, workers)
		if err != nil {
			return fmt.Errorf("begin aligned MySQL snapshots: %w", err)
		}
		if err := m.migrateAllDataParallel(ctx, tables, snapshots); err != nil {
			snapshots.rollback()
			return fmt.Errorf("failed to migrate data: %w", err)
		}
		if err := snapshots.commit(ctx); err != nil {
			return fmt.Errorf("finish consistent MySQL snapshots: %w", err)
		}
	} else {
		m.logf("📦 Step 2: Migrating data...")
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
	}

	dataPhase := time.Since(dataPhaseStart)
	rowsCopied := m.rowsCopiedThisRun.Load()
	throughput := ""
	if seconds := dataPhase.Seconds(); seconds > 0 && rowsCopied > 0 {
		throughput = fmt.Sprintf(", %.0f rows/s", float64(rowsCopied)/seconds)
	}
	m.logf("📦 Step 2 finished in %s (%d rows copied this run%s)", formatDuration(dataPhase), rowsCopied, throughput)

	// Step 5: Create schema objects after the data is in place.
	m.logf("🔧 Step 3: Migrating schema objects...")
	indexPhaseStart := time.Now()
	schemaErr := m.migrateAllSchemaObjects(ctx, tables)
	// DDL that needs a human decision is persisted even when schema object
	// migration reported errors.
	if err := m.writeManualStatements(); err != nil {
		m.warnf("⚠️  Failed to write manual DDL file: %v", err)
	}
	if schemaErr != nil {
		return fmt.Errorf("failed to migrate schema objects: %w", schemaErr)
	}
	m.logf("🔧 Step 3 finished in %s", formatDuration(time.Since(indexPhaseStart)))

	m.logf("✅ Migration completed successfully in %s (%d rows copied this run)", formatDuration(time.Since(runStart)), rowsCopied)
	return nil
}

// formatRowProgress renders copy progress. Exact totals come from a
// snapshot COUNT(*) and can never be exceeded; the statistics-estimate
// fallback is marked with '~' and dropped entirely once it proves wrong.
func formatRowProgress(processed, total int64, exact bool) string {
	switch {
	case total <= 0 || (!exact && processed > total):
		return fmt.Sprintf("%d rows", processed)
	case exact:
		return fmt.Sprintf("%d/%d rows", processed, total)
	default:
		return fmt.Sprintf("%d/~%d rows", processed, total)
	}
}

// formatDuration renders run timings at a human scale: milliseconds under a
// second, tenths of a second under a minute, whole seconds beyond.
func formatDuration(duration time.Duration) string {
	switch {
	case duration < time.Second:
		return duration.Round(time.Millisecond).String()
	case duration < time.Minute:
		return duration.Round(100 * time.Millisecond).String()
	default:
		return duration.Round(time.Second).String()
	}
}

// writeManualStatements saves DDL that pgstream deliberately skipped (for
// example foreign keys whose referenced table is missing from a partial
// backup) so it can be reviewed and applied once the blocker is resolved.
func (m *Migrator) writeManualStatements() error {
	statements := m.schemaMigrator.ManualStatements()
	if len(statements) == 0 {
		return nil
	}

	var script strings.Builder
	script.WriteString("-- pgstream: DDL requiring manual review, session " + m.sessionId + "\n")
	script.WriteString("-- Each statement was skipped for the stated reason. Review it, resolve\n")
	script.WriteString("-- the blocker, and run the statement against the target database.\n")
	script.WriteString("-- Re-running the same pgstream session after restoring missing source\n")
	script.WriteString("-- tables also retries these constraints automatically.\n\n")
	for _, statement := range statements {
		fmt.Fprintf(&script, "-- [%s] %s on %s\n-- reason: %s\n%s;\n\n", statement.Kind, statement.Name, statement.Table, statement.Reason, statement.SQL)
	}

	fileName := fmt.Sprintf("pgstream-manual-%s.sql", m.sessionId)
	if err := os.WriteFile(fileName, []byte(script.String()), 0o600); err != nil {
		return err
	}
	m.warnf("⚠️  %d schema objects need manual work; their DDL was written to %s", len(statements), fileName)
	return nil
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

	// COUNT(*) on the snapshot connection is exact for precisely the rows
	// this copy will read (same snapshot), scans only the clustered index
	// server-side, and costs a few seconds even on multi-million-row tables
	// — a fair price for progress that cannot exceed 100%. InnoDB's
	// TABLE_ROWS statistics estimate (often off by 30-50%) is only the
	// fallback when counting fails.
	var rowCount int64
	exactCount := true
	if err := m.sourceQueryer(ctx).QueryRowContext(ctx, fmt.Sprintf("SELECT COUNT(*) FROM %s", quoteMySQLIdentifier(table))).Scan(&rowCount); err != nil {
		exactCount = false
		var estimatedRowCount sql.NullInt64
		row := m.sourceQueryer(ctx).QueryRowContext(ctx, `
			SELECT TABLE_ROWS
			FROM INFORMATION_SCHEMA.TABLES
			WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ?
		`, table)
		if err := row.Scan(&estimatedRowCount); err != nil {
			return fmt.Errorf("estimate rows in %s: %w", table, err)
		}
		rowCount = estimatedRowCount.Int64
		m.warnf("⚠️  Could not count rows in %s exactly; progress totals for this table are the ~%d statistics estimate", table, rowCount)
	}

	if exactCount {
		m.progress(table, 0, rowCount, "📦 Migrating table %s (%d rows)...", table, rowCount)
	} else {
		m.progress(table, 0, rowCount, "📦 Migrating table %s (approximately %d rows)...", table, rowCount)
	}

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
			m.logf("🔄 Resuming migration after %d rows\n", processedRows)
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
		m.warnf("⚠️  Table %s has no primary key; using one streaming source scan with bounded insert batches. This table cannot be resumed after interruption.\n", table)
		if err := m.migrateKeylessTable(ctx, table, mysqlColumns, mappedColumns, rowCount, exactCount, batchSize); err != nil {
			return err
		}
		if err := m.syncIdentitySequences(ctx, table); err != nil {
			return fmt.Errorf("synchronize identity sequences for %s: %w", table, err)
		}
		return nil
	}

	// A prior interrupted invocation may have committed one batch beyond its
	// checkpoint, so up to one previous-run batch of rows after the cursor can
	// already exist in the target and must load through upserts. The previous
	// run's batch size is recorded in the checkpoint; a legacy checkpoint
	// without one gets an unbounded replay window. Rows past the window read
	// strictly beyond anything previously committed and can stream through
	// COPY.
	replayWindow := int64(batchSize)
	if state != nil {
		if state.BatchSize > 0 {
			replayWindow = state.BatchSize
		} else {
			replayWindow = -1
		}
	}
	loadedThisInvocation := int64(0)

	// Reader/writer pipeline: the reader pulls the next batch from MySQL
	// while the writer validates, loads, and checkpoints the previous one.
	// The unbuffered channel bounds memory to two in-flight batches, and the
	// single ordered writer preserves the checkpoint and replay-window
	// invariants exactly as in the sequential path. The reader owns the
	// source connection and the channel; the writer owns all target and
	// checkpoint writes.
	type sourceBatch struct {
		values     [][]any
		lastCursor string
	}
	batches := make(chan sourceBatch)

	group, groupCtx := errgroup.WithContext(ctx)

	// Wait-time accounting: each duration is owned by one goroutine and read
	// only after group.Wait, so a completed table can report where its wall
	// time actually went (source reads vs target writes vs local work).
	var sourceReadTime, transformTime, targetWriteTime, checkpointTime time.Duration
	phaseStart := time.Now()

	group.Go(func() error {
		defer close(batches)
		readCursor := cursor
		readOffset := processedRows
		for {
			readStart := time.Now()
			query, queryArgs, err := buildMySQLBatchQuery(table, mysqlColumns, primaryKeyColumns, readCursor, readOffset, batchSize)
			if err != nil {
				return fmt.Errorf("build source query for %s: %w", table, err)
			}
			rows, err := m.sourceQueryer(groupCtx).QueryContext(groupCtx, query, queryArgs...)
			if err != nil {
				return fmt.Errorf("read source batch from %s: %w", table, err)
			}

			// One arena per batch backs every row's value slice, and the
			// scan-pointer slice is reused across rows: two allocations per
			// batch instead of two per row.
			columnCount := len(mysqlColumns)
			values := make([][]any, 0, batchSize)
			arena := make([]any, batchSize*columnCount)
			colPtrs := make([]any, columnCount)
			for rows.Next() {
				var cols []any
				if next := len(values) * columnCount; next+columnCount <= len(arena) {
					cols = arena[next : next+columnCount : next+columnCount]
				} else {
					cols = make([]any, columnCount)
				}
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
				sourceReadTime += time.Since(readStart)
				return nil
			}

			sourceReadTime += time.Since(readStart)
			readCursor, err = extractCursor(values[len(values)-1], mysqlColumns, primaryKeyColumns)
			if err != nil {
				return fmt.Errorf("capture resume cursor for %s: %w", table, err)
			}
			encodedCursor, err := encodeCursor(readCursor)
			if err != nil {
				return fmt.Errorf("encode resume cursor for %s: %w", table, err)
			}
			readOffset += int64(len(values))

			select {
			case batches <- sourceBatch{values: values, lastCursor: encodedCursor}:
			case <-groupCtx.Done():
				return groupCtx.Err()
			}
			if len(values) < batchSize {
				return nil
			}
		}
	})

	group.Go(func() error {
		for batch := range batches {
			transformStart := time.Now()
			transformedValues, err := m.validateAndTransformData(groupCtx, table, mappedColumns, batch.values)
			if err != nil {
				return fmt.Errorf("validate/transform data for %s: %w", table, err)
			}
			transformTime += time.Since(transformStart)

			replayRisk := replayWindow < 0 || loadedThisInvocation < replayWindow
			writeStart := time.Now()
			if err := m.loadBatch(groupCtx, table, mappedColumns, mappedPrimaryKeyColumns, transformedValues, replayRisk); err != nil {
				return err
			}
			targetWriteTime += time.Since(writeStart)
			loadedThisInvocation += int64(len(batch.values))
			processedRows += int64(len(batch.values))
			m.rowsCopiedThisRun.Add(int64(len(batch.values)))

			checkpointStart := time.Now()
			if err := m.storage.UpsertMigration(groupCtx, storage.MigrationRecord{
				SessionId:    m.sessionId,
				TableName:    table,
				Status:       "in_progress",
				LastOffset:   processedRows,
				LastCursor:   batch.lastCursor,
				BatchSize:    int64(batchSize),
				RowCount:     rowCount,
				ErrorMessage: "",
			}); err != nil {
				return fmt.Errorf("checkpoint migration for %s: %w", table, err)
			}
			checkpointTime += time.Since(checkpointStart)

			m.progress(table, processedRows, rowCount, "✅ Inserted %d rows into %s (progress: %s)", len(batch.values), table, formatRowProgress(processedRows, rowCount, exactCount))
		}
		return nil
	})

	if err := group.Wait(); err != nil {
		return err
	}

	// The reader and writer overlap, so segments are reported against wall
	// time individually; the dominant segment is the bottleneck.
	if wall := time.Since(phaseStart); wall > time.Second {
		m.logf("⏱  %s time breakdown: source read %s (%.0f%%), target write %s (%.0f%%), transform %s, checkpoints %s (wall %s)",
			table,
			formatDuration(sourceReadTime), 100*sourceReadTime.Seconds()/wall.Seconds(),
			formatDuration(targetWriteTime), 100*targetWriteTime.Seconds()/wall.Seconds(),
			formatDuration(transformTime), formatDuration(checkpointTime), formatDuration(wall))
	}

	if err := m.syncIdentitySequences(ctx, table); err != nil {
		return fmt.Errorf("synchronize identity sequences for %s: %w", table, err)
	}
	return nil
}

func (m *Migrator) migrateKeylessTable(ctx context.Context, table string, mysqlColumns, mappedColumns []string, rowCount int64, exactCount bool, batchSize int) error {
	// Keyless copies cannot deduplicate, so the target must start empty.
	targetRows, err := m.targetTableRowCount(ctx, table)
	if err != nil {
		return fmt.Errorf("inspect keyless target table %s: %w", table, err)
	}
	if targetRows != 0 {
		return fmt.Errorf("keyless table %s already has %d rows in the target; truncate %s.%s and restart its migration", table, targetRows, m.schemaName, table)
	}

	query, err := buildMySQLStreamingQuery(table, mysqlColumns)
	if err != nil {
		return fmt.Errorf("build streaming source query for %s: %w", table, err)
	}
	rows, err := m.sourceQueryer(ctx).QueryContext(ctx, query)
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
		// Keyless tables are always verified empty before streaming starts,
		// so their batches never replay committed rows.
		if err := m.loadBatch(ctx, table, mappedColumns, nil, transformedValues, false); err != nil {
			return err
		}
		processedRows += int64(len(batch))
		m.rowsCopiedThisRun.Add(int64(len(batch)))
		if err := m.storage.UpsertMigration(ctx, storage.MigrationRecord{
			SessionId:    m.sessionId,
			TableName:    table,
			Status:       "in_progress",
			LastOffset:   processedRows,
			LastCursor:   "",
			BatchSize:    int64(batchSize),
			RowCount:     rowCount,
			ErrorMessage: "",
		}); err != nil {
			return fmt.Errorf("checkpoint keyless migration for %s: %w", table, err)
		}
		m.progress(table, processedRows, rowCount, "✅ Inserted %d rows into %s (progress: %s)", len(batch), table, formatRowProgress(processedRows, rowCount, exactCount))
		batch = batch[:0]
		return nil
	}

	// Same arena pattern as the keyed reader: the arena is safe to reuse
	// after each flush because flush loads the batch synchronously.
	columnCount := len(mysqlColumns)
	arena := make([]any, batchSize*columnCount)
	valuePointers := make([]any, columnCount)
	for rows.Next() {
		var values []any
		if next := len(batch) * columnCount; next+columnCount <= len(arena) {
			values = arena[next : next+columnCount : next+columnCount]
		} else {
			values = make([]any, columnCount)
		}
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
			arena = make([]any, batchSize*columnCount)
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

// bulkCopyPostgres loads one batch through the PostgreSQL COPY protocol in a
// single transaction. COPY cannot resolve conflicts, so callers must only use
// it for batches that cannot replay already-copied rows.
func (m *Migrator) bulkCopyPostgres(ctx context.Context, table string, columns []string, data [][]any) error {
	if len(data) == 0 {
		return nil
	}
	if len(columns) == 0 {
		return fmt.Errorf("cannot copy into %s.%s without columns", m.schemaName, table)
	}

	tx, err := m.postgres.GetDB().BeginTxx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin copy transaction for %s.%s: %w", m.schemaName, table, err)
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx, pq.CopyInSchema(m.schemaName, table, columns...))
	if err != nil {
		return fmt.Errorf("prepare copy into %s.%s: %w", m.schemaName, table, err)
	}

	for rowIndex, row := range data {
		if len(row) != len(columns) {
			_ = stmt.Close()
			return fmt.Errorf("row %d has %d values for %d columns", rowIndex, len(row), len(columns))
		}
		if _, err := stmt.ExecContext(ctx, row...); err != nil {
			_ = stmt.Close()
			return fmt.Errorf("buffer copy row %d into %s.%s: %w", rowIndex, m.schemaName, table, err)
		}
	}

	result, err := stmt.ExecContext(ctx)
	if err != nil {
		_ = stmt.Close()
		return fmt.Errorf("flush copy into %s.%s: %w", m.schemaName, table, err)
	}
	if err := stmt.Close(); err != nil {
		return fmt.Errorf("close copy statement for %s.%s: %w", m.schemaName, table, err)
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read copied row count for %s.%s: %w", m.schemaName, table, err)
	}
	if rowsAffected != int64(len(data)) {
		return fmt.Errorf("copy into %s.%s wrote %d rows; expected %d", m.schemaName, table, rowsAffected, len(data))
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit copy into %s.%s: %w", m.schemaName, table, err)
	}
	return nil
}

// loadBatch writes one validated batch to PostgreSQL. replayRisk marks batches
// that may contain rows already committed by an interrupted invocation; those
// must go through conflict-targeted inserts because COPY cannot upsert.
func (m *Migrator) loadBatch(ctx context.Context, table string, columns, conflictColumns []string, data [][]any, replayRisk bool) error {
	if m.loadMethod == LoadMethodCopy && !replayRisk {
		return m.bulkCopyPostgres(ctx, table, columns, data)
	}
	return m.bulkInsertPostgres(ctx, table, columns, conflictColumns, data)
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

// validateAndTransformData validates a batch and converts values in place.
// The heavy lifting lives in buildColumnPlans/transformRows: the plan
// resolves all per-column metadata once, so the per-cell loop performs no
// map lookups, no case folding, and no string-based type dispatch
// (benchmarked in hotpath_bench_test.go).
func (m *Migrator) validateAndTransformData(ctx context.Context, table string, columns []string, data [][]any) ([][]any, error) {
	plans, err := m.buildColumnPlans(ctx, table, columns)
	if err != nil {
		return nil, err
	}
	if err := m.transformRows(table, plans, data); err != nil {
		return nil, err
	}
	return data, nil
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

	m.logf("✅ Created table %s.%s (%d columns)\n", m.schemaName, table, len(mysqlColumns))
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
}

// isGeneratedColumn reports whether EXTRA marks a MySQL VIRTUAL or STORED
// generated column (as opposed to DEFAULT_GENERATED expression defaults).
func isGeneratedColumn(extra string) bool {
	lower := strings.ToLower(extra)
	return strings.Contains(lower, "virtual generated") || strings.Contains(lower, "stored generated")
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
	pgType, err := m.convertMySQLTypeToPostgres(col.Type, col.Name, table)
	if err != nil {
		return "", "", fmt.Errorf("column %s: %w", col.Name, err)
	}
	if col.IsUUID {
		pgType = "UUID"
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

// migrateAllData migrates all data from MySQL to PostgreSQL
func (m *Migrator) migrateAllData(ctx context.Context, tables []string) error {
	for i, table := range tables {
		m.logf("📦 Migrating data for table %d/%d: %s\n", i+1, len(tables), table)
		if err := m.migrateOneTable(ctx, table); err != nil {
			return err
		}
	}
	return nil
}

// migrateAllDataParallel copies tables through a bounded worker pool. Each
// worker owns one aligned snapshot connection for its source reads; the first
// table failure cancels the remaining work.
func (m *Migrator) migrateAllDataParallel(ctx context.Context, tables []string, snapshots *sourceSnapshots) error {
	group, groupCtx := errgroup.WithContext(ctx)
	tableCh := make(chan string)

	for _, conn := range snapshots.conns {
		workerCtx := withSourceQueryer(groupCtx, conn)
		group.Go(func() error {
			for table := range tableCh {
				m.logf("📦 Migrating data for table: %s\n", table)
				if err := m.migrateOneTable(workerCtx, table); err != nil {
					return err
				}
			}
			return nil
		})
	}

	group.Go(func() error {
		defer close(tableCh)
		for _, table := range tables {
			select {
			case tableCh <- table:
			case <-groupCtx.Done():
				return groupCtx.Err()
			}
		}
		return nil
	})

	return group.Wait()
}

// migrateOneTable drives one table through state checks, data copy, and
// completion verification. Safe to call from concurrent workers as long as
// each worker's context carries its own source queryer.
func (m *Migrator) migrateOneTable(ctx context.Context, table string) error {
	tableStart := time.Now()
	{
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
				BatchSize:    int64(m.batchSize),
				ErrorMessage: "",
			}
			if err := m.storage.UpsertMigration(ctx, *state); err != nil {
				return fmt.Errorf("create migration state for %s: %w", table, err)
			}
		}

		startOffset := state.LastOffset

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
			m.logf("⏭️  Skipping %s (already done)\n", table)
			return nil
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
			m.logf("🔄 Resuming %s from its last successful checkpoint\n", table)
		}

		// Update status to in_progress
		state.Status = "in_progress"
		state.ErrorMessage = ""
		if err := m.storage.UpsertMigration(ctx, *state); err != nil {
			return fmt.Errorf("mark migration in progress for %s: %w", table, err)
		}

		// Migrate table data
		if err := m.MigrateTable(ctx, table, m.batchSize); err != nil {
			// A user interrupt is not a failure: the last committed batch is
			// checkpointed and the table stays resumable. Cleanup writes use
			// a non-cancelled context so the interrupt itself cannot prevent
			// recording state.
			cleanupCtx := context.WithoutCancel(ctx)
			if errors.Is(err, context.Canceled) || ctx.Err() != nil {
				m.warnf("⏸️  Interrupted while migrating %s; progress up to the last committed batch is checkpointed\n", table)
				return err
			}

			m.warnf("❌ Migration failed for %s: %v\n", table, err)
			latest, stateErr := m.storage.GetMigration(cleanupCtx, m.sessionId, table)
			if stateErr != nil {
				return fmt.Errorf("migration failed for %s: %w; also failed to read checkpoint: %w", table, err, stateErr)
			}
			if latest == nil {
				latest = state
			}
			latest.Status = "error"
			latest.ErrorMessage = err.Error()
			if stateErr := m.storage.UpsertMigration(cleanupCtx, *latest); stateErr != nil {
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

		tableRows := latest.LastOffset - startOffset
		tableTook := time.Since(tableStart)
		rate := ""
		if seconds := tableTook.Seconds(); seconds > 0 && tableRows > 0 {
			rate = fmt.Sprintf(", %.0f rows/s", float64(tableRows)/seconds)
		}
		m.logf("✅ Completed data migration for: %s (%d rows in %s%s)\n", table, tableRows, formatDuration(tableTook), rate)
	}

	return nil
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
