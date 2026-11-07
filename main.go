// godb: a crash-safe key/value store (append-only checksummed log).
//
// Keys and values are exchanged as hex on the command line and in serve
// mode, so any byte string (spaces, newlines, NULs) is safe.
//
//	godb open    [--max-bytes N] <db>                 recover and report
//	godb put     [--max-bytes N] <db> <hexkey> <hexval>
//	godb get     <db> <hexkey>                        prints hex value
//	godb del     [--max-bytes N] <db> <hexkey>
//	godb batch   [--max-bytes N] <db>                 ops on stdin, one atomic commit
//	godb dump    <db>                                 "hexkey hexvalue" per line, sorted
//	godb check   <db>                                 read-only integrity check
//	godb compact [--max-bytes N] <db>
//	godb serve   [--max-bytes N] <db>                 line protocol, ACK after fsync
//
// Exit codes: 0 ok, 1 error, 2 usage, 3 not found, 4 disk full,
// 5 corruption detected.
//
// The old SQL shell lives in extras/sqlshell and is not part of this CLI.
package main

import (
	"bufio"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"godb/internal/storage"
)

const (
	exitOK       = 0
	exitErr      = 1
	exitUsage    = 2
	exitNotFound = 3
	exitDiskFull = 4
	exitCorrupt  = 5
)

var concurrent *bool

func main() { os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr)) }

func usage(w io.Writer) int {
	fmt.Fprintln(w, "usage: godb <open|put|get|del|batch|dump|check|compact|serve> [--max-bytes N] <db> [args]")
	return exitUsage
}

func run(args []string, in io.Reader, out, errw io.Writer) int {
	if len(args) < 1 {
		return usage(errw)
	}
	cmd := args[0]
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.SetOutput(errw)
	maxBytes := fs.Int64("max-bytes", 0, "cap on database file size in bytes (0 = unlimited)")
	concurrent = fs.Bool("concurrent", false, "serve only: handle requests in parallel (replies may reorder)")
	if err := fs.Parse(args[1:]); err != nil {
		return exitUsage
	}
	rest := fs.Args()
	if len(rest) < 1 {
		return usage(errw)
	}
	path, rest := rest[0], rest[1:]

	need := map[string]int{"open": 0, "put": 2, "get": 1, "del": 1, "batch": 0, "dump": 0, "check": 0, "compact": 0, "serve": 0}
	n, ok := need[cmd]
	if !ok || len(rest) != n {
		return usage(errw)
	}

	if cmd == "check" {
		return cmdCheck(path, out, errw)
	}

	db, err := storage.OpenWithOptions(path, storage.Options{MaxBytes: *maxBytes})
	if err != nil {
		fmt.Fprintf(errw, "ERR open: %v\n", err)
		return codeFor(err)
	}
	code := exec(cmd, db, rest, in, out, errw)
	if err := db.Close(); err != nil {
		fmt.Fprintf(errw, "ERR close: %v\n", err)
		if code == exitOK {
			code = exitErr
		}
	}
	return code
}

func codeFor(err error) int {
	switch {
	case errors.Is(err, storage.ErrDiskFull):
		return exitDiskFull
	case errors.Is(err, storage.ErrCorruptLog):
		return exitCorrupt
	default:
		return exitErr
	}
}

func exec(cmd string, db *storage.Engine, rest []string, in io.Reader, out, errw io.Writer) int {
	fail := func(err error) int {
		fmt.Fprintf(errw, "ERR %v\n", err)
		return codeFor(err)
	}
	switch cmd {
	case "open":
		fmt.Fprintf(out, "OK keys=%d\n", len(db.Scan("")))
	case "put":
		k, v, err := decode2(rest[0], rest[1])
		if err != nil {
			return fail(err)
		}
		if err := db.Put(k, v); err != nil {
			return fail(err)
		}
		fmt.Fprintln(out, "OK")
	case "get":
		k, err := unhex(rest[0])
		if err != nil {
			return fail(err)
		}
		v, found := db.Get(string(k))
		if !found {
			fmt.Fprintln(out, "NOT_FOUND")
			return exitNotFound
		}
		fmt.Fprintln(out, hex.EncodeToString(v))
	case "del":
		k, err := unhex(rest[0])
		if err != nil {
			return fail(err)
		}
		if err := db.Delete(string(k)); err != nil {
			return fail(err)
		}
		fmt.Fprintln(out, "OK")
	case "batch":
		b := db.NewBatch()
		sc := bufio.NewReaderSize(in, 1<<20)
		for {
			line, err := sc.ReadString('\n')
			if line = strings.TrimSpace(line); line != "" {
				if perr := addBatchOp(b, strings.Fields(line)); perr != nil {
					return fail(perr)
				}
			}
			if err != nil {
				break
			}
		}
		if err := b.Commit(); err != nil {
			return fail(err)
		}
		fmt.Fprintln(out, "OK")
	case "dump":
		w := bufio.NewWriter(out)
		for _, kv := range db.Scan("") {
			fmt.Fprintf(w, "%s %s\n", hex.EncodeToString([]byte(kv.Key)), hex.EncodeToString(kv.Value))
		}
		if err := w.Flush(); err != nil {
			return fail(err)
		}
	case "compact":
		if err := db.Compact(); err != nil {
			return fail(err)
		}
		fmt.Fprintln(out, "OK")
	case "serve":
		return serve(db, in, out)
	}
	return exitOK
}

