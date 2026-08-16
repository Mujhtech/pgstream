package storage

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/mujhtech/pgstream/config"
	_ "modernc.org/sqlite"
)

func TestStoragePersistsMigrationCursor(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "metadata.db")
	storage, err := InitStorage(ctx, sqliteConfig(path))
	if err != nil {
		t.Fatalf("initialize storage: %v", err)
	}
	t.Cleanup(func() { _ = storage.db.Close() })

	sessionID, err := storage.CreateSession(ctx, `{"source":"mysql","target":"postgres"}`)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if session, err := storage.GetSessionByID(ctx, sessionID); err != nil || session == nil {
		t.Fatalf("get session: session=%#v err=%v", session, err)
	}
	if err := storage.UpdateSessionConnector(ctx, sessionID, "encrypted-connector"); err != nil {
		t.Fatalf("update session connector: %v", err)
	}
	updatedSession, err := storage.GetSessionByID(ctx, sessionID)
	if err != nil || updatedSession == nil || updatedSession.Connector != "encrypted-connector" {
		t.Fatalf("updated session: session=%#v err=%v", updatedSession, err)
	}
	fileInfo, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat metadata file: %v", err)
	}
	if permissions := fileInfo.Mode().Perm(); permissions != 0o600 {
		t.Fatalf("metadata file permissions are %o; want 600", permissions)
	}

	want := MigrationRecord{
		SessionId:  sessionID,
		TableName:  "events",
		Status:     "in_progress",
		LastOffset: 15_000_000_000,
		LastCursor: `[{"type":"bytes","value":"NDI="}]`,
		RowCount:   20_000_000_000,
	}
	if err := storage.UpsertMigration(ctx, want); err != nil {
		t.Fatalf("upsert migration: %v", err)
	}

	got, err := storage.GetMigration(ctx, sessionID, "events")
	if err != nil {
		t.Fatalf("get migration: %v", err)
	}
	if got == nil {
		t.Fatal("expected migration record")
	}
	if got.LastOffset != want.LastOffset || got.LastCursor != want.LastCursor || got.RowCount != want.RowCount {
		t.Fatalf("migration checkpoint changed\nwant: %#v\n got: %#v", want, *got)
	}

	got.Status = "done"
	if err := storage.UpsertMigration(ctx, *got); err != nil {
		t.Fatalf("complete migration: %v", err)
	}
	reloaded, err := storage.GetMigration(ctx, sessionID, "events")
	if err != nil {
		t.Fatalf("reload migration: %v", err)
	}
	if reloaded.Status != "done" || reloaded.LastOffset != want.LastOffset || reloaded.LastCursor != want.LastCursor {
		t.Fatalf("completion lost checkpoint: %#v", *reloaded)
	}
}

func TestStorageUpgradesLegacyMigrationTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open legacy database: %v", err)
	}
	_, err = db.Exec(`
		CREATE TABLE migrations (
			id TEXT PRIMARY KEY,
			session_id TEXT NOT NULL,
			table_name TEXT NOT NULL,
			status TEXT,
			last_offset INTEGER,
			row_count INTEGER,
			error_message TEXT,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			deleted_at DATETIME DEFAULT NULL,
			UNIQUE(session_id, table_name)
		)
	`)
	if err != nil {
		t.Fatalf("create legacy schema: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close legacy database: %v", err)
	}

	storage, err := InitStorage(context.Background(), sqliteConfig(path))
	if err != nil {
		t.Fatalf("upgrade legacy storage: %v", err)
	}
	defer storage.db.Close()

	var exists bool
	err = storage.db.GetDB().QueryRow(`SELECT EXISTS(SELECT 1 FROM pragma_table_info('migrations') WHERE name = 'last_cursor')`).Scan(&exists)
	if err != nil {
		t.Fatalf("inspect upgraded schema: %v", err)
	}
	if !exists {
		t.Fatal("expected last_cursor column to be added")
	}
}

func sqliteConfig(path string) config.Database {
	return config.Database{
		Driver: config.DatabaseDriverSqlite3,
		Path:   path,
	}
}
