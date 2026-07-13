package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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

	db, err := sql.Open(string(cfg.Driver), cfg.BuildDsn())
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
