# CodeHolic Database Project Report

## 1. Executive summary

This repository contains `godb`, a small local database project written in Go. Its main program is a persistent key/value store backed by a checksummed append-only file. A separate SQL-like command-line application builds table and row operations on top of that storage engine.

The project also contains a separate Windows desktop terminal-emulator prototype under `terminal-emulator/`. It is independent of the database and is not needed to use the SQL shell. The SQL shell itself is a native terminal application; its banner, prompt, and result messages use terminal text and ANSI colors rather than a browser or desktop window.

The implementation has useful durability, recovery, locking, verification, compaction, and stress-test mechanisms. Its tests are meaningful regression coverage, but they do not prove safety under every crash, power-loss, storage-device, or operating-system failure. Treat this as a learning/prototype project, not as a production database for valuable data without additional review and testing.

## 2. Repository map

| Path | Purpose |
|---|---|
| `main.go` | Root key/value CLI: `open`, `put`, `get`, `del`, `batch`, `dump`, `check`, `compact`, and `serve`. |
| `internal/storage/` | Append-only log engine, record format, recovery, locking, quotas, compaction, verification, and storage tests. |
| `extras/sqlshell/main.go` | Entry point for the interactive SQL-like shell. |
| `extras/shell/` | Reads terminal input, handles shell commands, calls the parser/executor, and formats output. |
| `extras/sql/` | SQL-like lexer and parser; converts one command into a typed command or condition tree. |
| `extras/executor/` | Executes parsed commands against the catalog and storage engine. |
| `extras/catalog/` | Table schemas, column validation, row encoding/decoding, and storage-key layout. |
| `cmd/torture/` | Process-kill/recovery stress-test runner for the root key/value CLI. |
| `PROJECT_REPORT.md` | This overview. |
| `STORAGE_TEST_REPORT.md` | More detailed review of storage unit-test scope and its coverage gaps. |
| `terminal-emulator/` | Optional, separate Wails Windows desktop terminal prototype. |

The root Go module is named `godb` and declares Go 1.24 in `go.mod`. The root module has no third-party Go dependencies. The optional desktop app is its own Go/Node subproject with separate dependencies.

## 3. Two ways to use the database

### SQL-like shell

The SQL shell is the interactive table interface most users should start with:

```powershell
Set-Location 'D:\path\to\Srinathonn'
go run ./extras/sqlshell .\mydb.data
```

At the styled `CH godb>` prompt, enter SQL-like commands such as `SHOW TABLES`, `CREATE TABLE ...`, and `SELECT ...`. Type `HELP` for the in-shell command list, or `EXIT` to close the shell. The data file persists and can be reopened by passing the same path again.

To build and run a standalone SQL-shell executable:

```powershell
go build -o sqlshell.exe ./extras/sqlshell
.\sqlshell.exe .\mydb.data
```

Run Go commands such as `go run` and `go build` at the PowerShell prompt (`PS ...>`), not at the database prompt (`CH godb>`).

### Root key/value CLI

The repository root's `main.go` is a different command-line interface, not the SQL shell. It requires a subcommand:

```powershell
go run . open .\mydb.data
go run . put .\kv.data 6e616d65 41736861
go run . get .\kv.data 6e616d65
go run . dump .\kv.data
go run . check .\kv.data
go run . compact .\kv.data
```

The key/value CLI takes hexadecimal keys and values. In the example, `6e616d65` is `name` and `41736861` is `Asha`; `get` prints the value in hex. The SQL shell stores its metadata and rows under its own key prefixes in the same underlying file format. Prefer using one interface consistently for a given database file.

The root CLI also supports `del`, `batch`, and `serve`. Its optional `--max-bytes N` flag caps database size for supported operations; `serve --concurrent` allows concurrent request handling with replies that may be reordered. See the usage comment and argument validation in `main.go` for exact forms and exit codes.

## 4. SQL-like language reference

