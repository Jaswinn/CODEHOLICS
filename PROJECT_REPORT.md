# godb Project Report

## 1. Executive summary

`godb` is a small, local, command-line database written in Go. It accepts a limited SQL-like language, stores table schemas and rows in a file-backed key/value log, and reconstructs its in-memory state when the database is opened.

The project is self-contained in the inspected source: the command-line program reads from standard input, writes to standard output, and operates on a caller-selected local file. The inspected Go module declares no third-party dependencies, and the inspected source contains no HTTP, RPC, or database-server client implementation. There is no evidence here of a remote database/API integration.

The code comments describe the engine as “crash-safe,” but the tests do not simulate abrupt process termination, power loss, or failing storage. That label should be treated as an implementation goal, not as independently demonstrated production-grade crash safety.

## 2. What problem it solves

The program provides a compact way to create tables and perform basic row operations without installing or connecting to a separate database server. Its data is kept in a local file, rather than sent to an external API.

The implementation provides:

- Persistent tables and rows using a local append-only log.
- A small SQL-like interactive shell.
- Basic schema changes and row CRUD operations.
- Per-operation durable writes and atomic batches in the storage engine.
- Startup replay, handling of incomplete log tails, log verification, and compaction.
- A single-open-process lock intended to prevent two processes from independently writing the same database.

There is no code evidence that this project is intended to be a general-purpose SQL database, a network service, a multi-user server, or a replacement for a mature database product.

## 3. Architecture

The source is divided into these layers:

| Layer | Source | Responsibility |
|---|---|---|
| Program entry point | [`main.go`](./main.go) | Selects the database path, opens storage, starts the shell, closes storage, and reports errors. |
| Interactive shell | [`internal/shell/shell.go`](./internal/shell/shell.go) | Reads commands from standard input, handles shell-only commands, invokes the parser and executor, and prints results. |
| SQL-like lexer/parser | [`internal/sql/lexer.go`](./internal/sql/lexer.go), [`internal/sql/parser.go`](./internal/sql/parser.go) | Converts one line/statement into tokens and then a typed command or WHERE-expression tree. |
| Command executor | [`internal/executor/executor.go`](./internal/executor/executor.go) | Applies parsed table and row commands using the catalog and storage engine. |
| Schema catalog and row codec | [`internal/catalog/catalog.go`](./internal/catalog/catalog.go) | Stores table metadata, maps rows to engine keys, validates/coerces values, and encodes rows as JSON. |
| Storage engine and record format | [`internal/storage/engine.go`](./internal/storage/engine.go), [`internal/storage/record.go`](./internal/storage/record.go) | Maintains the in-memory key/value map and appends/checks/replays framed records in the database file. |
| Process locking | [`internal/storage/lock_windows.go`](./internal/storage/lock_windows.go), [`internal/storage/lock_unix.go`](./internal/storage/lock_unix.go), [`internal/storage/lock_other.go`](./internal/storage/lock_other.go) | Implements or attempts to implement platform-dependent database locking and file replacement. |

### Data representation

The catalog uses engine keys with these prefixes:

- `meta/<table>` stores a JSON table schema.
- `row/<table>/<row-id>` stores a JSON object whose properties are permanent numeric column IDs.

Permanent column IDs let the schema append or remove columns without rewriting every row: newly added columns read as `NULL` for older rows, and dropped-column values are ignored. Dropping and later re-adding a column gives it a new ID.

The storage file is an append-only sequence of records. Each record contains a magic header, CRC32 checksum, operation type, batch ID, sequence number, key/value lengths, key/value bytes, and trailer. Record framing and limits are implemented in `record.go`.

The engine loads the file into a Go map on open and keeps current data in memory while it is running. Consequently, this is not a page-based or memory-mapped database: startup reads and scans the log, and the current key/value state also consumes memory.

## 4. How a command flows through the program

```text
Start program
  -> choose database path (argument, or "godb.data")
  -> acquire process lock and read/replay the log
  -> create shell over stdin/stdout
  -> read one command (possibly continued with a trailing backslash)
       -> shell command (.verify, .compact, .help, .exit), OR
       -> lexer -> parser -> executor
            -> catalog loads/validates schema
            -> executor reads rows and/or constructs changes
            -> storage appends records and synchronizes writes
       -> print result or error; continue prompt
  -> EXIT/QUIT or end of input
  -> synchronize/close database and release lock
```

### Writes and batches

A normal `Put` appends one record and calls file `Sync` before updating the in-memory map. A normal `Delete` does the same when the key exists; deleting a missing key is a no-op.

A batch accumulates operations in memory, then appends the batch records plus a commit marker and synchronizes them. On replay, a batch is applied only when its matching commit marker and operation count are valid. The SQL executor uses batches for multi-row inserts, updates, and deletes; inserts also commit the new row counter with the inserted rows.

The engine mutex serializes access inside one process. A separate OS lock is used to reject another process opening the same lock path. These mechanisms do not make the catalog itself a concurrently safe public API; the catalog explicitly states that it is not safe for concurrent goroutine use, and the shell executes one command at a time.

### Startup recovery and verification

On open, the engine reads the database file and scans records:

- A valid standalone operation is replayed.
- A batch is applied only after a valid commit record.
- A trailing incomplete/corrupt portion is excluded from replay and truncated to the last accepted boundary.
- Detected middle-of-log corruption returns an error rather than intentionally truncating through later valid records.

`.verify` re-reads and scans the file without modifying it. It reports file size, accepted bytes, record/batch counts, live key count, and trailing bytes. It checks the storage log format and batch structure; it does not validate every catalog schema or decode every row JSON document.

