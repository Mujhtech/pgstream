# PGStream

PGStream migrates large MySQL databases to PostgreSQL — COPY loading from consistent snapshots, resumable checkpoints, data-driven type conversion, dry-run planning, and a live web UI. It streams tables in bounded memory, so database size is not limited by RAM, and it has been hardened against a real 83-table production database that it migrates end to end in about ten minutes (see [BENCHMARKS.md](BENCHMARKS.md)).

As with any migration tool: run `--dry-run` first, and validate the result against the source before a production cutover.

## Quick start

```bash
export ENCRYPTION_KEY="$(openssl rand -base64 32)"
make webui              # build the web UI once so it can be embedded (requires bun)
go build -o pgstream ./cmd

./pgstream session --dry-run     # preview the full translation plan, writes nothing
./pgstream session               # run the migration (interactive connection setup)
```

Requirements:

- Go 1.25 or newer
- Network access from the CLI to the source MySQL and target PostgreSQL servers
- InnoDB source tables (non-transactional MySQL engines cannot provide the required consistent snapshot)
- A PostgreSQL role allowed to create the target schema and its objects
- A persistent 32-byte credential-encryption key (raw, base64, or 64-character hex) in `ENCRYPTION_KEY`

## CLI usage

| Command | Purpose |
| --- | --- |
| `pgstream session` | Run or resume a migration (interactive connection setup on first run) |
| `pgstream status` | Show progress of sessions from the session store, live |
| `pgstream serve` | Start the local HTTP server with the JSON API and bundled web UI |
| `pgstream version` | Print the build version |

### `pgstream session`

Starts a migration. Without `--id` it prompts interactively for the MySQL and PostgreSQL connection details, stores them encrypted, prints a new session ID, and begins a fresh migration. A fresh session refuses to copy into target tables that already contain rows.

| Flag | Default | Meaning |
| --- | --- | --- |
| `--id <session-id>` | — | Resume an existing session from its last committed checkpoint |
| `--batch-size <n>` | `5000` | Rows read from MySQL per batch |
| `--include-tables a,b` | — | Migrate only the listed tables |
| `--exclude-tables a,b` | — | Migrate everything except the listed tables |
| `--dry-run` | off | Print the full translation plan and exit; writes nothing |
| `--load-method copy\|insert` | `copy` | How batches are written to PostgreSQL |
| `--workers <n>` | `1` | Tables migrated concurrently (max 16) |
| `--cast <rule>` | — | Override a type mapping, repeatable (see below) |
| `--schema-only` | off | Create tables, indexes, and foreign keys; copy no data |
| `--data-only` | off | Copy data into tables a previous run created; no DDL |
| `--target-tuning` | on | Raise `maintenance_work_mem` on the target for faster index builds |
| `--skip-snapshot-lock` | off | Skip the multi-worker snapshot alignment lock (see below) |
| `--source-compression` | on | zlib compression on the MySQL connection |

Notes on each:

- **`--dry-run`** connects to both databases and prints every planned table, column type mapping, enum, index, and foreign key, plus warnings (generated columns, `ON UPDATE CURRENT_TIMESTAMP`, invalid-enum markers in the data, dangling foreign keys) and blocking issues. It exits non-zero when the plan has blocking issues, so it works as a CI/pre-flight gate. Without `--id` it needs neither `ENCRYPTION_KEY` nor session storage.
- **`--include-tables` / `--exclude-tables`** are mutually exclusive and fail closed: unknown table names error, and a selection whose foreign keys point outside the selection errors before anything is written.
- **`--load-method copy`** (default) streams batches through the PostgreSQL COPY protocol; the first batch after a resume still uses conflict-targeted inserts so replayed rows stay idempotent. `insert` keeps multi-row INSERTs. On a real 1.6M-row production backup, COPY cut end-to-end migration time by 1.84× and doubled large-table throughput — see [BENCHMARKS.md](BENCHMARKS.md).
- **`--id`** resumes primary-keyed tables from their last committed batch. Keyless tables cannot resume; truncate their partial target and re-run. Resuming is also how skipped foreign keys are retried after missing source tables are restored.
- **`--cast`** overrides the built-in type mapping, in the spirit of pgloader's CAST clauses. Two rule forms, both repeatable: `table.column=TYPE` pins one column (highest precedence), and `mysqltype=TYPE` rewrites every column whose MySQL type starts with that prefix (first matching rule wins). Typical uses: `--cast 'tinyint(1)=smallint'` when a "boolean" column actually stores 0–2, `--cast 'datetime=timestamptz'` to get timezone-aware timestamps, `--cast 'jobs.id=text'` to keep a UUID-shaped key as text. A cast is authoritative: it replaces the built-in mapping, exempts the column from native-UUID conversion, and data validation follows the cast type. `--dry-run` lists every applied cast per column.
- **`--schema-only` / `--data-only`** split a migration the way pgloader's `schema only`/`data only` options do. `--schema-only` runs table creation and all schema objects (indexes, foreign keys, comments, manual-work DDL) without copying a row — useful for reviewing or hand-tweaking the target schema before committing to a long load. `--data-only` then copies data into that existing schema, verifying every table first. Caveat: a full run creates foreign keys *after* the data on purpose; a data-only load runs with any existing constraints active, so it warns when the target already has foreign keys — child rows loading before their parents would fail. When that happens, drop the constraints, load, and restore them.
- **`--target-tuning`** raises `maintenance_work_mem` to 128MB (pgloader's default) on the migration's PostgreSQL connections, which speeds up the post-copy index builds on large tables. It deliberately does not touch durability settings like `synchronous_commit`, because resume checkpoints assume acknowledged commits survive a target crash. Poolers that reject startup options (PgBouncer) fall back to an untuned connection automatically.
- **`--workers N`** copies up to N tables concurrently, each on its own MySQL connection with a consistent snapshot, all observing the same point in time. Alignment tries two strategies automatically: a brief `FLUSH TABLES WITH READ LOCK` (milliseconds; needs the `RELOAD` privilege), then — when that is unavailable, as on RDS and most managed MySQL — **lock-free verified alignment**: the binlog position is captured before and after opening the snapshots, and identical positions prove no transaction committed in between (needs `REPLICATION CLIENT`, retried a few times on a busy source). If neither works, pass `--skip-snapshot-lock` to assert the source receives no writes, or run with `--workers 1`. Index creation also parallelizes across tables; foreign keys stay sequential to avoid PostgreSQL lock conflicts. Independent of worker count, every table copy pipelines its MySQL reads with its PostgreSQL writes, so even `--workers 1` is faster than earlier releases. Because workers copy whole tables, wall time can never drop below your largest table's copy time: raise `--workers` when the schema has several similarly sized tables, and leave it at 1 when one table holds most of the data (see [BENCHMARKS.md](BENCHMARKS.md)).

Interrupting a run with Ctrl+C is safe: the migration stops after the current batch, progress stays checkpointed, the interruption is recorded in the session log, and the CLI prints the exact resume command. A second Ctrl+C terminates immediately.

Typical workflow:

```bash
./pgstream session --dry-run                          # 1. preview and fix reported issues
./pgstream session --batch-size 5000                  # 2. migrate; note the printed session ID
./pgstream status --id <session-id>                   # 3. watch from another terminal
./pgstream session --id <session-id>                  # 4. resume if interrupted
```

Every run may also produce `pgstream-manual-<session-id>.sql` in the working directory: DDL that pgstream deliberately did not execute (foreign keys whose referenced table is missing from the source, `ON UPDATE CURRENT_TIMESTAMP` trigger equivalents, generated-column templates awaiting expression translation), each with the reason. Review and apply it after resolving each blocker.

### `pgstream status`

Reads the session store only — no database credentials or `ENCRYPTION_KEY` needed.

```bash
./pgstream status                  # list sessions: status, tables done, rows copied
./pgstream status --id <session>   # per-table progress plus the recent engine log
```

Progress checkpoints and the engine log update after every committed batch, so this observes a migration currently running in another terminal or in the server.

### `pgstream serve`

| Flag | Default | Meaning |
| --- | --- | --- |
| `--addr` | `127.0.0.1:8080` | Listen address |
| `--config` | `.env` | Configuration file loaded before environment variables |

Serves the bundled web UI and the JSON API (see [Server and web UI](#server-and-web-ui)). Binds to localhost by default because migration requests carry database credentials.

## Configuration

`pgstream session`, `status`, and `serve` load `.env` from the working directory (or `--config` for `serve`), then read environment variables:

| Variable | Default | Meaning |
| --- | --- | --- |
| `ENCRYPTION_KEY` | — (required to migrate) | 32-byte key protecting stored session credentials |
| `DB_DRIVER` | `sqlite3` | Session-store driver: `sqlite3` or `postgres` |
| `DB_PATH` | `pgstream.db` | SQLite session-store path |
| `DB_HOST` / `DB_PORT` / `DB_USER` / `DB_PASSWORD` / `DB_DATABASE` | — | PostgreSQL session store (when `DB_DRIVER=postgres`) |
| `PORT` | — | Overrides the serve port when `--addr` is not given |
| `LOG_LEVEL` | `info` | `debug` or `trace` for verbose server logging |

The session store holds session records, per-table checkpoints, and the persisted engine log. **The CLI and server must share the same store to see each other's sessions** — run both from the same directory or point both at it via `DB_PATH` (or the `DB_*` variables). The SQLite file is restricted to owner read/write.

Store `ENCRYPTION_KEY` in a secret manager or a private `.env` file and keep it unchanged for the lifetime of resumable sessions — losing or changing it makes encrypted session credentials unreadable. Legacy plaintext session records are encrypted the first time they are resumed. Delete stale session records when no longer needed.

## What works today

- Interactive MySQL and PostgreSQL connection setup with encrypted, resumable credentials
- PostgreSQL schema and table creation with common MySQL type mappings; MySQL `ENUM` creation
- Primary-key preservation (including composite keys) and keyset pagination for large tables
- Persistent typed cursors and per-batch checkpoints for interrupted migrations
- A read-only MySQL `REPEATABLE READ` snapshot per copy invocation
- PostgreSQL `COPY` bulk loading with idempotent, conflict-targeted replay after resume
- Identity/serial synchronization preserving MySQL's next `AUTO_INCREMENT` value
- Secondary/unique/composite index and single/composite foreign-key migration after data load
- Table, column, and index comment migration (`COMMENT ON`)
- Phase control (`--schema-only`/`--data-only`) and user type-mapping overrides (`--cast`)
- Table selection, dry-run planning, SQLite or PostgreSQL session storage
- A local HTTP server with a JSON API, live log streaming, and a bundled web UI

## How tricky schemas are handled

The guiding rule is fail-closed: stop with an actionable error instead of silently substituting a different schema. Where a lossless path exists, pgstream takes it and reports the rest as reviewed manual work:

- **Views, stored functions, triggers** are discovered and reported for manual conversion; translating MySQL SQL by string replacement can silently change behavior, so nothing unsafe is installed.
- **Dangling foreign keys** (referenced table missing from the source — partial or truncated backups) are skipped with their exact `ALTER TABLE` DDL saved to the manual-work file; re-running the session after restoring the missing tables retries them automatically. If a staged migration split column types (`VARCHAR(36)` vs `UUID`), the error includes the exact `ALTER COLUMN ... TYPE uuid USING` statement to reconcile them.
- **`ON UPDATE CURRENT_TIMESTAMP`** columns migrate normally; the auto-update behavior becomes generated trigger DDL (faithful to MySQL: refresh only when the column was not set explicitly) in the manual-work file.
- **Generated columns** (VIRTUAL and STORED) migrate as plain columns holding the values MySQL had computed at snapshot time; the manual-work file contains a `GENERATED ALWAYS AS (...) STORED` template with the original MySQL expression, deliberately unrunnable until translated.
- **Invalid-enum markers**: rows storing MySQL's empty-string marker (invalid values inserted outside strict SQL mode) are detected in the data, counted, and preserved by adding `''` to the PostgreSQL enum type, with a warning telling you how to find and clean them.
- **MySQL zero dates** (`0000-00-00 00:00:00`, allowed by pre-strict SQL modes) have no PostgreSQL representation. They migrate as `NULL` when the target column is nullable (with a warning and an inspect query); a `NOT NULL` target fails closed with remediation guidance. `--dry-run` counts them per column up front, reporting nullable columns as warnings and `NOT NULL` columns as blocking issues.
- **UUID conversion is decided by the data, not just the shape.** `char(36)`/`varchar(36)` keys usually hold UUIDs, but not always (application-defined string keys are common). Before converting, pgstream scans the candidate column in one pass; any non-UUID value keeps the original `VARCHAR` type (lossless) with a warning and an inspect query. Foreign-key columns follow their referenced column's decision so constraint types stay compatible — including mixed-shape constraints, where a wider column (say `varchar(64)`) references a `char(36)` key: the key group then keeps its original type so the constraint stays creatable, with `--cast 'table.column=uuid'` as the opt-in when the wider column's data is really UUIDs. `--dry-run` reports every demotion before anything is created.
- **NUL bytes in strings** (`0x00`, legal in MySQL VARCHAR/TEXT, impossible in PostgreSQL text) are removed during copy with a per-column warning and an inspect query — the byte has no target representation, so removal is the only lossless-as-possible path. Binary columns keep them (`BYTEA` stores NUL fine), and `--dry-run` counts affected rows per column up front.
- **`BIGINT UNSIGNED` is decided by the data.** Its lossless mapping is `NUMERIC(20)` (the type spans 0..2^64-1), but numeric can neither back an identity sequence nor join a foreign key against integer columns — so `bigint unsigned AUTO_INCREMENT` keys, one of the most common MySQL patterns, would fail closed. Before creating tables, pgstream scans each unsigned bigint column (grouped with every column its foreign keys connect it to, so both ends of a constraint always agree) and maps the group to `BIGINT` when `MAX()` proves the data fits the signed 63-bit range. Columns holding larger values keep `NUMERIC(20)` with a warning; a pre-existing target that disagrees with this run's decision fails fast with the exact reconciling `ALTER`.
- **MySQL `TIME` is a duration** (`-838:59:59` to `838:59:59`), not a clock time. Values PostgreSQL `TIME` can hold copy normally; out-of-range values stop the migration with a pointer to `--cast 'table.column=interval'`, which stores the full duration range losslessly. `--dry-run` counts out-of-range values per column as blocking issues.
- **When the built-in mapping is wrong for your data**, `--cast` (inspired by [pgloader](https://pgloader.readthedocs.io/)'s CAST clauses) overrides it per column or per MySQL type — the escape hatch for schemas where `tinyint(1)` is not a boolean, `char(36)` should stay text, or timestamps should land as `timestamptz`.
- **Keyless tables** are copied in one streaming pass with bounded insert batches (no unsafe `OFFSET`), but cannot resume after interruption — add a stable primary key or truncate the partial target and re-run.
- Unknown column types and expression defaults still stop the migration with an actionable error.

## Migration order

1. Create the target schema, tables, enum types, and primary keys.
2. Copy table data in batches and checkpoint every committed batch.
3. Create secondary indexes and foreign keys.
4. Report views, triggers, stored functions, and other manual work.

Loading data before secondary indexes, foreign keys, and triggers improves throughput and prevents target-side behavior from changing copied rows.

## Operational notes

Migration speed is usually bound by the network, not by pgstream: a CLI at 1–5% CPU is waiting on the databases. Each completed table logs a time breakdown (`⏱ source read … target write …`) that shows which side dominates. The two biggest levers: run pgstream close to the databases (for cloud sources, a small instance in the same region routinely beats a fast machine over a WAN by an order of magnitude), and keep `--source-compression` on — MySQL wire compression shrinks text-heavy transfers several-fold for idle CPU you already have.

Each invocation reads rows from one consistent source snapshot. For an interrupted migration resumed in a later process, keep the source quiescent until cutover: a new invocation necessarily opens a new snapshot and cannot account for changes made between snapshots without CDC/binlog tailing.

For a large source, the long-running snapshot can retain InnoDB undo history. Monitor source purge/undo pressure during the copy, avoid concurrent schema changes, and rehearse the operational window before production cutover.

## Server and web UI

```bash
export ENCRYPTION_KEY="$(openssl rand -base64 32)"
make webui build
./pgstream serve            # http://127.0.0.1:8080
```

`pgstream serve` starts a local HTTP server that serves the bundled web UI and a JSON API:

- `GET /api/sessions` — list all sessions with aggregate progress, including CLI-driven ones
- `POST /api/sessions` — create a session and start a fresh migration
- `POST /api/sessions/{id}/start` — resume an interrupted session
- `GET /api/sessions/{id}` — run status plus per-table progress records
- `GET /api/sessions/{id}/events` — Server-Sent Events stream of live logs and progress (supports `Last-Event-ID` replay); for sessions running in another process (e.g. the CLI) it tails the persisted engine log from the session store
- `POST /api/dry-run` — full schema translation plan without writing anything

The web UI's session list shows every session with live progress — including CLI-driven migrations — and streams their complete engine log. The server binds to `127.0.0.1` by default because migration requests carry database credentials; put an authenticating reverse proxy in front of it before exposing it further. For web UI development, `npm run dev` in `webui/` proxies `/api` to the local server.

## Verify the repository

```bash
go test ./...
go test -race ./...
go vet ./...

cd webui
npm run lint
npm run build
```

## Near-term roadmap

- End-to-end MySQL/PostgreSQL integration tests with generated large datasets
- Whole-database snapshot/cutover coordination and checksum verification
- Safe handling and reporting for every unsupported MySQL type or default
- Cutover support for ongoing writes (CDC/binlog tailing)
- A pure-Go SQLite driver so release binaries can keep `CGO_ENABLED=0` with the default metadata storage

The older feature inventory is retained in [MIGRATION_FEATURES.md](MIGRATION_FEATURES.md) as a product target, not a claim that every item is complete.