Enter one command per line at the SQL prompt. A trailing semicolon is optional. A line ending with `\` continues onto the next line. The parser accepts one statement at a time; it does not run general multi-statement SQL scripts.

### Table and schema commands

```sql
CREATE TABLE name (column TYPE, ...)
DROP TABLE name
ALTER TABLE name ADD COLUMN column TYPE
ALTER TABLE name DROP COLUMN column
SHOW TABLES
DESCRIBE name
```

Supported column types are `INT`, `FLOAT`, `TEXT`, and `BOOL`. Names and keywords are case-insensitive and table/column names are normalized to lowercase by the catalog. `DESCRIBE` also accepts `DESC`.

### Insert rows

```sql
INSERT INTO name VALUES (value, ...)
INSERT INTO name (column, ...) VALUES (value, ...)
INSERT INTO name VALUES (value, ...), (value, ...)
```

Each `INSERT` targets one table. Without a column list, values must match the table's column order and count. With a column list, values must match that list. A multi-row insert is committed as a batch along with the table's next-row counter.

### Select rows

```sql
SELECT * FROM name
SELECT column, ... FROM name
SELECT column, ... FROM name WHERE condition
```

The implementation scans rows for the selected table. It has no indexes or `ORDER BY`; result ordering should not be treated as an SQL ordering guarantee.

### Update and delete

```sql
UPDATE name SET column = value WHERE condition
DELETE FROM name WHERE condition
```

Multiple update assignments can be comma-separated. Values are literals; expressions such as `age = age + 1` are not implemented. Omitting `WHERE` is accepted: an `UPDATE` then matches all rows, and a `DELETE` removes all rows in the selected table. Check the target table and condition carefully before using either form without `WHERE`.

### Literals and conditions

Values may be integer, floating-point, text, boolean (`TRUE` or `FALSE`), or `NULL` literals. Text uses single quotes; double an apostrophe to include it inside a string, for example `'it''s text'`.

Conditions support comparisons `=`, `!=`, `<>`, `<`, `>`, `<=`, and `>=`, plus `IS NULL`, `IS NOT NULL`, parentheses, `AND`, and `OR`. `AND` binds more tightly than `OR`.

Examples:

```sql
SELECT * FROM products WHERE price >= 10
SELECT * FROM users WHERE name = 'Ravi'
SELECT * FROM users WHERE active = TRUE
SELECT * FROM users WHERE name IS NOT NULL
SELECT * FROM products WHERE price > 10 AND in_stock = TRUE
SELECT * FROM users WHERE name = 'Ravi' OR name = 'Asha'
```

This is a deliberately small SQL-like language, not a complete SQL implementation.

### Shell-only commands

```text
HELP       show SQL and shell command help
.help      show shell command help
.verify    read-only log integrity check
.compact   rewrite the log to retain current live data
EXIT       leave the shell (also QUIT, .exit, .quit)
```

Only one process can open a given database at a time. The OS-managed `.lock` sidecar is used while open; do not delete or replace it while the database is running.

## 5. Terminal prompt appearance

The SQL shell writes directly to the terminal. It prints a CodeHolic banner, an orange-accented `CH godb>` prompt, highlighted table headings, and colored success, error, and status messages when output is an interactive terminal. ANSI color is disabled when `NO_COLOR` is set, `TERM=dumb`, or output is redirected/non-terminal. The prompt change does not change SQL syntax or database behavior.

The shell is not an interactive full-screen terminal emulator: it does not provide panes, tabs, a dashboard, command history editing, or a virtual keyboard. Those are different application features.

## 6. Architecture and data flow

```text
SQL shell entry point
  -> open selected local database file
  -> read one command from stdin
  -> lexer -> parser -> typed command
  -> executor
       -> catalog reads/validates schema and rows
       -> storage engine commits key/value changes
  -> print result/error to stdout
  -> EXIT or end of input
  -> close database and release process lock
