package storage

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"sort"
	"sync"
	"syscall"
)

var (
	// ErrCorruptLog means a bad record was found in the MIDDLE of the log:
	// valid records follow it, so acknowledged data would be lost if we
	// truncated. The file is left untouched.
	ErrCorruptLog = errors.New("storage: corruption in the middle of the log")

	ErrClosed           = errors.New("storage: engine is closed")
	ErrDatabaseLocked   = errors.New("storage: database is already open by another process")
	ErrRecoveryRequired = errors.New("storage: write rollback failed; close and recover the database")
	ErrKeyEmpty         = errors.New("storage: key is empty")
	ErrKeyTooLarge      = errors.New("storage: key too large")
	ErrValueTooBig      = errors.New("storage: value too large")
	ErrBatchTooBig      = errors.New("storage: too many operations in batch")
	ErrBatchDone        = errors.New("storage: batch already committed")

	// ErrDiskFull means the write would exceed MaxBytes (or the OS reported
	// ENOSPC). The write was NOT applied and the database is still usable:
	// reads work and smaller writes or deletes may still succeed.
	ErrDiskFull = errors.New("storage: disk full")
)

// Options configures Open.
type Options struct {
	// MaxBytes caps the size of the database file. 0 means unlimited.
	MaxBytes int64
}

// KV is one key/value pair returned by Scan.
type KV struct {
	Key   string
	Value []byte
}

// Engine is a single-file, crash-safe key-value store.
// All data lives in memory; the file is an append-only write-ahead log.
type Engine struct {
	mu        sync.Mutex
	path      string
	f         *os.File
	lock      *processLock
	data      map[string][]byte
	offset    int64  // end of valid data in the file
	seq       uint64 // last sequence number used
	nextBatch uint64 // next batch ID (assigned under mu)
	closed    bool
	failure   error
	maxBytes  int64 // 0 = unlimited
}

// ---------------------------------------------------------------------------
// Log scanning (shared by Open and Verify; never modifies the file)
// ---------------------------------------------------------------------------

type scanResult struct {
	data      map[string][]byte
	goodEnd   int64 // offset just after the last fully committed record
	records   int
	batches   int
	maxSeq    uint64
	maxBatch  uint64
	tailBytes int64 // bytes after goodEnd (torn write / uncommitted batch)
}

type pendingBatch struct {
	id  uint64
	ops []Record
}

// scanLog replays buf. A bad record followed by NO valid record is a torn
// tail and is reported via tailBytes. A bad record followed by valid records
// is mid-file corruption and returns ErrCorruptLog.
func scanLog(buf []byte) (*scanResult, error) {
	res := &scanResult{data: make(map[string][]byte)}
	var pend *pendingBatch
	off := 0

	apply := func(r Record) {
		if r.Type == OpPut {
			res.data[string(r.Key)] = r.Value
		} else if r.Type == OpDel {
			delete(res.data, string(r.Key))
		}
	}

	for off < len(buf) {
		rec, n, err := ParseRecord(buf[off:])
		if err != nil {
			if hasValidRecordAfter(buf, off) {
				return nil, fmt.Errorf("%w: bad record at offset %d but valid records follow", ErrCorruptLog, off)
			}
			break // bad tail: caller may cut it off
		}

		if rec.Seq > res.maxSeq {
			res.maxSeq = rec.Seq
		}
		if rec.BatchID > res.maxBatch {
			res.maxBatch = rec.BatchID
		}

		switch rec.Type {
		case OpPut, OpDel:
			if rec.BatchID == 0 {
				if pend != nil {
					return nil, fmt.Errorf("%w: standalone record inside an open batch at offset %d", ErrCorruptLog, off)
				}
				apply(rec)
				res.records++
				res.goodEnd = int64(off + n)
			} else {
				if pend == nil {
					pend = &pendingBatch{id: rec.BatchID}
				} else if pend.id != rec.BatchID {
					return nil, fmt.Errorf("%w: interleaved batches at offset %d", ErrCorruptLog, off)
				}
				pend.ops = append(pend.ops, rec)
			}

		case OpBatchCommit:
			if pend == nil || pend.id != rec.BatchID || len(rec.Value) != 8 ||
				binary.LittleEndian.Uint64(rec.Value) != uint64(len(pend.ops)) {
				return nil, fmt.Errorf("%w: invalid batch commit at offset %d", ErrCorruptLog, off)
			}
			for _, op := range pend.ops {
				apply(op)
			}
			res.records += len(pend.ops) + 1
			res.batches++
			res.goodEnd = int64(off + n)
			pend = nil
		}
		off += n
	}

	res.tailBytes = int64(len(buf)) - res.goodEnd
	return res, nil
}