### Compaction

`.compact` writes current live keys to a temporary `<database>.compact` file, synchronizes it, scans it back, and checks that the reconstructed keys/values match the current in-memory state before replacing the original file. The platform-specific replacement and directory-sync behavior differs by OS. A `.lock` sidecar is used while the engine is open; do not delete or replace it while the database is in use.

## 5. Commands actually implemented

The shell help and parser implement the following command forms:

```sql
CREATE TABLE name (column TYPE, ...)
DROP TABLE name
ALTER TABLE name ADD COLUMN column TYPE
ALTER TABLE name DROP COLUMN column
SHOW TABLES
DESCRIBE name
INSERT INTO name [(column, ...)] VALUES (...), (...)
SELECT column, ... | * FROM name [WHERE condition]
UPDATE name SET column = value, ... [WHERE condition]
DELETE FROM name [WHERE condition]
HELP
EXIT
QUIT
```

Shell-only commands:

```text
.verify
.compact
.help
.exit
.quit
```

Supported column types are `INT`, `FLOAT`, `TEXT`, and `BOOL`. `NULL` is accepted for each type. Values are literals; `UPDATE` assignments do not evaluate expressions.

WHERE conditions support `=`, `!=`, `<>`, `<`, `>`, `<=`, `>=`, `IS NULL`, `IS NOT NULL`, parentheses, `AND`, and `OR`. `AND` binds more tightly than `OR`.

Names and keywords are case-insensitive in the parser; names are normalized to lowercase. The lexer accepts ASCII letters and underscores at the start of identifiers and digits after the first character. String literals use single quotes and the lexer escapes an embedded apostrophe by doubling it, e.g. `'it''s'`. A trailing semicolon is accepted. A trailing backslash in shell input joins the next line.

The parser handles one command per input; it does not implement a general multi-statement SQL script.

### Example session

```sql
CREATE TABLE users (name TEXT, age INT, active BOOL)
INSERT INTO users VALUES ('Asha', 31, TRUE), ('Noah', 28, FALSE)
SELECT name, age FROM users WHERE active = TRUE
UPDATE users SET age = 32 WHERE name = 'Asha'
DELETE FROM users WHERE active = FALSE
SHOW TABLES
DESCRIBE users
.verify
.compact
EXIT
```

## 6. Running it

The module name is `godb`; `go.mod` declares Go `1.27.3`. The program uses `godb.data` in the current directory unless a database path is supplied as its first argument.

```powershell
go test ./...
go run . 'D:\path\to\my.data'
```

The shell reads commands from standard input. For example, on PowerShell:

```powershell
@'
CREATE TABLE users (name TEXT, age INT)
INSERT INTO users VALUES ('Asha', 31)
SELECT * FROM users
EXIT
'@ | go run . 'D:\path\to\my.data'
```

The program is not an HTTP API. The observed entry point connects `storage.Open` to the shell using `os.Stdin` and `os.Stdout`; no remote endpoint is configured in the inspected files.

## 7. What it does not implement

The inspected parser/executor has no implementation for joins, indexes, ordering, limits, aggregates, constraints, foreign keys, prepared statements, user authentication, roles, network protocol, replication, backup/restore command, or a migration framework. This list describes what was not found in the inspected project source; it is not a claim about a future roadmap.

There is no documented support for multiple concurrent application processes sharing a database. The locking code is intended to enforce one open process, not to provide multi-process transactions.

The executor scans the in-memory map by key prefix and loads all rows for the target table for `SELECT`, `UPDATE`, and `DELETE`; no secondary index is implemented. Large tables therefore should not be assumed to have production-scale query performance.

There are no README or deployment/operations instructions in the inspected workspace. The supplied `test.data` and zero-byte file named `26` exist, but their purpose and provenance are not established by the source or documentation inspected here.

## 8. Verified status and honest limitations

### Checks run for this report

- `go test ./...` — passed; non-storage packages report no test files.
- `go vet ./...` — passed.
- A Linux storage-test cross-compile had passed in the previous review. That was compilation only, not Linux runtime validation.
- `go test -race` was previously attempted but could not run because CGO was disabled and the race detector requires CGO in this environment.
- A Plan 9 cross-compile was run for this report and **failed** with `undefined: os` in `internal/storage/lock_other.go`. That fallback file calls `os.Rename` but imports only `fmt`. The project's intended fallback behavior is to report unsupported inter-process locking; as written, at least this target does not compile. This issue is documented, not fixed, in this report.

### Test coverage is not a safety certification

The storage tests cover normal reopen, a torn tail, selected middle corruption, committed/uncommitted batches, compaction/reopen, goroutine writers, randomized operations, and rejection of a competing process. They do not simulate power loss, forced process termination during a write/compaction, injected filesystem errors, or all supported OS lock/replacement behaviors. See [`STORAGE_TEST_REPORT.md`](./STORAGE_TEST_REPORT.md) for the test-by-test review and recommended follow-up.

The successful test and vet commands establish only that those commands passed on this machine for the checked code. They do not establish that the database is safe for valuable production data, that all crash scenarios are handled, or that performance is adequate for a particular workload.

## 9. Bottom line

The evidence supports describing this as a small, local, single-process SQL-like database prototype with an in-memory key/value map backed by a checksummed append-only file. It has useful durability and recovery mechanisms and a meaningful storage regression suite. It is not demonstrated to be a mature production database, and its crash behavior, filesystem-failure behavior, scalability, and cross-platform support need more testing. The confirmed fallback-platform compile failure should be fixed before claiming portable builds.
