package migrator

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/lib/pq"
	"github.com/mujhtech/pgstream/internal/storage"
	"golang.org/x/sync/errgroup"
)

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