// hasValidRecordAfter reports whether any complete, valid record starts
// after position off.
func hasValidRecordAfter(buf []byte, off int) bool {
	i := off + 1
	for i < len(buf) {
		j := bytes.Index(buf[i:], magicHeader)
		if j < 0 {
			return false
		}
		i += j
		if _, _, err := ParseRecord(buf[i:]); err == nil {
			return true
		}
		i++
	}
	return false
}

// ---------------------------------------------------------------------------
// Open / Close
// ---------------------------------------------------------------------------

// Open opens (or creates) the database file and recovers its state.
// A torn tail is cut off; corruption in the middle returns ErrCorruptLog
// and leaves the file untouched. An OS lock prevents multiple processes from
// opening the same database with independent in-memory snapshots.
func Open(path string) (*Engine, error) { return OpenWithOptions(path, Options{}) }

// OpenWithOptions is Open with a size cap and other options.
func OpenWithOptions(path string, opts Options) (*Engine, error) {
	lock, err := acquireProcessLock(path)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*Engine, error) {
		if lockErr := lock.close(); lockErr != nil {
			return nil, errors.Join(err, fmt.Errorf("release database lock: %w", lockErr))
		}
		return nil, err
	}

	// A crash during compaction can leave a half-written temp file. It is
	// never part of the database (only the atomic rename publishes it), and
	// we hold the lock, so it is safe to delete.
	if err := os.Remove(path + ".compact"); err != nil && !os.IsNotExist(err) {
		return fail(err)
	}

	var raw []byte
	if b, err := os.ReadFile(path); err == nil {
		raw = b
	} else if !os.IsNotExist(err) {
		return fail(err)
	}

	res, err := scanLog(raw)
	if err != nil {
		return fail(err)
	}

	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return fail(err)
	}
	if res.tailBytes > 0 {
		if err := f.Truncate(res.goodEnd); err != nil {
			return fail(errors.Join(err, f.Close()))
		}
		if err := f.Sync(); err != nil {
			return fail(errors.Join(err, f.Close()))
		}
	}

	return &Engine{
		path:      path,
		f:         f,
		lock:      lock,
		data:      res.data,
		offset:    res.goodEnd,
		seq:       res.maxSeq,
		nextBatch: res.maxBatch + 1,
		maxBytes:  opts.MaxBytes,
	}, nil
}

// Close flushes and closes the file.
func (e *Engine) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return nil
	}
	e.closed = true
	var syncErr, fileErr, lockErr error
	if e.f != nil {
		syncErr = e.f.Sync()
		fileErr = e.f.Close()
	}
	if e.lock != nil {
		lockErr = e.lock.close()
		e.lock = nil
	}
	return errors.Join(syncErr, fileErr, lockErr)
}

// ---------------------------------------------------------------------------
// Writing
// ---------------------------------------------------------------------------

// isNoSpace reports whether err is the OS saying the device is full.
func isNoSpace(err error) bool {
	return errors.Is(err, syscall.ENOSPC) || errors.Is(err, errDiskQuota)
}

