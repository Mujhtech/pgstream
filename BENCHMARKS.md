# Benchmarks

Measured on a real production database backup (7 tables, ~1.6M rows, ~2.35GB of InnoDB data, dominated by a 1.34M-row API-log table with three `longtext` columns per row). The dataset also carried real-world warts — `varchar(36)` UUID primary keys, `decimal(30,16)` money columns, seven `ENUM` columns including one containing MySQL's empty-string invalid-enum marker, and eight foreign keys pointing at tables absent from the backup. Table names below are anonymized; row counts, sizes, and timings are as measured. A later run of the **complete 83-table production database over the network** is reported in its own section below.

## Environment

- Apple M2 Pro, 16 GB RAM, Docker Desktop (source, target, and pgstream on the same host)
- Source: MySQL 8.4 (`mysql:8`, `innodb-buffer-pool-size=2G`)
- Target: PostgreSQL 16 (`postgres:16`, `shared_buffers=1GB`, `max_wal_size=4GB`)
- pgstream `session` defaults: `--batch-size 5000`, secondary indexes and foreign keys created after data load
- The `copy` run executed **first** (cold caches), the `insert` run second (warm caches), so the comparison is conservative in COPY's favor

## Results

End-to-end migration (schema + data + indexes + foreign keys), identical source, fresh target schema per run:

| Load method | Total wall time | Speedup |
| --- | --- | --- |
| `--load-method insert` (multi-row INSERT) | 90.9 s | 1.0× |
| `--load-method copy` (default) | **49.4 s** | **1.84×** |

Per-table data-copy throughput:

| Table | Rows | insert rows/s | copy rows/s | copy speedup |
| --- | --- | --- | --- | --- |
| api_request_logs (wide, 3× longtext) | 1,341,785 | 17,088 | 34,783 | 2.04× |
| ledger_entries (narrow, decimal-heavy) | 262,598 | 49,701 | **121,745** | 2.45× |
| payout_recipients | 4,628 | 47,116 | 44,040 | ~1× |
| bill_payments | 2,953 | 20,234 | 22,335 | ~1× |

The advantage grows with row count: small tables are dominated by per-table setup (snapshot queries, metadata loading, index/FK creation), large tables by the wire/parse cost that COPY eliminates.

## Correctness verification

Both load methods were verified against the MySQL source after migration:

- Per-table `COUNT(*)` matched exactly for all 7 tables (0 / 17 / 262,598 / 228 / 4,628 / 2,953 / 1,341,785).
- `SUM` over every `decimal(30,16)` column matched to all 16 decimal places.
- **Full-content checksum: every row and every column of every table** (1,612,209 rows including ~2.3GB of longtext) was rendered with normalized types (booleans, UUIDs, timestamps, NULL sentinels), hashed per row in byte order, and compared across engines — all 7 tables matched exactly.
- All satisfiable foreign keys and all 20 indexes were created identically in both schemas.

## Edge cases this dataset surfaced (now handled)

1. **Truncated backups.** The dump ends mid-INSERT with no mysqldump completion footer — the backup was interrupted while dumping `api_request_logs` (which is also why only the alphabetically-first subset of tables exists). The MySQL client silently discards an unterminated trailing statement, so a truncated dump loads "successfully" with missing rows. pgstream migrates whatever the source actually contains; verifying the backup itself (footer present, `mysqldump` exit code) remains the operator's job before trusting any migration of it.
2. **Partial backups with dangling foreign keys.** 8 FK constraints referenced tables that don't exist in the source. pgstream previously failed at the schema-object stage after copying all data. It now skips constraints whose referenced table is missing **from the source** (they are unenforceable anywhere), warns loudly, and writes their exact `ALTER TABLE ... ADD CONSTRAINT` DDL to `pgstream-manual-<session>.sql` for later execution. `--dry-run` reports the same statements up front. Re-running the same session after restoring the missing tables retries them automatically.
3. **Staged partial migrations split UUID types.** When the missing tables are restored and the session resumed, their `varchar(36)` primary keys convert to native `UUID` — but the referencing columns were already migrated as `VARCHAR(36)` back when the target tables were absent. FK creation between `uuid` and `varchar` is impossible, so pgstream now pre-checks column-type compatibility and fails with the exact remediation statement (`ALTER TABLE ... ALTER COLUMN ... TYPE uuid USING ...::uuid`). Applying the suggested casts and resuming created all 12 constraints in the end-to-end test.
4. **MySQL's empty-string invalid-enum marker.** One production row held `''` in a `NOT NULL` enum column — MySQL's artifact for invalid inserts in non-strict mode. pgstream now detects the marker in source data, adds `''` as a label to the created PostgreSQL enum type so every row copies losslessly, and warns to clean the rows up after migration. `--dry-run` reports the same finding before any writes.

## Full production database over the network

The complete production database behind the backup above — **83 tables, 8,539,914 rows**, on managed MySQL (RDS) reached over a WAN — migrated end to end in **10m8s** with zero manual intervention:

| Phase | Wall time |
| --- | --- |
| Table structures (incl. enum types and comments) | 1m12s |
| Data copy (`--workers 5 --batch-size 10000`) | 5m53s — **24,194 rows/s aggregate** |
| ~160 secondary indexes and 135 foreign keys | 2m30s |

COPY loading and wire compression were at their defaults. `FLUSH TABLES WITH READ LOCK` is denied on RDS, so the five worker snapshots aligned **lock-free**, verified by an unchanged binlog position across the snapshot window.

What the run confirmed:

- **The largest table still bounds the wall clock.** The data phase took 5m53s; the dominant table (`api_request_logs`, 2,078,212 rows) alone took 5m47s. The other 82 tables — 6.5M rows — finished entirely inside its shadow, exactly the `--workers` ceiling described below. Aggregate throughput was still ~7× the earlier single-worker WAN figure, because everything else rode along for free.
- **The copy stayed network-bound.** Source read was 87–100% of every per-table time breakdown; target write never exceeded 25%. Per-table throughput spanned 126 rows/s (a small table of very wide rows) to 18,355 rows/s (a 971,560-row narrow token table) — rows/s is a row-shape-dependent metric.
- **Every data-quality safeguard fired on real data in a single run**: six enum columns carrying MySQL's empty-string invalid-enum marker (from 1 to 201 rows each); four `varchar(36)` id columns kept as varchar because a data scan proved they are not UUIDs; one UUID-shaped key group anchored to varchar by a `varchar(255)` foreign-key column (the mixed-shape constraint case); zero dates migrated as NULL in two temporal columns; two keyless tables copied in one streaming pass; and 7 objects (`ON UPDATE CURRENT_TIMESTAMP` trigger DDL, one generated column) written to the manual-work file for review. All 135 satisfiable foreign keys were created on the first attempt.

## Concurrency (pipeline and `--workers`)

Two independent mechanisms, measured separately:

**Read/write pipelining** (always on): each table's next MySQL batch is read while the previous batch COPYs into PostgreSQL, preserving checkpoint ordering exactly. On the production backup above (single dominant table, where table-level parallelism cannot help), the pipeline alone cut the end-to-end run from 49.4s to **43.2s** (~13%).

**`--workers N` table-level parallelism**: N aligned consistent snapshots, tables pulled from a shared queue. Measured on a balanced synthetic dataset (6 tables × 400,000 rows × ~420-byte rows, ~1GB total; alternating run order to cancel cache-warming bias):

| Configuration | Data-copy phase | Speedup |
| --- | --- | --- |
| `--workers 1` | 23.5–26.8 s | 1.0× |
| `--workers 4` | **15.0–17.3 s** | **~1.6×** |

Per-table row counts and content checksums were identical between the parallel and sequential runs, and the 4-worker end-to-end run was also repeated under Go's race detector with zero findings.

### When does `--workers` help?

`--workers` parallelizes across **tables** — each worker copies whole tables, one at a time, pulled from a shared queue. A single table is never split between workers. That has a hard consequence: **total wall time can never drop below the time to copy your largest table**, because exactly one worker carries it regardless of the worker count.

- **Many similarly sized tables** (the synthetic dataset above): all workers stay busy for the whole copy → real speedup (~1.6× measured).
- **One table dominating the data** (the production dump, where `api_request_logs` is ~95% of the bytes): the small tables finish in seconds, the remaining workers idle, and wall time is still ≈ the giant table. Worse, while the small tables copied alongside it, they competed with the giant table for the same disk reads, WAL writes, and CPU — so the 4-worker run measured slightly *slower* than 1 worker. Parallelism had a cost but nothing to parallelize.

For the single-giant-table case, the mechanism that helps is the **always-on read/write pipeline** — parallelism *inside* a table rather than across tables: the next batch is read from MySQL while the previous batch is written to PostgreSQL, overlapping the two halves of every batch cycle. That is what took the production dump from 49.4s to 43.2s at any worker count.

One caveat on the absolute numbers: source, target, and pgstream all shared a single machine here, which is exactly the setup where cross-worker I/O contention hurts most. In a production topology (source database, target database, and the migration host on separate machines, each with its own disk and CPU), the contention penalty shrinks and the parallel ceiling rises — expect `--workers` to scale better there than in these local measurements.

## Hot-path micro-optimizations

The per-row validation/transform loop and the INSERT statement builder were profiled and optimized (Go benchmarks in `internal/migrator/hotpath_bench_test.go`, compared with `benchstat`, n=6, Apple M2 Pro; batch of 1,000 rows × 9 mixed-type columns):

| Function | Metric | Before | After | Change |
| --- | --- | --- | --- | --- |
| `validateAndTransformData` | CPU | 1032 µs | **548 µs** | −46.9% (p=0.002) |
| | allocations | 20,202 | 15,206 | −24.7% |
| `buildPostgresInsert` | CPU | 852 µs | **168 µs** | −80.3% (p=0.002) |
| | allocations | 20,806 | **51** | −99.75% |

What changed:

1. **Per-column plans instead of per-cell dispatch.** The column list is identical for every batch of a table, so target metadata is now resolved once into a compact plan (name, kind, nullability, enum set). The per-cell loop previously did a `strings.ToLower` (one allocation per cell), a map lookup, and a cascade of string comparisons — ~18M of each for the production dump's largest table.
2. **In-place transformation.** Rows are freshly scanned and owned by the pipeline stage, so values are converted in place instead of copying every row into a second slice.
3. **Scan arena.** Each read batch allocates one flat backing array for all row slices and reuses one scan-pointer slice, replacing two allocations per row with two per batch.
4. **Placeholder builder.** The INSERT VALUES clause is built into a single `strings.Builder` with `strconv`-formatted placeholders instead of one `fmt.Sprintf` per placeholder plus per-row joins.
5. **One scan for all enum marker checks.** The empty-string invalid-enum-marker check now counts every enum column in a single table scan (`SUM(col='')` per column) instead of one full scan per column — previously two full scans of the 2.3GB log table.

Remaining transform allocations are almost entirely the mandatory `[]byte → string` copies of text values out of the driver's buffers. Full-content checksum verification against the production dump was re-run after these changes to confirm byte-identical results.

Reproduce with any MySQL dump: load it into a disposable MySQL, run `pgstream session --dry-run` first, then time `--load-method insert` vs `--load-method copy` into separate target schemas.
