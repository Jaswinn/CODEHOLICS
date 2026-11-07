// torture: kill -9 the database over and over and check that every
// acknowledged write survived and every batch is all-or-nothing.
//
//	go build -o godb . && go build -o torture ./cmd/torture
//	./torture --bin ./godb --seed 1 --iterations 500 --mode mixed
//
// How it works
//   - Starts "godb serve --concurrent" as a child process; W writer
//     goroutines send ops through its stdin. Each writer owns a private key
//     prefix ("w<i>/") and has at most ONE op in flight.
//   - The oracle (this process's memory, which outlives every kill, plus a
//     debug log file) records an op as "acked" only when the child printed
//     ACK, which godb does only after fsync.
//   - After a random delay the child gets SIGKILL. Then we run
//     "godb check" and "godb dump" and require, per writer, that the
//     database equals either (acked state) or (acked state + the one
//     in-flight op applied as a whole). Anything else is a failure:
//     lost acked write, torn batch, phantom key, or wrong value.
//
// Modes: mixed, large-values, large-batches, recovery-crash, compaction,
// disk-full.
//
// Limits (be honest about them): SIGKILL stops the process between system
// calls, it does not cut power, so the OS page cache survives. A torn
// write therefore cannot occur naturally; recovery-crash mode simulates the
// result by appending a partial record or an uncommitted batch after the
// kill. Real power loss and real ENOSPC are NOT tested here.
package main

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"godb/internal/storage"
)

type op struct {
	kind string // put, del, batch, compact
	keys []string
	vals [][]byte // nil value => delete
}

func (o op) apply(m map[string][]byte) {
	for i, k := range o.keys {
		if o.vals[i] == nil {
			delete(m, k)
		} else {
			m[k] = o.vals[i]
		}
	}
}

type writer struct {
	id      int
	mu      sync.Mutex
	acked   map[string][]byte
	pending *op
	pid     string
	resp    chan struct{}
	rng     *rand.Rand
	seq     int
}

type config struct {
	bin, dir, mode      string
	seed                int64
	iterations, writers int
	minMs, maxMs        int
	maxBytes            int64
	keepGoing           bool
	compactEvery        int
}

type stats struct{ acked, errs, diskFull, killsDuringOpen, tears int }

func main() {
	var c config
	flag.StringVar(&c.bin, "bin", "./godb", "path to the godb binary")
	flag.StringVar(&c.dir, "dir", "", "work dir (default: temp dir)")
	flag.StringVar(&c.mode, "mode", "mixed", "mixed|large-values|large-batches|recovery-crash|compaction|disk-full")
	flag.Int64Var(&c.seed, "seed", 1, "random seed")
	flag.IntVar(&c.iterations, "iterations", 100, "number of kill cycles")
	flag.IntVar(&c.writers, "writers", 4, "concurrent writers")
	flag.IntVar(&c.minMs, "min-ms", 3, "min delay before kill (ms)")
	flag.IntVar(&c.maxMs, "max-ms", 60, "max delay before kill (ms)")
	flag.Int64Var(&c.maxBytes, "max-bytes", 0, "disk-full mode: cap (default 256KiB)")
	flag.BoolVar(&c.keepGoing, "keep-going", false, "after a failure, adopt the observed state and continue")
	flag.IntVar(&c.compactEvery, "compact-every", 0, "run an offline compact every N iterations (default: 5 for large modes)")
	flag.Parse()

	if c.mode == "disk-full" && c.maxBytes == 0 {
		c.maxBytes = 64 << 10
	}
	if c.compactEvery == 0 && (c.mode == "large-values" || c.mode == "large-batches") {
		c.compactEvery = 5
	}
	if c.dir == "" {
		d, err := os.MkdirTemp("", "torture-")
		must(err)
		c.dir = d
	}
	must(os.MkdirAll(c.dir, 0o755))
	bin, err := filepath.Abs(c.bin)
	must(err)
	c.bin = bin

	fails := run(c)
	if fails > 0 {
		fmt.Printf("FAIL mode=%s seed=%d: %d failure(s); evidence in %s\n", c.mode, c.seed, fails, c.dir)
		os.Exit(1)
	}
	fmt.Printf("PASS mode=%s seed=%d iterations=%d\n", c.mode, c.seed, c.iterations)
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "torture:", err)
		os.Exit(2)
	}
}

