package storage

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestBatchLastOpWins(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	e, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	b := e.NewBatch()
	b.Put("k", []byte("1"))
	b.Delete("k")
	b.Put("k", []byte("3"))
	b.Put("gone", []byte("x"))
	b.Delete("gone")
	if err := b.Commit(); err != nil {
		t.Fatal(err)
	}
	e.Close()
	e, _ = Open(path) // replay path must agree with the in-memory path
	defer e.Close()
	if v, ok := e.Get("k"); !ok || string(v) != "3" {
		t.Fatalf("k = %q,%v want 3", v, ok)
	}
	if _, ok := e.Get("gone"); ok {
		t.Fatal("deleted-in-batch key resurrected")
	}
}

func TestMaxBytesCompactsThenRetries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	e, err := OpenWithOptions(path, Options{MaxBytes: 4096})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	// Overwrite one key far more times than 4 KiB could hold uncompacted.
	for i := 0; i < 500; i++ {
		if err := e.Put("k", []byte(fmt.Sprintf("value-%d", i))); err != nil {
			t.Fatalf("put %d: %v", i, err)
		}
	}
	if fi, _ := os.Stat(path); fi.Size() > 4096 {
		t.Fatalf("file size %d exceeds cap", fi.Size())
	}
	if v, _ := e.Get("k"); string(v) != "value-499" {
		t.Fatalf("got %q", v)
	}
}

func TestDiskFullLeavesDatabaseUsable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	e, _ := OpenWithOptions(path, Options{MaxBytes: 1024})
	defer e.Close()
	if err := e.Put("a", make([]byte, 500)); err != nil {
		t.Fatal(err)
	}
	err := e.Put("b", make([]byte, 900))
	if !errors.Is(err, ErrDiskFull) {
		t.Fatalf("want ErrDiskFull, got %v", err)
	}
	if _, ok := e.Get("b"); ok {
		t.Fatal("failed put is visible")
	}
	// Deleting frees room; a small write then succeeds.
	if err := e.Delete("a"); err != nil {
		t.Fatalf("delete while full: %v", err)
	}
	if err := e.Put("c", []byte("ok")); err != nil {
		t.Fatalf("put after freeing space: %v", err)
	}
	e.Close()
	e2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer e2.Close()
	if v, _ := e2.Get("c"); string(v) != "ok" {
		t.Fatal("c lost")
	}
	if _, ok := e2.Get("a"); ok {
		t.Fatal("a resurrected")
	}
}

func TestOpenRemovesStaleCompactFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	os.WriteFile(path+".compact", []byte("half written"), 0o644)
	e, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	if _, err := os.Stat(path + ".compact"); err == nil {
		t.Fatal("stale .compact survived Open")
	}
}

// Run with: GODB_BENCH=1 go test ./internal/storage -run Recovery1M -v
func TestRecovery1M(t *testing.T) {
	if os.Getenv("GODB_BENCH") == "" {
		t.Skip("set GODB_BENCH=1")
	}
	path := filepath.Join(t.TempDir(), "db")
	e, _ := Open(path)
	for i := 0; i < 100; i++ {
		b := e.NewBatch()
		for j := 0; j < 10000; j++ {
			b.Put(fmt.Sprintf("key-%08d", i*10000+j), []byte("value-0123456789"))
		}
		if err := b.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	e.Close()
	fi, _ := os.Stat(path)
	start := time.Now()
	e, err := Open(path)
	d := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	t.Logf("recovered %d keys from %d MiB in %v", len(e.data), fi.Size()>>20, d)
	if d > 2*time.Second {
		t.Errorf("recovery took %v, want < 2s", d)
	}
}