// appendLocked assigns sequence numbers, writes all records in ONE write,
// and fsyncs. On failure the file is rolled back to its previous end.
//
// Disk-full policy (checked BEFORE any byte is written):
//  1. If offset+size would exceed maxBytes, compact once to reclaim dead
//     space and re-check.
//  2. If it still does not fit, return ErrDiskFull. Nothing was written,
//     so there is no partial record and memory is unchanged.
//
// A real ENOSPC from the OS is handled the same way after rollback.
// Caller must hold e.mu.
func (e *Engine) appendLocked(recs []Record) error {
	size := int64(0)
	for i := range recs {
		size += int64(recs[i].Size())
	}
	if e.maxBytes > 0 && e.offset+size > e.maxBytes {
		if err := e.compactLocked(); err != nil && !errors.Is(err, ErrDiskFull) {
			return err
		}
		if e.failure != nil {
			return fmt.Errorf("%w: %v", ErrRecoveryRequired, e.failure)
		}
		if e.offset+size > e.maxBytes {
			return ErrDiskFull
		}
	}

	startSeq := e.seq
	var buf []byte
	for i := range recs {
		e.seq++
		recs[i].Seq = e.seq
		buf = append(buf, recs[i].Serialize()...)
	}

	_, err := e.f.WriteAt(buf, e.offset)
	if err == nil {
		err = e.f.Sync()
	}
	if err != nil {
		e.seq = startSeq
		// Roll back to the last good offset. If we cannot, the file may
		// hold a partial record; refuse all further writes (reads still
		// work from memory) and let the next Open cut the tail.
		if rollbackErr := e.f.Truncate(e.offset); rollbackErr != nil {
			e.failure = errors.Join(err, fmt.Errorf("truncate failed: %w", rollbackErr))
			return fmt.Errorf("%w: %v", ErrRecoveryRequired, e.failure)
		}
		if rollbackErr := e.f.Sync(); rollbackErr != nil {
			e.failure = errors.Join(err, fmt.Errorf("sync after truncate failed: %w", rollbackErr))
			return fmt.Errorf("%w: %v", ErrRecoveryRequired, e.failure)
		}
		if isNoSpace(err) {
			return fmt.Errorf("%w: %v", ErrDiskFull, err)
		}
		return err
	}
	e.offset += int64(len(buf))
	return nil
}

func validateKey(key string) error {
	if len(key) == 0 {
		return ErrKeyEmpty
	}
	if len(key) > MaxKeySize {
		return ErrKeyTooLarge
	}
	return nil
}

// Put stores value under key durably.
func (e *Engine) Put(key string, value []byte) error {
	if err := validateKey(key); err != nil {
		return err
	}
	if len(value) > MaxValueSize {
		return ErrValueTooBig
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return ErrClosed
	}
	if e.failure != nil {
		return fmt.Errorf("%w: %v", ErrRecoveryRequired, e.failure)
	}
	v := append([]byte(nil), value...)
	if err := e.appendLocked([]Record{{Type: OpPut, Key: []byte(key), Value: v}}); err != nil {
		return err
	}
	e.data[key] = v
	return nil
}

// Delete removes key durably. Deleting a missing key is a no-op.
func (e *Engine) Delete(key string) error {
	if err := validateKey(key); err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return ErrClosed
	}
	if e.failure != nil {
		return fmt.Errorf("%w: %v", ErrRecoveryRequired, e.failure)
	}
	if _, ok := e.data[key]; !ok {
		return nil
	}
	if err := e.appendLocked([]Record{{Type: OpDel, Key: []byte(key)}}); err != nil {
		return err
	}
	delete(e.data, key)
	return nil
}

// ---------------------------------------------------------------------------
// Reading
// ---------------------------------------------------------------------------

// Get returns a copy of the value for key.
func (e *Engine) Get(key string) ([]byte, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	v, ok := e.data[key]
	if !ok {
		return nil, false
	}
	return append([]byte(nil), v...), true
}