func run(c config) int {
	db := filepath.Join(c.dir, fmt.Sprintf("db-%s-%d", c.mode, c.seed))
	for _, s := range []string{"", ".lock", ".compact"} {
		os.Remove(db + s)
	}
	logf, err := os.Create(db + ".oplog")
	must(err)
	defer logf.Close()
	olog := bufio.NewWriter(logf)
	defer olog.Flush()

	rootRng := rand.New(rand.NewSource(c.seed))
	ws := make([]*writer, c.writers)
	for i := range ws {
		ws[i] = &writer{id: i, acked: map[string][]byte{}, resp: make(chan struct{}, 1)}
	}
	var st stats
	fails := 0

	for it := 1; it <= c.iterations; it++ {
		for _, w := range ws {
			w.rng = rand.New(rand.NewSource(c.seed*1_000_003 + int64(it)*101 + int64(w.id)))
		}
		delay := time.Duration(c.minMs+rootRng.Intn(c.maxMs-c.minMs+1)) * time.Millisecond

		runWriters(c, db, ws, delay, olog, &st)

		if c.mode == "recovery-crash" {
			// Damage the tail as a power cut would, then kill recovery itself.
			if rootRng.Intn(3) > 0 {
				tearTail(db, rootRng)
				st.tears++
			}
			for k := rootRng.Intn(3); k >= 0; k-- {
				killOpen(c, db, time.Duration(rootRng.Intn(20))*time.Millisecond)
				st.killsDuringOpen++
			}
		}

		if msg := verify(c, db, ws); msg != "" {
			fails++
			keep := fmt.Sprintf("%s.fail-iter%d", db, it)
			copyFile(db, keep)
			fmt.Printf("FAIL iter=%d mode=%s seed=%d: %s\n  saved %s\n", it, c.mode, c.seed, msg, keep)
			if !c.keepGoing {
				return fails
			}
			adopt(c, db, ws)
		}

		if c.compactEvery > 0 && it%c.compactEvery == 0 {
			out, err := exec.Command(c.bin, "compact", db).CombinedOutput()
			if err != nil {
				fmt.Printf("offline compact failed: %v %s\n", err, out)
			}
		}
		if it%50 == 0 || it == c.iterations {
			fmt.Printf("  iter %d/%d ok (acked=%d err=%d disk_full=%d)\n", it, c.iterations, st.acked, st.errs, st.diskFull)
		}
	}
	fmt.Printf("  totals: acked=%d errors=%d disk_full=%d tears=%d kills_during_open=%d\n",
		st.acked, st.errs, st.diskFull, st.tears, st.killsDuringOpen)
	return fails
}

// ---------------------------------------------------------------------------

func serveArgs(c config, db string) []string {
	a := []string{"serve", "--concurrent"}
	if c.maxBytes > 0 {
		a = append(a, "--max-bytes", strconv.FormatInt(c.maxBytes, 10))
	}
	return append(a, db)
}

func runWriters(c config, db string, ws []*writer, delay time.Duration, olog *bufio.Writer, st *stats) {
	cmd := exec.Command(c.bin, serveArgs(c, db)...)
	stdin, err := cmd.StdinPipe()
	must(err)
	stdout, err := cmd.StdoutPipe()
	must(err)
	cmd.Stderr = io.Discard
	must(cmd.Start())

	var inMu, logMu sync.Mutex
	done := make(chan struct{})
	ready := make(chan struct{})

	var rwg sync.WaitGroup
	rwg.Add(1)
	go func() { // reader: the only place the oracle learns about ACKs
		defer rwg.Done()
		defer close(done)
		rd := bufio.NewReaderSize(stdout, 1<<20)
		once := false
		for {
			line, err := rd.ReadString('\n')
			f := strings.Fields(line)
			if len(f) > 0 && err == nil {
				if f[0] == "READY" && !once {
					once = true
					close(ready)
					continue
				}
				if len(f) >= 2 {
					handleReply(ws, f, olog, &logMu, st)
				}
			}
			if err != nil {
				return
			}
		}
	}()

	select {
	case <-ready:
	case <-done: // server died before READY (e.g. recovery error)
	}

	stop := make(chan struct{})
	var wwg sync.WaitGroup
	for _, w := range ws {
		// A reply that landed after the previous kill leaves a stale token;
		// it would let the writer run ahead of its own ACK.
		select {
		case <-w.resp:
		default:
		}
		wwg.Add(1)
		go func(w *writer) {
			defer wwg.Done()
			for {
				select {
				case <-stop:
					return
				case <-done:
					return
				default:
				}
				o, line := genOp(c, w)
				w.mu.Lock()
				if o.kind != "compact" {
					w.pending = &o
				}
				w.pid = fmt.Sprintf("w%d-%d", w.id, w.seq)
				w.seq++
				id := w.pid
				w.mu.Unlock()

				logMu.Lock()
				fmt.Fprintf(olog, "SENT %s %s keys=%d\n", id, o.kind, len(o.keys))
				logMu.Unlock()

				inMu.Lock()
				_, err := io.WriteString(stdin, id+" "+line+"\n")
				inMu.Unlock()
				if err != nil {
					return
				}
				select {
				case <-w.resp:
				case <-done:
					return
				case <-stop:
					return
				}
			}
		}(w)
	}

	time.Sleep(delay)
	_ = cmd.Process.Kill() // SIGKILL
	close(stop)
	_ = cmd.Wait()
	rwg.Wait()
	wwg.Wait()
	olog.Flush()
}

