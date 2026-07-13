package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/mujhtech/pgstream/config"
	"github.com/mujhtech/pgstream/internal/database"
)

var (
	sqliteCreateTable = `
	CREATE TABLE IF NOT EXISTS sessions (
		id TEXT PRIMARY KEY,
		connector TEXT NOT NULL,
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		deleted_at DATETIME DEFAULT NULL
	);

	CREATE TABLE IF NOT EXISTS migrations (
		id TEXT PRIMARY KEY,
		session_id TEXT NOT NULL,
		table_name TEXT NOT NULL,
		status TEXT,
		last_offset INTEGER,
		last_cursor TEXT NOT NULL DEFAULT '',
		row_count INTEGER,
		error_message TEXT,
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		deleted_at DATETIME DEFAULT NULL,
		UNIQUE(session_id, table_name)
	);
	`

	postgresCreateTable = `
	CREATE TABLE IF NOT EXISTS sessions (
		id UUID PRIMARY KEY,
		connector TEXT NOT NULL,
		created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
		updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
		deleted_at TIMESTAMP DEFAULT NULL
	);

	CREATE TABLE IF NOT EXISTS migrations (
		id UUID PRIMARY KEY,
		session_id UUID NOT NULL,
		table_name TEXT NOT NULL,
		status TEXT,
		last_offset BIGINT,
		last_cursor TEXT NOT NULL DEFAULT '',
		row_count BIGINT,
		error_message TEXT,
		created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
		updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
		deleted_at TIMESTAMP DEFAULT NULL,
		UNIQUE(session_id, table_name)
	);
	`
)

type Storage struct {
	db  *database.Database
	cfg config.Database
}

func (s *Storage) Close() error {
	return s.db.Close()
}

func InitStorage(ctx context.Context, cfg config.Database) (*Storage, error) {
	if err := secureSQLiteMetadataFile(cfg); err != nil {
		return nil, err
	}

	db, err := database.Connect(ctx, cfg)

	if err != nil {
		return nil, err
	}

	storage := &Storage{
		db:  db,
		cfg: cfg,
	}

	if err := storage.setup(); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := secureSQLiteMetadataFile(cfg); err != nil {
		_ = db.Close()
		return nil, err
	}

	return storage, nil
}

func secureSQLiteMetadataFile(cfg config.Database) error {
	if cfg.Driver != config.DatabaseDriverSqlite3 || cfg.Path == "" || cfg.Path == ":memory:" || strings.HasPrefix(cfg.Path, "file:") {
		return nil
	}
	if _, err := os.Stat(cfg.Path); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return fmt.Errorf("inspect SQLite metadata file: %w", err)
	}
	if err := os.Chmod(cfg.Path, 0o600); err != nil {
		return fmt.Errorf("secure SQLite metadata file: %w", err)
	}
	return nil
}

func (s *Storage) setup() error {
	// Determine which schema to use based on the database type
	var createTableSQL string

	// Use the config to determine the database type
	switch s.cfg.Driver {
	case config.DatabaseDriverSqlite3:
		createTableSQL = sqliteCreateTable
	case config.DatabaseDriverPostgres:
		createTableSQL = postgresCreateTable
	default:
		return fmt.Errorf("unsupported metadata storage driver %q", s.cfg.Driver)
	}

	if _, err := s.db.GetDB().Exec(createTableSQL); err != nil {
		return fmt.Errorf("create metadata tables: %w", err)
	}

	return s.ensureMigrationCursorColumn()
}

func (s *Storage) ensureMigrationCursorColumn() error {
	var exists bool
	var query string

	switch s.cfg.Driver {
	case config.DatabaseDriverSqlite3:
		query = `SELECT EXISTS(SELECT 1 FROM pragma_table_info('migrations') WHERE name = 'last_cursor')`
	case config.DatabaseDriverPostgres:
		query = `SELECT EXISTS(
			SELECT 1 FROM information_schema.columns
			WHERE table_schema = current_schema()
			AND table_name = 'migrations'
			AND column_name = 'last_cursor'
		)`
	default:
		return fmt.Errorf("unsupported metadata storage driver %q", s.cfg.Driver)
	}

	if err := s.db.GetDB().QueryRow(query).Scan(&exists); err != nil {
		return fmt.Errorf("inspect migrations metadata schema: %w", err)
	}
	if exists {
		return nil
	}

	if _, err := s.db.GetDB().Exec(`ALTER TABLE migrations ADD COLUMN last_cursor TEXT NOT NULL DEFAULT ''`); err != nil {
		return fmt.Errorf("add migration cursor metadata: %w", err)
	}
	return nil
}

