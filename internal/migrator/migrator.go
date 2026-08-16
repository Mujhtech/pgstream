package migrator

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/mujhtech/pgstream/internal/database"
	"github.com/mujhtech/pgstream/internal/storage"
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
	casts          *castRules
	phase          Phase
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

// Phase restricts a run to part of the migration, in the spirit of
// pgloader's "schema only" and "data only" options.
type Phase string

const (
	// PhaseAll runs the full migration: tables, data, then schema objects.
	PhaseAll Phase = ""
	// PhaseSchemaOnly creates tables and schema objects (indexes, foreign
	// keys, triggers) but copies no data.
	PhaseSchemaOnly Phase = "schema-only"
	// PhaseDataOnly copies data into tables that already exist (from a full
	// or schema-only run) and skips schema object migration.
	PhaseDataOnly Phase = "data-only"
)

// WithPhase restricts the run to the schema or data portion of a migration.
func WithPhase(phase Phase) Option {
	return func(migrator *Migrator) error {
		switch phase {
		case PhaseAll, PhaseSchemaOnly, PhaseDataOnly:
			migrator.phase = phase
			return nil
		default:
			return fmt.Errorf("unsupported phase %q; use %q or %q", phase, PhaseSchemaOnly, PhaseDataOnly)
		}
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

	// Step 3: Create all tables in PostgreSQL (structure only). A data-only
	// run verifies the tables a prior run created instead of creating any.
	schemaPhaseStart := time.Now()
	if m.phase == PhaseDataOnly {
		m.logf("🏗️  Step 1: Verifying existing table structures (--data-only)...")
		if err := m.verifyTablesForDataOnly(ctx, tables); err != nil {
			return fmt.Errorf("verify tables for data-only run: %w", err)
		}
	} else {
		m.logf("🏗️  Step 1: Creating table structures...")
		if err := m.createAllTables(ctx, tables); err != nil {
			return fmt.Errorf("failed to create tables: %w", err)
		}
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
	if m.phase == PhaseSchemaOnly {
		m.logf("📦 Step 2: Skipping data migration (--schema-only)")
	} else if workers > 1 {
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
	if m.phase != PhaseSchemaOnly {
		throughput := ""
		if seconds := dataPhase.Seconds(); seconds > 0 && rowsCopied > 0 {
			throughput = fmt.Sprintf(", %.0f rows/s", float64(rowsCopied)/seconds)
		}
		m.logf("📦 Step 2 finished in %s (%d rows copied this run%s)", formatDuration(dataPhase), rowsCopied, throughput)
	}

	// Step 5: Create schema objects after the data is in place. A data-only
	// run leaves them to a later full or schema-only run so partial loads
	// never carry half-built constraints.
	if m.phase == PhaseDataOnly {
		m.logf("🔧 Step 3: Skipping schema object migration (--data-only)")
	} else {
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
	}

	m.logf("✅ Migration completed successfully in %s (%d rows copied this run)", formatDuration(time.Since(runStart)), rowsCopied)
	return nil
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