func handleReply(ws []*writer, f []string, olog *bufio.Writer, logMu *sync.Mutex, st *stats) {
	var id string
	switch f[0] {
	case "ACK", "ERR", "NIL", "VAL":
		id = f[1]
	default:
		return
	}
	wi, _ := strconv.Atoi(strings.TrimPrefix(strings.SplitN(id, "-", 2)[0], "w"))
	if wi < 0 || wi >= len(ws) {
		return
	}
	w := ws[wi]
	w.mu.Lock()
	if w.pid == id {
		switch f[0] {
		case "ACK":
			if w.pending != nil {
				w.pending.apply(w.acked)
			}
			st.acked++
		case "ERR":
			// The engine reports ERR only when the op was not applied.
			st.errs++
			if len(f) > 2 && f[2] == "DISK_FULL" {
				st.diskFull++
			}
		}
		w.pending = nil
	}
	w.mu.Unlock()
	logMu.Lock()
	fmt.Fprintf(olog, "%s %s\n", f[0], id)
	logMu.Unlock()
	select {
	case w.resp <- struct{}{}:
	default:
	}
}

// ---------------------------------------------------------------------------

func genOp(c config, w *writer) (op, string) {
	r := w.rng
	keyspace, maxVal, batchLo, batchHi := 16, 200, 2, 8
	switch c.mode {
	case "large-values":
		maxVal = 1 << 20 // storage.MaxValueSize
	case "large-batches":
		keyspace, maxVal, batchLo, batchHi = 2000, 64, 500, 5000
	case "disk-full":
		maxVal = 4096
	}
	key := func() string { return fmt.Sprintf("w%d/k%d", w.id, r.Intn(keyspace)) }
	val := func() []byte {
		n := 1 + r.Intn(maxVal) // never empty: an empty hex token breaks the line protocol
		if c.mode == "large-values" && r.Intn(2) == 0 {
			n = maxVal/2 + 1 + r.Intn(maxVal/2)
		}
		b := make([]byte, n)
		r.Read(b)
		return b
	}
	p := r.Intn(100)
	switch {
	case c.mode == "compaction" && w.id == 0 && p < 25:
		return op{kind: "compact"}, "compact"
	case p < 50:
		k, v := key(), val()
		return op{kind: "put", keys: []string{k}, vals: [][]byte{v}},
			"put " + hexOrEmpty(k) + " " + hexOrEmpty(string(v))
	case p < 65:
		k := key()
		return op{kind: "del", keys: []string{k}, vals: [][]byte{nil}}, "del " + hexOrEmpty(k)
	default:
		n := batchLo + r.Intn(batchHi-batchLo+1)
		o := op{kind: "batch"}
		var sb strings.Builder
		sb.WriteString("batch")
		for i := 0; i < n; i++ {
			k := key()
			if r.Intn(5) == 0 {
				o.keys, o.vals = append(o.keys, k), append(o.vals, nil)
				sb.WriteString(" del:" + hexOrEmpty(k))
			} else {
				v := val()
				o.keys, o.vals = append(o.keys, k), append(o.vals, v)
				sb.WriteString(" put:" + hexOrEmpty(k) + ":" + hexOrEmpty(string(v)))
			}
		}
		return o, sb.String()
	}
}

// hexOrEmpty hex-encodes s (the generator never produces empty values).
func hexOrEmpty(s string) string { return hex.EncodeToString([]byte(s)) }

// ---------------------------------------------------------------------------

func verify(c config, db string, ws []*writer) string {
	out, err := exec.Command(c.bin, "check", db).CombinedOutput()
	if err != nil {
		return fmt.Sprintf("godb check failed: %v: %s", err, bytes.TrimSpace(out))
	}
	obs, err := dump(c, db) // this also runs recovery (open)
	if err != nil {
		return err.Error()
	}
	out, err = exec.Command(c.bin, "check", db).CombinedOutput()
	if err != nil || !strings.Contains(string(out), "trailing=0") {
		return fmt.Sprintf("after recovery, check shows a dirty tail or error: %v %s", err, bytes.TrimSpace(out))
	}
	if _, err := os.Stat(db + ".compact"); err == nil {
		return "stale .compact file left after recovery"
	}
	if c.maxBytes > 0 {
		if fi, err := os.Stat(db); err == nil && fi.Size() > c.maxBytes {
			return fmt.Sprintf("file size %d exceeds --max-bytes %d", fi.Size(), c.maxBytes)
		}
	}

	claimed := map[string]bool{}
	for _, w := range ws {
		prefix := fmt.Sprintf("w%d/", w.id)
		got := map[string][]byte{}
		for k, v := range obs {
			if strings.HasPrefix(k, prefix) {
				got[k] = v
				claimed[k] = true
			}
		}
		w.mu.Lock()
		okAcked := equal(got, w.acked)
		okPending := false
		if !okAcked && w.pending != nil {
			alt := clone(w.acked)
			w.pending.apply(alt)
			okPending = equal(got, alt)
		}
		var msg string
		if !okAcked && !okPending {
			msg = fmt.Sprintf("writer %d: database matches neither acked state nor acked+in-flight (%s); %s",
				w.id, pendingDesc(w.pending), diff(got, w.acked))
		}
		if okPending {
			w.pending.apply(w.acked)
		}
		w.pending = nil
		w.mu.Unlock()
		if msg != "" {
			return msg
		}
	}
	for k := range obs {
		if !claimed[k] {
			return fmt.Sprintf("unknown key %q in database", k)
		}
	}
	return ""
}