```

### Catalog layout

The catalog stores:

- `meta/<table>`: JSON-encoded table schema, including the next row/column IDs.
- `row/<table>/<row-id>`: JSON-encoded row values, keyed by permanent numeric column IDs.

Permanent column IDs make schema changes relatively cheap. Existing rows have no value for a newly added column and read it as `NULL`; values for a dropped column are ignored. Re-adding a dropped column uses a new ID so the old value does not silently reappear.

### Storage engine

The storage engine loads the log into an in-memory Go map during open and updates it while the process runs. The file is an append-only sequence of framed records containing metadata such as operation type, sequence/batch identifiers, lengths, checksum, key/value data, and framing markers. Record sizes are bounded (the record code defines a 1 MiB maximum value and a maximum batch operation count).

A standalone put is appended and synchronized before the in-memory state is changed. Deletes of missing keys are no-ops. Batches accumulate changes, append a commit marker, and synchronize; replay applies a batch only when its commit record and operation count are valid. SQL multi-row inserts and row mutations use batches where appropriate.

The engine serializes in-process access with a mutex and uses platform-specific locking to reject a second process opening the same database. This lock does not implement multi-process transactions. The catalog is documented as not safe for concurrent goroutine use.

### Recovery, verification, and compaction

On open, the engine replays valid standalone operations and committed batches. Incomplete trailing data and uncommitted batches are excluded from live state and handled as a tail-recovery case. Detected middle-of-log corruption is reported instead of intentionally truncating through later valid data.

`.verify` scans the log without repairing it and reports file size, valid bytes, record/batch counts, live-key count, and any trailing bytes. It verifies storage-log structure; it does not validate every SQL schema or decode every row.

`.compact` writes current live keys to a temporary file, synchronizes and checks the result, then replaces the original using platform-specific replacement logic. Compaction is intended to discard obsolete overwritten/deleted log history. Keep a separate backup of important data; compaction and recovery behavior are not a substitute for backups.

## 7. Root key/value CLI details

The root executable is useful for exercising storage directly without SQL tables:

| Command | Purpose |
|---|---|
| `open <db>` | Open/recover the file and report the current key count. |
| `put <db> <hexkey> <hexval>` | Store a byte-string key and value. |
| `get <db> <hexkey>` | Print the value in hex, or report not found. |
| `del <db> <hexkey>` | Delete a key. |
| `batch <db>` | Read batch operations from stdin and commit them atomically. |
| `dump <db>` | Print sorted key/value pairs in hex. |
| `check <db>` | Perform read-only storage-log verification. |
| `compact <db>` | Compact the storage log. |
| `serve <db>` | Read the documented line protocol from stdin and respond on stdout. |

The CLI also defines exit codes for usage errors, missing keys, disk-full errors, and detected log corruption. Consult `main.go` for the precise output and protocol format.

## 8. Testing and observed verification

### Automated Go tests

From the repository root:

```powershell
go test ./...
go vet ./...
```

During the latest SQL-prompt change, `go test ./...` and `go vet ./...` passed. The SQL shell regression tests exercise a plain-text SQL session and ANSI styling for prompt/errors. The storage tests cover normal reopen, batch replay, tail recovery, selected middle corruption, concurrent in-process writers, compaction/reopen, deterministic operation sequences, and second-process locking.

See [`STORAGE_TEST_REPORT.md`](./STORAGE_TEST_REPORT.md) for the existing detailed test-by-test review and gaps.

### Process-kill torture runs observed on Windows

The separate `cmd/torture` runner was built and run against temporary databases in these modes:

| Mode | Iterations | Observed result |
|---|---:|---|
| `mixed` | 500 | PASS; 30,055 acknowledged operations. |
| `large-values` | 100 | PASS. |
| `large-batches` | 20 | PASS. |
| `recovery-crash` | 100 | PASS; 70 simulated torn tails and 198 recovery-process kills. |
| `compaction` | 100 | PASS. |
| `disk-full` | 100 | PASS; 96 disk-full responses were handled as expected with a 64 KiB cap. |

These are process-termination and simulated-tail tests, not actual power-loss or hardware-failure simulations. The process-kill runner checks acknowledged writes and batch consistency against its in-memory oracle. It is not a proof of production crash safety.

### Optional terminal-emulator checks

The separate `terminal-emulator/` desktop prototype was also built and checked during development. Its Go tests passed, including a Windows ConPTY smoke test that started PowerShell and observed output. Its frontend bundle built successfully, and its desktop process stayed running during a startup smoke test. These results apply to that separate prototype, not to the SQL shell or database durability.

## 9. Separate Windows desktop terminal prototype

`terminal-emulator/` is a standalone Wails desktop application that embeds an xterm.js frontend in a native Windows app window and runs PowerShell using Windows ConPTY. It includes a dark/orange dashboard, system metrics, terminal tabs, and a two-pane split action. It is separate from the SQL shell and database.

Although it uses HTML/JavaScript/CSS inside the native app window, it is not a hosted website or web service. Node.js/npm are needed to build its frontend bundle; the resulting Go application embeds that bundle in the executable. It can be ignored if the goal is only to use or restyle the `godb>` database prompt.

The app's own build and run instructions are in [`terminal-emulator/README.md`](./terminal-emulator/README.md).

## 10. Limitations and non-goals

The inspected parser/executor does not implement joins, indexes, `ORDER BY`, `LIMIT`, aggregates, constraints, foreign keys, prepared statements, authentication/roles, replication, a network database service, or a migration/backup framework. There is no secondary index: SQL row reads and mutations scan table rows, so large-table performance is not established.

The database maintains its current key/value state in RAM. Startup replays the log, and scans can be proportional to stored data. The storage lock is intended to enforce a single open process, not coordinate multiple clients. Platform-specific lock, rename, and directory-sync behavior requires testing on each supported operating system.

Tests do not cover every malformed log boundary, injected short writes/sync failures, every compaction replacement failure, every platform's runtime locking behavior, or genuine power-loss behavior. A process kill does not clear the operating system's file cache. The existing storage report identifies additional test recommendations.

## 11. Summary

The repository is a local Go key/value engine with a small SQL-like table layer and terminal shell. Use `go run ./extras/sqlshell <database-file>` for the interactive SQL interface, and `go run . <subcommand> ...` for direct storage operations. The styled SQL prompt is terminal-native. The optional Wails emulator is a separate desktop app and is not required for database access.

The test and stress results provide useful evidence that the checked scenarios work on the tested Windows machine, but they are not a safety certification or a substitute for backups. Do not assume full SQL compatibility, multi-process access, production-scale performance, or proven behavior under all crash and hardware failure modes.
