# Storage Engine Test Report

## Scope and bottom line

This report documents the tests in [`internal/storage/engine_test.go`](./internal/storage/engine_test.go). It is a report on test coverage, not a certification of the storage engine or a claim that every crash, filesystem, or operating-system failure is safe.

The file contains seven substantive test scenarios and one subprocess helper test. Together they cover basic durable state, batch replay, tail recovery, refusal to open a database with detected middle corruption, in-process concurrent writes, compaction/reopen, and rejection of a second process while a database is open.

**Bottom line:** this is a useful focused regression suite, but it does not yet prove crash safety under real process termination or power loss, behavior under injected I/O failures, or all platform-specific locking and replacement behavior.

## Test-by-test coverage

| Test | What it does | What it establishes | What it does not establish |
|---|---|---|---|
| `TestReopenAndCompactPreserveCommittedState` | Writes and overwrites a key, deletes another key, commits a two-key batch, closes/reopens, compacts, then closes/reopens again. | The tested key/value state survives ordinary close/reopen and compaction. Deleted keys remain absent, and the tested batch's values are present. | It does not assert that the compacted file is smaller, inspect its exact record layout, or exercise compaction failure/replacement error paths. |
| `TestOpenTruncatesIncompleteBatchAndTornTail` | Creates a valid database, appends a deliberately shortened put record with a nonzero batch ID, then reopens it. | Reopening retains the previously committed key, does not expose the incomplete operation, and truncates the file back to the recorded valid length. | It tests one truncation point in one operation record. It does not enumerate every possible partial header/body/trailer or I/O failure point. |
| `TestOpenRejectsMiddleCorruptionWithoutChangingFile` | Writes two standalone records, corrupts a byte in the first record's checksum, and tries to reopen. | The tested corruption is reported as `ErrCorruptLog`, and the file bytes remain unchanged by the failed open. | It does not test all corruption types or offsets, nor prove that every malformed log is classified correctly. |
| `TestOpenIgnoresUncommittedBatch` | Writes a valid batch operation followed by a truncated batch-commit record, then opens the database. | The uncommitted operation is not visible, and the incomplete log tail is truncated to an empty file. | It does not separately test a complete batch operation with no commit record, malformed complete commit markers, or interleaved batches. |
| `TestConcurrentWritersPersistAllOperations` | Uses eight goroutines to issue 50 unique-key puts each, checks the resulting key count, closes/reopens, and checks the count again. | The tested in-process concurrent writes complete without reported errors, and all 400 unique keys are present before and after reopening. | It checks the count rather than every stored value. It is not a multiprocess write test and does not by itself prove race freedom; use the race detector where supported. |
| `TestRandomizedOperationsSurviveRecoveryAndCompaction` | Applies 500 deterministic pseudo-random puts/deletes over 48 keys, closes/reopens and compacts twice, and compares the database with an in-memory expected map. | The tested repeatable operation sequence matches the expected state after the tested normal close/reopen and compaction cycles. | Despite “recovery” in its name, it closes normally before reopening; it does not simulate a crash. Its fixed seed gives repeatability, not broad random coverage. |
| `TestOpenRejectsConcurrentProcess` | Opens a temporary database in the parent test process and starts a second copy of the test executable to try opening that same database. | The second process is rejected with `ErrDatabaseLocked` while the parent holds the database open. | It does not test successful acquisition after the first process exits, locking during abrupt process termination, multiple contenders, or competing writes. |
| `TestOpenLockHelper` | Acts as the subprocess entry point when `GODB_STORAGE_LOCK_HELPER` is set; otherwise returns without opening a database. | In the subprocess path, it verifies that the competing open fails with `ErrDatabaseLocked`. | Running it alone without the environment variable is a no-op, not an independent lock test. |

The remaining functions (`assertValue`, `assertMissing`, and `assertState`) are test helpers, not separate test cases. They verify selected values, key absence, or equality of the complete scanned state against the expected map.

## Gaps and recommended next work

These are coverage gaps, not claims that the implementation is known to fail:

1. **Crash-path coverage:** add subprocess tests that terminate a writer without a normal `Close`, then reopen and verify all acknowledged data and the documented handling of unacknowledged writes. A process kill is still not a full power-loss or storage-device simulation.
2. **I/O failure handling:** introduce a narrow injectable file-operation boundary or another controlled fault mechanism to test short writes, sync failures, truncation failures, and temporary-file/rename failures. Verify both the returned error and whether the engine refuses subsequent writes when recovery is required.
3. **Compaction failure safety:** prove that invalid or failed temporary compaction never replaces the original database, and that failures while closing, replacing, reopening, or synchronizing leave the engine in a clearly reported usable or unusable state.
4. **Lock lifecycle and platform coverage:** test that a second process can open after the first closes, and after the first exits abnormally. Run lock contention tests on each supported operating system; compiling the platform-specific implementation is not equivalent to executing its behavior.
5. **Log parser boundaries:** add table-driven corruption cases for bad magic, invalid record type, impossible lengths, invalid trailer, checksum mismatch, malformed commit marker, and corruption at different positions in the log. Check both recovery classification and whether disk bytes are modified.
6. **Concurrent-write assertions:** validate every key/value after reopening, and run `go test -race ./...` in an environment with CGO and a supported C compiler.
7. **Resource/error assertions:** check errors from deferred `Close` calls in tests where close behavior matters, and assert compaction changes file size when reduction is an intended guarantee.

Prioritize crash-path and compaction-failure tests first: they address the most consequential behavior that the existing suite does not directly simulate.

## Verification record

The storage tests were observed passing with `go test ./internal/storage`. The broader Go test suite and `go vet ./...` were also observed passing during the review. A Linux cross-compile of the storage test binary succeeded; that verifies compilation, not Linux runtime behavior.

The race detector was not run successfully in the reviewed environment: `go test -race` reported that CGO is required, and CGO was disabled. This report therefore makes no race-detector result claim.
