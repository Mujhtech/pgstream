package migrator

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// sourceSnapshots holds one dedicated MySQL connection per worker, each with
// an open REPEATABLE READ consistent-snapshot transaction. When aligned under
// FLUSH TABLES WITH READ LOCK all connections observe the same point in time.
type sourceSnapshots struct {
	conns []*sql.Conn
}

// beginAlignedSnapshots opens n consistent snapshots that all observe the
// same source state. Alignment strategies, in order:
//
//  1. FLUSH TABLES WITH READ LOCK around the opens (strongest; needs the
//     RELOAD privilege; the lock is held for a few milliseconds).
//  2. Lock-free verified alignment: capture the binlog position, open the
//     snapshots, capture it again. Equal positions prove no transaction
//     committed while the snapshots were opened, so they are identical —
//     no lock, no write stall, no RELOAD privilege. This is the path on
//     managed platforms (e.g. RDS) where FTWRL is unavailable.
//  3. WithSkipSnapshotLock skips both; the caller asserts quiescence.
func (m *Migrator) beginAlignedSnapshots(ctx context.Context, n int) (*sourceSnapshots, error) {
	if m.skipSnapLock {
		m.warnf("⚠️  Snapshot alignment skipped (--skip-snapshot-lock): the %d worker snapshots are only consistent with each other if the source receives no writes right now.", n)
		return m.openSnapshotConnections(ctx, n)
	}

	snapshots, lockErr := m.beginSnapshotsUnderLock(ctx, n)
	if lockErr == nil {
		return snapshots, nil
	}
	m.warnf("⚠️  FLUSH TABLES WITH READ LOCK unavailable (%v); trying lock-free alignment verified via binlog position.", lockErr)

	snapshots, verifyErr := m.beginSnapshotsVerified(ctx, n)
	if verifyErr == nil {
		return snapshots, nil
	}
	return nil, fmt.Errorf(
		"could not align %d worker snapshots: the lock path failed (%v) and lock-free verification failed (%w). Options: grant the RELOAD privilege for the lock path, grant REPLICATION CLIENT for binlog verification, pass --skip-snapshot-lock if the source is guaranteed quiescent, or run with --workers 1",
		n, lockErr, verifyErr,
	)
}

// beginSnapshotsUnderLock opens n snapshots while holding
// FLUSH TABLES WITH READ LOCK.
func (m *Migrator) beginSnapshotsUnderLock(ctx context.Context, n int) (*sourceSnapshots, error) {
	lockConn, err := m.mysql.GetDB().DB.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("open snapshot lock connection: %w", err)
	}
	if _, err := lockConn.ExecContext(ctx, "FLUSH TABLES WITH READ LOCK"); err != nil {
		_ = lockConn.Close()
		return nil, err
	}
	defer func() {
		_, _ = lockConn.ExecContext(ctx, "UNLOCK TABLES")
		_ = lockConn.Close()
	}()

	return m.openSnapshotConnections(ctx, n)
}

// snapshotAlignmentAttempts bounds retries of lock-free verification on a
// source that commits during the (millisecond-scale) open window.
const snapshotAlignmentAttempts = 5

// beginSnapshotsVerified opens n snapshots without any lock and proves their
// alignment: identical binlog positions before and after the opens mean no
// transaction committed in between, so every snapshot observes the same data.
func (m *Migrator) beginSnapshotsVerified(ctx context.Context, n int) (*sourceSnapshots, error) {
	for attempt := 1; attempt <= snapshotAlignmentAttempts; attempt++ {
		before, err := m.binlogToken(ctx)
		if err != nil {
			return nil, err
		}

		snapshots, err := m.openSnapshotConnections(ctx, n)
		if err != nil {
			return nil, err
		}

		after, err := m.binlogToken(ctx)
		if err != nil {
			snapshots.rollback()
			return nil, err
		}
		if before == after {
			m.logf("✅ %d worker snapshots aligned lock-free (binlog position %s unchanged while opening)", n, after)
			return snapshots, nil
		}
		snapshots.rollback()
		m.warnf("⚠️  Source committed writes while opening snapshots (attempt %d/%d); retrying alignment.", attempt, snapshotAlignmentAttempts)
	}
	return nil, fmt.Errorf("the source kept committing writes during %d alignment attempts", snapshotAlignmentAttempts)
}