// Scan returns all pairs whose key starts with prefix, sorted by key.
func (e *Engine) Scan(prefix string) []KV {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []KV
	for k, v := range e.data {
		if len(k) >= len(prefix) && k[:len(prefix)] == prefix {
			out = append(out, KV{Key: k, Value: append([]byte(nil), v...)})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// ---------------------------------------------------------------------------
// Atomic batches
// ---------------------------------------------------------------------------

// Batch collects operations that are committed all-or-nothing.
type Batch struct {
	e    *Engine
	ops  []Record
	done bool
	err  error // first error from Put/Delete, reported by Commit
}

// NewBatch starts an empty batch. Nothing is written until Commit.
func (e *Engine) NewBatch() *Batch { return &Batch{e: e} }

func (b *Batch) Put(key string, value []byte) {
	if b.err != nil {
		return
	}
	if err := validateKey(key); err != nil {
		b.err = err
		return
	}
	if len(value) > MaxValueSize {
		b.err = ErrValueTooBig
		return
	}
	b.ops = append(b.ops, Record{Type: OpPut, Key: []byte(key), Value: append([]byte(nil), value...)})
}

func (b *Batch) Delete(key string) {
	if b.err != nil {
		return
	}
	if err := validateKey(key); err != nil {
		b.err = err
		return
	}
	b.ops = append(b.ops, Record{Type: OpDel, Key: []byte(key)})
}

// Commit writes the batch plus a commit record in a single write + fsync.
// Operations apply in the order they were added, both in memory and on
// replay, so if a key appears more than once the LAST operation wins.
// The batch ID is assigned while holding the engine lock.
func (b *Batch) Commit() error {
	if b.done {
		return ErrBatchDone
	}
	if b.err != nil {
		return b.err
	}
	if len(b.ops) > MaxBatchOps {
		return ErrBatchTooBig
	}
	if len(b.ops) == 0 {
		b.done = true
		return nil
	}

	e := b.e
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return ErrClosed
	}
	if e.failure != nil {
		return fmt.Errorf("%w: %v", ErrRecoveryRequired, e.failure)
	}

	id := e.nextBatch // assigned inside the lock: no race
	e.nextBatch++

	recs := make([]Record, 0, len(b.ops)+1)
	for _, op := range b.ops {
		op.BatchID = id
		recs = append(recs, op)
	}
	count := make([]byte, 8)
	binary.LittleEndian.PutUint64(count, uint64(len(b.ops)))
	recs = append(recs, Record{Type: OpBatchCommit, BatchID: id, Value: count})

	if err := e.appendLocked(recs); err != nil {
		return err
	}
	for _, op := range b.ops {
		if op.Type == OpPut {
			e.data[string(op.Key)] = op.Value
		} else {
			delete(e.data, string(op.Key))
		}
	}
	b.done = true
	return nil
}

// ---------------------------------------------------------------------------
// Compaction
// ---------------------------------------------------------------------------

// Compact rewrites the log with only live keys, then atomically replaces
// the old file.
func (e *Engine) Compact() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return ErrClosed
	}
	if e.failure != nil {
		return fmt.Errorf("%w: %v", ErrRecoveryRequired, e.failure)
	}
	return e.compactLocked()
}

