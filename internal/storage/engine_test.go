package storage

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
)

func TestReopenAndCompactPreserveCommittedState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db.data")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Put("keep", []byte("first")); err != nil {
		t.Fatal(err)
	}
	if err := db.Put("keep", []byte("latest")); err != nil {
		t.Fatal(err)
	}
	if err := db.Put("remove", []byte("old")); err != nil {
		t.Fatal(err)
	}
	if err := db.Delete("remove"); err != nil {
		t.Fatal(err)
	}

	batch := db.NewBatch()
	batch.Put("batch/a", []byte("a"))
	batch.Put("batch/b", []byte("b"))
	if err := batch.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	assertValue(t, db, "keep", "latest")
	assertMissing(t, db, "remove")
	assertValue(t, db, "batch/a", "a")
	assertValue(t, db, "batch/b", "b")
	if err := db.Compact(); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	assertValue(t, db, "keep", "latest")
	assertMissing(t, db, "remove")
	assertValue(t, db, "batch/a", "a")
	assertValue(t, db, "batch/b", "b")
}

func TestOpenTruncatesIncompleteBatchAndTornTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db.data")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Put("kept", []byte("value")); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	incomplete := Record{Type: OpPut, BatchID: 1, Key: []byte("lost"), Value: []byte("uncommitted")}.Serialize()
	if err := os.WriteFile(path, append(raw, incomplete[:len(incomplete)-2]...), 0o600); err != nil {
		t.Fatal(err)
	}

	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	assertValue(t, db, "kept", "value")
	assertMissing(t, db, "lost")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != int64(len(raw)) {
		t.Fatalf("recovered file size = %d, want %d", info.Size(), len(raw))
	}
}

func TestOpenRejectsMiddleCorruptionWithoutChangingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db.data")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Put("first", []byte("1")); err != nil {
		t.Fatal(err)
	}
	if err := db.Put("second", []byte("2")); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	raw[4] ^= 0xff
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); !errors.Is(err, ErrCorruptLog) {
		t.Fatalf("Open error = %v, want ErrCorruptLog", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("Open modified a database containing middle corruption")
	}
}

func TestOpenIgnoresUncommittedBatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db.data")
	op := Record{Type: OpPut, BatchID: 7, Seq: 1, Key: []byte("uncommitted"), Value: []byte("value")}.Serialize()
	commit := make([]byte, 8)
	binary.LittleEndian.PutUint64(commit, 1)
	commitRecord := Record{Type: OpBatchCommit, BatchID: 7, Seq: 2, Value: commit}.Serialize()
	if err := os.WriteFile(path, append(op, commitRecord[:len(commitRecord)-1]...), 0o600); err != nil {
		t.Fatal(err)
	}

	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	assertMissing(t, db, "uncommitted")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 0 {
		t.Fatalf("file size = %d, want 0 after discarding uncommitted batch", info.Size())
	}
}

func TestConcurrentWritersPersistAllOperations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db.data")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}

	const writers = 8
	const operationsPerWriter = 50
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for writer := 0; writer < writers; writer++ {
		wg.Add(1)
		go func(writer int) {
			defer wg.Done()
			for operation := 0; operation < operationsPerWriter; operation++ {
				key := fmt.Sprintf("writer/%d/%d", writer, operation)
				if err := db.Put(key, []byte(key)); err != nil {
					errs <- err
					return
				}
			}
		}(writer)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}

	if got, want := len(db.Scan("writer/")), writers*operationsPerWriter; got != want {
		t.Fatalf("Scan returned %d keys, want %d", got, want)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if got, want := len(db.Scan("writer/")), writers*operationsPerWriter; got != want {
		t.Fatalf("reopened Scan returned %d keys, want %d", got, want)
	}
}

func TestRandomizedOperationsSurviveRecoveryAndCompaction(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db.data")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if db != nil {
			_ = db.Close()
		}
	}()

	const operationCount = 500
	const keyCount = 48
	rng := rand.New(rand.NewSource(20261106))
	expected := make(map[string]string)
	for i := 0; i < operationCount; i++ {
		key := fmt.Sprintf("key/%02d", rng.Intn(keyCount))
		if rng.Intn(4) == 0 {
			if err := db.Delete(key); err != nil {
				t.Fatal(err)
			}
			delete(expected, key)
		} else {
			value := fmt.Sprintf("value/%d/%d", i, rng.Intn(1_000_000))
			if err := db.Put(key, []byte(value)); err != nil {
				t.Fatal(err)
			}
			expected[key] = value
		}

		if i == 149 || i == 349 {
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			db = nil
			db, err = Open(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := db.Compact(); err != nil {
				t.Fatal(err)
			}
			assertState(t, db, expected)
		}
	}

	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db = nil
	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	assertState(t, db, expected)
}

func TestOpenRejectsConcurrentProcess(t *testing.T) {
	if path := os.Getenv("GODB_STORAGE_LOCK_HELPER"); path != "" {
		return
	}

	path := filepath.Join(t.TempDir(), "db.data")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	cmd := exec.Command(os.Args[0], "-test.run=^TestOpenLockHelper$")
	cmd.Env = append(os.Environ(), "GODB_STORAGE_LOCK_HELPER="+path)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("lock helper failed: %v\n%s", err, output)
	}
}

func TestOpenLockHelper(t *testing.T) {
	path := os.Getenv("GODB_STORAGE_LOCK_HELPER")
	if path == "" {
		return
	}
	db, err := Open(path)
	if db != nil {
		_ = db.Close()
		t.Fatal("Open succeeded while another process held the database lock")
	}
	if !errors.Is(err, ErrDatabaseLocked) {
		t.Fatalf("Open error = %v, want ErrDatabaseLocked", err)
	}
}

func assertValue(t *testing.T, db *Engine, key, want string) {
	t.Helper()
	got, ok := db.Get(key)
	if !ok {
		t.Fatalf("Get(%q) did not find key", key)
	}
	if string(got) != want {
		t.Fatalf("Get(%q) = %q, want %q", key, got, want)
	}
}

func assertMissing(t *testing.T, db *Engine, key string) {
	t.Helper()
	if _, ok := db.Get(key); ok {
		t.Fatalf("Get(%q) found unexpected key", key)
	}
}

func assertState(t *testing.T, db *Engine, expected map[string]string) {
	t.Helper()
	got := db.Scan("")
	if len(got) != len(expected) {
		t.Fatalf("Scan returned %d keys, want %d", len(got), len(expected))
	}
	for _, kv := range got {
		want, ok := expected[kv.Key]
		if !ok {
			t.Errorf("Scan returned unexpected key %q", kv.Key)
			continue
		}
		if string(kv.Value) != want {
			t.Errorf("Get(%q) = %q, want %q", kv.Key, kv.Value, want)
		}
	}
}