// binlogToken returns a token identifying the current binlog position
// (file, offset, and executed GTID set when present). MySQL 8.4 renamed
// SHOW MASTER STATUS to SHOW BINARY LOG STATUS, so both are tried.
func (m *Migrator) binlogToken(ctx context.Context) (string, error) {
	for _, statement := range []string{"SHOW BINARY LOG STATUS", "SHOW MASTER STATUS"} {
		token, err := m.scanBinlogStatus(ctx, statement)
		if err == nil {
			return token, nil
		}
		if !isUnknownStatementError(err) {
			return "", fmt.Errorf("%s: %w", statement, err)
		}
	}
	return "", fmt.Errorf("neither SHOW BINARY LOG STATUS nor SHOW MASTER STATUS is available")
}

func (m *Migrator) scanBinlogStatus(ctx context.Context, statement string) (string, error) {
	rows, err := m.mysql.GetDB().QueryContext(ctx, statement)
	if err != nil {
		return "", err
	}
	defer rows.Close()

	columns, err := rows.Columns()
	if err != nil {
		return "", err
	}
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return "", err
		}
		return "", fmt.Errorf("binary logging is disabled on the source; lock-free snapshot alignment cannot be verified")
	}

	values := make([]sql.NullString, len(columns))
	dests := make([]any, len(columns))
	for i := range values {
		dests[i] = &values[i]
	}
	if err := rows.Scan(dests...); err != nil {
		return "", err
	}

	var token strings.Builder
	for i, column := range columns {
		switch strings.ToLower(column) {
		case "file", "position", "executed_gtid_set":
			token.WriteString(values[i].String)
			token.WriteByte('|')
		}
	}
	return token.String(), nil
}

// isUnknownStatementError reports MySQL's syntax error (1064), raised by
// versions that lack one of the binlog status statement spellings.
func isUnknownStatementError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "1064")
}

// openSnapshotConnections opens n dedicated connections, each with a
// REPEATABLE READ consistent-snapshot transaction started.
func (m *Migrator) openSnapshotConnections(ctx context.Context, n int) (*sourceSnapshots, error) {
	db := m.mysql.GetDB().DB
	snapshots := &sourceSnapshots{}
	for i := 0; i < n; i++ {
		conn, err := db.Conn(ctx)
		if err != nil {
			snapshots.rollback()
			return nil, fmt.Errorf("open snapshot connection %d: %w", i+1, err)
		}
		snapshots.conns = append(snapshots.conns, conn)
		if _, err := conn.ExecContext(ctx, "SET SESSION TRANSACTION ISOLATION LEVEL REPEATABLE READ"); err != nil {
			snapshots.rollback()
			return nil, fmt.Errorf("set isolation level on snapshot connection %d: %w", i+1, err)
		}
		if _, err := conn.ExecContext(ctx, "START TRANSACTION WITH CONSISTENT SNAPSHOT, READ ONLY"); err != nil {
			snapshots.rollback()
			return nil, fmt.Errorf("start consistent snapshot on connection %d: %w", i+1, err)
		}
	}
	return snapshots, nil
}

// commit ends every snapshot transaction and releases the connections.
func (s *sourceSnapshots) commit(ctx context.Context) error {
	var firstErr error
	for _, conn := range s.conns {
		if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil && firstErr == nil {
			firstErr = err
		}
		if err := conn.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	s.conns = nil
	return firstErr
}

// rollback abandons every snapshot transaction, best-effort.
func (s *sourceSnapshots) rollback() {
	for _, conn := range s.conns {
		_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		_ = conn.Close()
	}
	s.conns = nil
}