func (e *Engine) compactLocked() error {
	keys := make([]string, 0, len(e.data))
	for k := range e.data {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var buf []byte
	var seq uint64
	for _, k := range keys {
		seq++
		r := Record{Type: OpPut, Seq: seq, Key: []byte(k), Value: e.data[k]}
		buf = append(buf, r.Serialize()...)
	}

	// The compacted file replaces the log, so it must itself fit the cap.
	if e.maxBytes > 0 && int64(len(buf)) > e.maxBytes {
		return ErrDiskFull
	}

	// diskFull converts an OS out-of-space error into ErrDiskFull. The temp
	// file is always removed; the original log is untouched at this point.
	tmp := e.path + ".compact"
	diskFull := func(err error) error {
		if isNoSpace(err) {
			err = fmt.Errorf("%w: %v", ErrDiskFull, err)
		}
		return err
	}
	tf, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return diskFull(err)
	}
	if _, err := tf.Write(buf); err != nil {
		return errors.Join(diskFull(err), tf.Close(), os.Remove(tmp))
	}
	if err := tf.Sync(); err != nil {
		return errors.Join(diskFull(err), tf.Close(), os.Remove(tmp))
	}
	if err := tf.Close(); err != nil {
		return errors.Join(err, os.Remove(tmp))
	}

	written, err := os.ReadFile(tmp)
	if err != nil {
		return errors.Join(err, os.Remove(tmp))
	}
	check, err := scanLog(written)
	if err == nil && (check.tailBytes != 0 || len(check.data) != len(e.data)) {
		err = errors.New("compacted file has an incomplete tail or unexpected key count")
	}
	if err == nil {
		for key, value := range e.data {
			if got, ok := check.data[key]; !ok || !bytes.Equal(got, value) {
				err = fmt.Errorf("compacted file does not preserve key %q", key)
				break
			}
		}
	}
	if err != nil {
		return errors.Join(fmt.Errorf("validate compacted file: %w", err), os.Remove(tmp))
	}

	// Windows cannot replace a file that is open, so close first.
	if err := e.f.Close(); err != nil {
		f, reopenErr := os.OpenFile(e.path, os.O_RDWR, 0o644)
		if reopenErr != nil {
			e.f = nil
			e.failure = errors.Join(err, reopenErr)
		} else {
			e.f = f
		}
		return errors.Join(err, reopenErr, os.Remove(tmp))
	}
	e.f = nil
	renameErr := replaceDatabaseFile(tmp, e.path)
	if renameErr != nil {
		renameErr = errors.Join(renameErr, os.Remove(tmp))
	}

	f, err := os.OpenFile(e.path, os.O_RDWR, 0o644)
	if err != nil {
		e.failure = errors.Join(err, renameErr)
		return errors.Join(err, renameErr)
	}
	e.f = f
	if renameErr != nil {
		return renameErr // old file is intact and reopened
	}
	e.offset = int64(len(buf))
	e.seq = seq
	return syncDatabaseDirectory(e.path)
}

// ---------------------------------------------------------------------------
// Integrity check (read-only: never repairs or truncates)
// ---------------------------------------------------------------------------

// VerifyReport describes the state of the file on disk.
type VerifyReport struct {
	FileSize      int64
	GoodBytes     int64
	TrailingBytes int64 // torn tail or uncommitted batch (not repaired here)
	Records       int
	Batches       int
	LiveKeys      int
}

// Verify re-reads the file and checks every record. It never modifies the
// file. Mid-file corruption is returned as an error wrapping ErrCorruptLog.
func (e *Engine) Verify() (VerifyReport, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return VerifyReport{}, ErrClosed
	}
	raw, err := os.ReadFile(e.path)
	if err != nil {
		return VerifyReport{}, err
	}
	res, err := scanLog(raw)
	if err != nil {
		return VerifyReport{FileSize: int64(len(raw))}, err
	}
	return VerifyReport{
		FileSize:      int64(len(raw)),
		GoodBytes:     res.goodEnd,
		TrailingBytes: res.tailBytes,
		Records:       res.records,
		Batches:       res.batches,
		LiveKeys:      len(res.data),
	}, nil
}

// VerifyFile checks a database file WITHOUT opening it: it takes no lock,
// never truncates, never writes. Safe to run while another process has the
// database open (it then sees a snapshot, so a growing tail is normal).
func VerifyFile(path string) (VerifyReport, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return VerifyReport{}, err
	}
	res, err := scanLog(raw)
	if err != nil {
		return VerifyReport{FileSize: int64(len(raw))}, err
	}
	return VerifyReport{
		FileSize:      int64(len(raw)),
		GoodBytes:     res.goodEnd,
		TrailingBytes: res.tailBytes,
		Records:       res.records,
		Batches:       res.batches,
		LiveKeys:      len(res.data),
	}, nil
}