func pendingDesc(p *op) string {
	if p == nil {
		return "no op in flight"
	}
	return fmt.Sprintf("in-flight %s with %d key(s)", p.kind, len(p.keys))
}

func diff(got, want map[string][]byte) string {
	var lost, wrong, extra []string
	for k, v := range want {
		g, ok := got[k]
		if !ok {
			lost = append(lost, k)
		} else if !bytes.Equal(g, v) {
			wrong = append(wrong, k)
		}
	}
	for k := range got {
		if _, ok := want[k]; !ok {
			extra = append(extra, k)
		}
	}
	sort.Strings(lost)
	sort.Strings(wrong)
	sort.Strings(extra)
	trim := func(s []string) []string {
		if len(s) > 5 {
			return s[:5]
		}
		return s
	}
	return fmt.Sprintf("lost=%d %v wrong=%d %v extra=%d %v", len(lost), trim(lost), len(wrong), trim(wrong), len(extra), trim(extra))
}

func equal(a, b map[string][]byte) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || !bytes.Equal(v, w) {
			return false
		}
	}
	return true
}

func clone(m map[string][]byte) map[string][]byte {
	n := make(map[string][]byte, len(m))
	for k, v := range m {
		n[k] = v
	}
	return n
}

func dump(c config, db string) (map[string][]byte, error) {
	args := []string{"dump"}
	if c.maxBytes > 0 {
		args = append(args, "--max-bytes", strconv.FormatInt(c.maxBytes, 10))
	}
	cmd := exec.Command(c.bin, append(args, db)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("godb dump (open/recovery) failed: %v: %s", err, strings.TrimSpace(stderr.String()))
	}
	m := map[string][]byte{}
	for _, line := range strings.Split(string(out), "\n") {
		if line == "" {
			continue
		}
		f := strings.SplitN(line, " ", 2)
		k, err1 := hex.DecodeString(f[0])
		var v []byte
		var err2 error
		if len(f) == 2 {
			v, err2 = hex.DecodeString(f[1])
		}
		if err1 != nil || err2 != nil {
			return nil, fmt.Errorf("bad dump line %.60q", line)
		}
		m[string(k)] = v
	}
	return m, nil
}

// adopt makes the oracle equal to whatever the database holds (keep-going).
func adopt(c config, db string, ws []*writer) {
	obs, err := dump(c, db)
	if err != nil {
		return
	}
	for _, w := range ws {
		prefix := fmt.Sprintf("w%d/", w.id)
		w.mu.Lock()
		w.acked = map[string][]byte{}
		for k, v := range obs {
			if strings.HasPrefix(k, prefix) {
				w.acked[k] = v
			}
		}
		w.pending = nil
		w.mu.Unlock()
	}
}

// ---------------------------------------------------------------------------
// recovery-crash helpers

// tearTail appends bytes that look like an interrupted, never-acknowledged
// write: a prefix of a record, or a whole batch member with no commit.
func tearTail(db string, r *rand.Rand) {
	f, err := os.OpenFile(db, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	rec := storage.Record{Type: storage.OpPut, Seq: 1 << 40, Key: []byte("w0/torn"), Value: bytes.Repeat([]byte{0xAB}, 1+r.Intn(500))}
	if r.Intn(2) == 0 {
		rec.BatchID = 1 << 40 // complete record, but its batch never commits
		f.Write(rec.Serialize())
		return
	}
	b := rec.Serialize()
	f.Write(b[:1+r.Intn(len(b)-1)])
}

// killOpen starts recovery and kills it after d, so Open's truncate+fsync
// can be interrupted.
func killOpen(c config, db string, d time.Duration) {
	cmd := exec.Command(c.bin, "open", db)
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	if cmd.Start() != nil {
		return
	}
	time.Sleep(d)
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
}

func copyFile(src, dst string) {
	b, err := os.ReadFile(src)
	if err == nil {
		_ = os.WriteFile(dst, b, 0o644)
	}
}