func unhex(s string) ([]byte, error) {
	b, err := hex.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("bad hex %q: %w", truncate(s), err)
	}
	return b, nil
}

func truncate(s string) string {
	if len(s) > 40 {
		return s[:40] + "..."
	}
	return s
}

func decode2(hk, hv string) (string, []byte, error) {
	k, err := unhex(hk)
	if err != nil {
		return "", nil, err
	}
	v, err := unhex(hv)
	if err != nil {
		return "", nil, err
	}
	return string(k), v, nil
}

// addBatchOp parses "put <hexkey> <hexval>" or "del <hexkey>".
func addBatchOp(b *storage.Batch, f []string) error {
	switch {
	case len(f) == 3 && f[0] == "put":
		k, v, err := decode2(f[1], f[2])
		if err != nil {
			return err
		}
		b.Put(k, v)
	case len(f) == 2 && f[0] == "del":
		k, err := unhex(f[1])
		if err != nil {
			return err
		}
		b.Delete(string(k))
	default:
		return fmt.Errorf("bad batch op %q", strings.Join(f, " "))
	}
	return nil
}

func cmdCheck(path string, out, errw io.Writer) int {
	rep, err := storage.VerifyFile(path)
	if err != nil {
		fmt.Fprintf(errw, "CORRUPT %v\n", err)
		return codeFor(err)
	}
	// Trailing bytes are a torn tail / uncommitted batch: expected after a
	// crash, and cut off by the next open. They are reported, not an error.
	fmt.Fprintf(out, "OK size=%d good=%d trailing=%d records=%d batches=%d keys=%d\n",
		rep.FileSize, rep.GoodBytes, rep.TrailingBytes, rep.Records, rep.Batches, rep.LiveKeys)
	return exitOK
}

// serve speaks a line protocol on stdin/stdout. Each request line is
//
//	<id> put <hexkey> <hexval>
//	<id> del <hexkey>
//	<id> get <hexkey>
//	<id> batch <op>[ <op>...]     op = put:<hexkey>:<hexval> | del:<hexkey>
//	<id> compact
//
// Replies (one line each):
//
//	ACK <id>             the write is durable (fsync has returned)
//	VAL <id> <hexvalue>  / NIL <id>   for get
//	ERR <id> <code> <msg>   code = DISK_FULL | FAILED; the op was NOT applied
//
// An ACK is written only after the engine call has returned, and the engine
// returns only after fsync. By default requests run in order; with --concurrent each runs in its own
// goroutine (replies may reorder, match them by id). The first line printed
// is "READY" once recovery is done.
func serve(db *storage.Engine, in io.Reader, out io.Writer) int {
	var omu sync.Mutex
	w := bufio.NewWriter(out)
	reply := func(format string, a ...any) {
		omu.Lock()
		fmt.Fprintf(w, format+"\n", a...)
		w.Flush()
		omu.Unlock()
	}
	errReply := func(id string, err error) {
		code := "FAILED"
		if errors.Is(err, storage.ErrDiskFull) {
			code = "DISK_FULL"
		}
		reply("ERR %s %s %s", id, code, strings.ReplaceAll(err.Error(), "\n", " "))
	}
	reply("READY")

	var wg sync.WaitGroup
	rd := bufio.NewReaderSize(in, 1<<20)
	for {
		line, rerr := rd.ReadString('\n')
		if f := strings.Fields(line); len(f) >= 2 {
			handle := func(f []string) {
				id, op := f[0], f[1]
				switch {
				case op == "put" && len(f) == 4:
					k, v, err := decode2(f[2], f[3])
					if err == nil {
						err = db.Put(k, v)
					}
					if err != nil {
						errReply(id, err)
						return
					}
					reply("ACK %s", id)
				case op == "del" && len(f) == 3:
					k, err := unhex(f[2])
					if err == nil {
						err = db.Delete(string(k))
					}
					if err != nil {
						errReply(id, err)
						return
					}
					reply("ACK %s", id)
				case op == "get" && len(f) == 3:
					k, err := unhex(f[2])
					if err != nil {
						errReply(id, err)
						return
					}
					if v, ok := db.Get(string(k)); ok {
						reply("VAL %s %s", id, hex.EncodeToString(v))
					} else {
						reply("NIL %s", id)
					}
				case op == "batch" && len(f) >= 3:
					b := db.NewBatch()
					for _, tok := range f[2:] {
						p := strings.Split(tok, ":")
						if err := addBatchOp(b, p); err != nil {
							errReply(id, err)
							return
						}
					}
					if err := b.Commit(); err != nil {
						errReply(id, err)
						return
					}
					reply("ACK %s", id)
				case op == "compact" && len(f) == 2:
					if err := db.Compact(); err != nil {
						errReply(id, err)
						return
					}
					reply("ACK %s", id)
				default:
					reply("ERR %s FAILED bad request", id)
				}
			}
			if *concurrent {
				wg.Add(1)
				go func() { defer wg.Done(); handle(f) }()
			} else {
				handle(f) // in order: a get sees every earlier ACKed put
			}
		}
		if rerr != nil {
			break
		}
	}
	wg.Wait()
	return exitOK
}