func (s *Storage) query(query string) string {
	return s.db.GetDB().Rebind(query)
}

func (s *Storage) CreateSession(ctx context.Context, connector string) (string, error) {
	sessionId := uuid.New().String()
	_, err := s.db.GetDB().ExecContext(ctx, s.query(`
	INSERT INTO sessions (id, connector)
	VALUES (?, ?)
	`), sessionId, connector)
	if err != nil {
		return "", err
	}
	return sessionId, nil
}

func (s *Storage) GetSessionByID(ctx context.Context, sessionId string) (*Session, error) {
	row := s.db.GetDB().QueryRowContext(ctx, s.query("SELECT id, connector, created_at, updated_at, deleted_at FROM sessions WHERE id = ?"), sessionId)
	var session Session
	err := row.Scan(&session.Id, &session.Connector, &session.CreatedAt, &session.UpdatedAt, &session.DeletedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &session, nil
}

func (s *Storage) UpdateSessionConnector(ctx context.Context, sessionID string, connector string) error {
	result, err := s.db.GetDB().ExecContext(ctx, s.query(`
		UPDATE sessions
		SET connector = ?, updated_at = ?
		WHERE id = ?
	`), connector, time.Now(), sessionID)
	if err != nil {
		return err
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rowsAffected != 1 {
		return fmt.Errorf("session %q was not found", sessionID)
	}
	return nil
}

func (s *Storage) UpsertMigration(ctx context.Context, record MigrationRecord) error {
	// Generate ID if not provided
	if record.Id == "" {
		record.Id = uuid.New().String()
	}

	// Set timestamps if not provided
	if record.CreatedAt.IsZero() {
		record.CreatedAt = time.Now()
	}
	if record.UpdatedAt.IsZero() {
		record.UpdatedAt = time.Now()
	}

	_, err := s.db.GetDB().ExecContext(ctx, s.query(`
	INSERT INTO migrations (id, session_id, table_name, status, last_offset, last_cursor, row_count, error_message, created_at, updated_at)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(session_id, table_name) DO UPDATE SET
		status=excluded.status,
		last_offset=excluded.last_offset,
		last_cursor=excluded.last_cursor,
		row_count=excluded.row_count,
		error_message=excluded.error_message,
		updated_at=excluded.updated_at
	`), record.Id, record.SessionId, record.TableName, record.Status, record.LastOffset, record.LastCursor, record.RowCount, record.ErrorMessage, record.CreatedAt, record.UpdatedAt)
	return err
}

func (s *Storage) GetMigration(ctx context.Context, sessionId string, tableName string) (*MigrationRecord, error) {
	row := s.db.GetDB().QueryRowContext(ctx, s.query("SELECT id, session_id, table_name, status, last_offset, last_cursor, row_count, error_message, created_at, updated_at, deleted_at FROM migrations WHERE session_id = ? AND table_name = ?"), sessionId, tableName)
	var rec MigrationRecord
	err := row.Scan(&rec.Id, &rec.SessionId, &rec.TableName, &rec.Status, &rec.LastOffset, &rec.LastCursor, &rec.RowCount, &rec.ErrorMessage, &rec.CreatedAt, &rec.UpdatedAt, &rec.DeletedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &rec, nil
}

func (s *Storage) ListMigrations(ctx context.Context) ([]MigrationRecord, error) {
	rows, err := s.db.GetDB().QueryContext(ctx, "SELECT id, session_id, table_name, status, last_offset, last_cursor, row_count, error_message, created_at, updated_at, deleted_at FROM migrations")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var records []MigrationRecord
	for rows.Next() {
		var r MigrationRecord
		err := rows.Scan(&r.Id, &r.SessionId, &r.TableName, &r.Status, &r.LastOffset, &r.LastCursor, &r.RowCount, &r.ErrorMessage, &r.CreatedAt, &r.UpdatedAt, &r.DeletedAt)
		if err != nil {
			return nil, err
		}
		records = append(records, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return records, nil
}
