# PGStream

PGStream is an early-stage MySQL-to-PostgreSQL migration CLI focused on databases that are too large to load into memory.

The current milestone provides a resumable, bounded-memory copy path. It is still alpha software: test it against a disposable PostgreSQL database and validate the result before a production cutover.

## What works today

- Interactive MySQL and PostgreSQL connection setup
- PostgreSQL schema and table creation with common MySQL type mappings
- Fail-closed errors for unknown types and generated/default behavior that cannot be preserved safely
- MySQL `ENUM` creation in PostgreSQL
- Primary-key preservation, including composite primary keys
- Primary-key keyset pagination for large tables
- Persistent typed cursors and row progress for interrupted migrations
- A read-only MySQL `REPEATABLE READ` snapshot for each copy invocation
- Transactional multi-row PostgreSQL inserts, bounded by PostgreSQL's parameter limit
- Idempotent replay of primary-keyed batches with conflict-targeted upserts and row-count checks
- PostgreSQL identity/serial synchronization that preserves MySQL's next `AUTO_INCREMENT` value
- Secondary, unique, and composite index migration after data loading
- Single- and composite-column foreign keys after data loading
- Authenticated encryption for resumable connection credentials
- SQLite metadata storage by default, with PostgreSQL metadata storage support
- View discovery and explicit manual-conversion reporting

PGStream deliberately skips views, stored functions, and triggers for now. Translating MySQL SQL with string replacement can silently change application behavior, so the CLI reports those objects instead of installing unsafe placeholders.

The same fail-closed rule applies to unsupported column types, generated columns, `ON UPDATE` behavior, and default expressions: PGStream stops with an actionable error instead of silently substituting a different schema.

Tables without a primary key use one streaming source query with bounded PostgreSQL insert batches, avoiding unsafe `OFFSET` boundaries. They still cannot be resumed safely after interruption. Add a stable primary key before migration or truncate the partial target and start a fresh session.

## Build and run

Requirements:

- Go 1.24 or newer
- Network access from the CLI to the source MySQL and target PostgreSQL servers
- InnoDB source tables (non-transactional MySQL engines cannot provide the required consistent snapshot)
- A PostgreSQL role allowed to create the target schema and its objects
- A persistent 32-byte credential-encryption key (raw, base64, or 64-character hex)

```bash
export ENCRYPTION_KEY="$(openssl rand -base64 32)"
go build -o pgstream ./cmd
./pgstream session --batch-size 5000
```

Store `ENCRYPTION_KEY` in a secret manager or a private `.env` file and keep it unchanged for the lifetime of resumable sessions. Losing or changing it makes encrypted session credentials unreadable.

The CLI prints a new session ID for every invocation without `--id`. A fresh session refuses to copy into target tables that already contain rows. If the process is interrupted or a table fails, resume the same primary-keyed migration from its last committed batch:

```bash
./pgstream session --id <session-id> --batch-size 5000
```

Session metadata is stored in `pgstream.db` by default and the CLI restricts that file to owner read/write permissions. It can be moved to PostgreSQL through the `DB_DRIVER`, `DB_HOST`, `DB_PORT`, `DB_USER`, `DB_PASSWORD`, and `DB_DATABASE` environment variables.

The source and target credentials required for resume are stored with authenticated encryption. Existing valid plaintext session records are encrypted the first time they are resumed. Keep both the metadata database and encryption key private, and delete stale session records when they are no longer needed.

Each invocation reads rows from one consistent source snapshot. For an interrupted migration resumed in a later process, keep the source quiescent until cutover: a new invocation necessarily opens a new snapshot and cannot account for changes made between snapshots without CDC/binlog tailing.

For a large source, the long-running snapshot can retain InnoDB undo history. Monitor source purge/undo pressure during the copy, avoid concurrent schema changes, and rehearse the operational window before production cutover.

## Migration order

1. Create the target schema, tables, enum types, and primary keys.
2. Copy table data in batches and checkpoint every committed batch.
3. Create secondary indexes and foreign keys.
4. Report views, triggers, and stored functions that require manual conversion.

Loading data before secondary indexes, foreign keys, and triggers improves throughput and prevents target-side behavior from changing copied rows.

## Verify the repository

```bash
go test ./...
go test -race ./...
go vet ./...

cd webui
npm run lint
npm run build
```

The web UI currently builds but is not connected to the migration engine.

## Near-term roadmap

- End-to-end MySQL/PostgreSQL integration tests with generated large datasets
- Whole-database snapshot/cutover coordination and checksum verification
- Explicit include/exclude table selection and dry-run schema reports
- Faster PostgreSQL `COPY`-based loading
- Safe handling and reporting for every unsupported MySQL type or default
- A server API and live progress stream for the web UI
- Cutover support for ongoing writes (CDC/binlog tailing)

The older feature inventory is retained in [MIGRATION_FEATURES.md](MIGRATION_FEATURES.md) as a product target, not a claim that every item is complete.
