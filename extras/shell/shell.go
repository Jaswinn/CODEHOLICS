// Package shell is the interactive command prompt.
package shell

import (
	"bufio"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"godb/extras/catalog"
	"godb/extras/executor"
	"godb/extras/sql"
	"godb/internal/storage"
)

const helpText = `Commands (one per line; end a line with \ to continue on the next):

  CREATE TABLE name (col TYPE, ...)     types: INT, FLOAT, TEXT, BOOL
  DROP TABLE name
  ALTER TABLE name ADD COLUMN col TYPE
  ALTER TABLE name DROP COLUMN col
  SHOW TABLES
  DESCRIBE name
  INSERT INTO name [(cols)] VALUES (...), (...)
  SELECT cols|* FROM name [WHERE ...]
  UPDATE name SET col = value, ... [WHERE ...]
  DELETE FROM name [WHERE ...]

Shell commands:

  Only one godb process can open a database at a time; pass its file path on startup.
  The .lock sidecar is released by the OS on exit; do not delete it while running.
  .verify     check the log file on disk (read-only, never repairs)
  .compact    rewrite the log so it holds only live data
  .help       show this text
  EXIT        leave (also QUIT, .exit)
`

// Shell reads commands and prints results.
type Shell struct {
	db  *storage.Engine
	ex  *executor.Executor
	in  io.Reader
	out io.Writer
}

// New builds a shell on top of an open engine.
func New(db *storage.Engine, in io.Reader, out io.Writer) *Shell {
	return &Shell{db: db, ex: executor.New(catalog.New(db)), in: in, out: out}
}

// Run loops until EXIT or end of input.
func (s *Shell) Run() error {
	fmt.Fprintln(s.out, "godb - type HELP for commands, EXIT to quit.")
	sc := bufio.NewScanner(s.in)
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)

	var buf strings.Builder
	for {
		if buf.Len() == 0 {
			fmt.Fprint(s.out, "godb> ")
		} else {
			fmt.Fprint(s.out, "  ...> ")
		}
		if !sc.Scan() {
			fmt.Fprintln(s.out)
			return sc.Err()
		}
		line := strings.TrimRight(sc.Text(), " \t\r")
		if strings.HasSuffix(line, `\`) {
			buf.WriteString(strings.TrimSuffix(line, `\`))
			buf.WriteString(" ")
			continue
		}
		buf.WriteString(line)
		input := strings.TrimSpace(buf.String())
		buf.Reset()
		if input == "" {
			continue
		}
		if s.handle(input) {
			return nil
		}
	}
}

// handle runs one input line. It returns true when the shell should exit.
func (s *Shell) handle(input string) bool {
	if strings.HasPrefix(input, ".") {
		switch strings.ToLower(strings.TrimRight(input, "; ")) {
		case ".help":
			fmt.Fprint(s.out, helpText)
		case ".exit", ".quit":
			return true
		case ".verify":
			s.verify()
		case ".compact":
			if err := s.db.Compact(); err != nil {
				fmt.Fprintf(s.out, "Error: %v\n", err)
			} else {
				fmt.Fprintln(s.out, "Compacted.")
			}
		default:
			fmt.Fprintf(s.out, "Error: unknown shell command %q (try .help)\n", input)
		}
		return false
	}

	cmd, err := sql.Parse(input)
	if err != nil {
		fmt.Fprintf(s.out, "Error: %v\n", err)
		return false
	}
	switch cmd.(type) {
	case nil:
		return false
	case sql.Help:
		fmt.Fprint(s.out, helpText)
		return false
	case sql.Exit:
		return true
	}
	res, err := s.ex.Execute(cmd)
	if err != nil {
		fmt.Fprintf(s.out, "Error: %v\n", err)
		return false
	}
	s.print(res)
	return false
}

func (s *Shell) verify() {
	r, err := s.db.Verify()
	if err != nil {
		fmt.Fprintf(s.out, "Verify failed: %v\n", err)
		return
	}
	fmt.Fprintf(s.out, "File size:       %d bytes\n", r.FileSize)
	fmt.Fprintf(s.out, "Valid data:      %d bytes\n", r.GoodBytes)
	fmt.Fprintf(s.out, "Records:         %d (%d batches)\n", r.Records, r.Batches)
	fmt.Fprintf(s.out, "Live keys:       %d\n", r.LiveKeys)
	if r.TrailingBytes > 0 {
		fmt.Fprintf(s.out, "Warning: %d trailing bytes (torn write or uncommitted batch); they are ignored and removed the next time the file is opened.\n", r.TrailingBytes)
	} else {
		fmt.Fprintln(s.out, "OK: no problems found.")
	}
}

func (s *Shell) print(res *executor.Result) {
	if len(res.Columns) == 0 {
		if res.Message != "" {
			fmt.Fprintln(s.out, res.Message)
		}
		return
	}
	cells := make([][]string, len(res.Rows))
	widths := make([]int, len(res.Columns))
	for i, c := range res.Columns {
		widths[i] = utf8.RuneCountInString(c)
	}
	for i, row := range res.Rows {
		cells[i] = make([]string, len(row))
		for j, v := range row {
			str := catalog.FormatValue(v)
			cells[i][j] = str
			if n := utf8.RuneCountInString(str); j < len(widths) && n > widths[j] {
				widths[j] = n
			}
		}
	}
	line := func(vals []string) {
		var b strings.Builder
		for i, v := range vals {
			if i > 0 {
				b.WriteString(" | ")
			}
			b.WriteString(v)
			if i < len(vals)-1 {
				b.WriteString(strings.Repeat(" ", widths[i]-utf8.RuneCountInString(v)))
			}
		}
		fmt.Fprintln(s.out, b.String())
	}
	line(res.Columns)
	seps := make([]string, len(widths))
	for i, w := range widths {
		seps[i] = strings.Repeat("-", w)
	}
	fmt.Fprintln(s.out, strings.Join(seps, "-+-"))
	for _, r := range cells {
		line(r)
	}
	switch {
	case res.Message != "":
		fmt.Fprintln(s.out, res.Message)
	case len(res.Rows) == 1:
		fmt.Fprintln(s.out, "(1 row)")
	default:
		fmt.Fprintf(s.out, "(%d rows)\n", len(res.Rows))
	}
}
