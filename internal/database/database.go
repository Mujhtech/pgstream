package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/mujhtech/pgstream/config"
)

const (
	dbTx string = "db_tx"
)

type Database struct {
	dbx *sqlx.DB
	cfg config.Database
}

func Connect(ctx context.Context, cfg config.Database) (*Database, error) {

	// The pure-Go SQLite driver (modernc.org/sqlite) registers as "sqlite";
	// the configured driver name stays "sqlite3" for compatibility and for
	// sqlx's placeholder binding, which knows that name.
	openDriver := string(cfg.Driver)
	if cfg.Driver == config.DatabaseDriverSqlite3 {
		openDriver = "sqlite"
	}
	db, err := sql.Open(openDriver, cfg.BuildDsn())
	if err != nil {
		return nil, fmt.Errorf("failed to open the db: %w", err)
	}

	dbx := sqlx.NewDb(db, string(cfg.Driver))

	if err = pingDatabase(ctx, dbx); err != nil {
		_ = dbx.Close()
		return nil, fmt.Errorf("failed to ping the db: %w", err)
	}

	return &Database{
		dbx: dbx,
		cfg: cfg,
	}, nil
}

// targetTuningParameters are the session settings applied to migration-target
// connections, following pgloader's defaults: a raised maintenance_work_mem
// speeds up the post-copy CREATE INDEX phase. Only allocation limits belong
// here — settings that trade durability for speed (synchronous_commit) do
// not, because pgstream's resume checkpoints assume acknowledged commits
// survive a target crash.
const targetTuningParameters = "-c maintenance_work_mem=128MB"

// ConnectPostgresTarget connects to a migration target, optionally applying
// session tuning to every pooled connection via the startup "options"
// parameter. Connection poolers such as PgBouncer reject startup options, so
// a rejection falls back to an untuned connection instead of failing.
func ConnectPostgresTarget(ctx context.Context, cfg config.Database, tune bool) (*Database, error) {
	if tune {
		tuned := cfg
		tuned.Options = joinOptions(cfg.Options, "options="+url.QueryEscape(targetTuningParameters))
		db, err := ConnectPostgresPreferSSL(ctx, tuned)
		if err == nil || !isStartupParameterUnsupported(err) {
			return db, err
		}
	}
	return ConnectPostgresPreferSSL(ctx, cfg)
}

// ConnectPostgresPreferSSL implements sslmode=prefer semantics for lib/pq,
// which does not support "prefer" natively: attempt an SSL connection first
// and fall back to plaintext only when the server has SSL disabled.
func ConnectPostgresPreferSSL(ctx context.Context, cfg config.Database) (*Database, error) {
	secure := cfg
	secure.Options = joinOptions(cfg.Options, "sslmode=require&connect_timeout=30")
	db, err := Connect(ctx, secure)
	if err == nil {
		return db, nil
	}
	if !isPostgresSSLUnsupported(err) {
		return nil, err
	}

	insecure := cfg
	insecure.Options = joinOptions(cfg.Options, "sslmode=disable&connect_timeout=30")
	return Connect(ctx, insecure)
}

// joinOptions merges query-string fragments, skipping empty parts.
func joinOptions(parts ...string) string {
	nonEmpty := make([]string, 0, len(parts))
	for _, part := range parts {
		if part != "" {
			nonEmpty = append(nonEmpty, part)
		}
	}
	return strings.Join(nonEmpty, "&")
}

// isStartupParameterUnsupported reports a server or pooler rejecting the
// startup "options" parameter, PgBouncer's documented behavior.
func isStartupParameterUnsupported(err error) bool {
	return err != nil && strings.Contains(err.Error(), "unsupported startup parameter")
}

// isPostgresSSLUnsupported reports whether the connection failed because the
// server does not speak SSL, which is lib/pq's deterministic response and the
// only error that justifies a plaintext retry.
func isPostgresSSLUnsupported(err error) bool {
	return err != nil && strings.Contains(err.Error(), "SSL is not enabled on the server")
}

func (d *Database) GetDB() *sqlx.DB {
	return d.dbx
}

func (d *Database) Close() error {
	return d.dbx.Close()
}

func (d *Database) StartTx(ctx context.Context, opts *sql.TxOptions) (*sqlx.Tx, error) {
	return d.dbx.BeginTxx(ctx, opts)
}

func (d *Database) RollbackTx(tx *sqlx.Tx, err error) error {
	if err != nil {
		rollbackErr := tx.Rollback()
		if rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			return errors.Join(err, rollbackErr)
		}
		return err
	}
	return tx.Commit()
}

func (d *Database) GetTx(ctx context.Context, db *sqlx.DB) (*sqlx.Tx, error) {
	ctxTx, ok := ctx.Value(dbTx).(*sqlx.Tx)
	if ok {
		return ctxTx, nil
	}

	tx, err := db.BeginTxx(ctx, nil)

	if err != nil {
		return nil, fmt.Errorf("failed to start a transaction: %w", err)
	}

	return tx, nil
}

func pingDatabase(ctx context.Context, db *sqlx.DB) error {
	var err error
	for i := 1; i <= 30; i++ {
		err = db.PingContext(ctx)

		if errors.Is(err, context.Canceled) {
			return err
		}

		if err == nil {
			return nil
		}

		// A server without SSL support answers deterministically; retrying
		// only delays the caller's plaintext fallback.
		if isPostgresSSLUnsupported(err) {
			return err
		}

		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return ctx.Err()
		case <-timer.C:
		}
	}

	return fmt.Errorf("all 30 tries failed, last failure: %w", err)
}

func (d *Database) GetSchema() string {
	return d.cfg.Schema
}

func (d *Database) GetConfig() config.Database {
	return d.cfg
}
