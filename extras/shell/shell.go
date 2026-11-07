// Package shell is the interactive command prompt.
package shell

import (
	"bufio"
	"fmt"
	"io"
	"os"
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
	db    *storage.Engine
	ex    *executor.Executor
	in    io.Reader
	out   io.Writer
	color bool
}

// New builds a shell on top of an open engine.
func New(db *storage.Engine, in io.Reader, out io.Writer) *Shell {
	return &Shell{
		db:    db,
		ex:    executor.New(catalog.New(db)),
		in:    in,
		out:   out,
		color: colorOutput(out),
	}
}

// Run loops until EXIT or end of input.
func (s *Shell) Run() error {
	s.banner()
	sc := bufio.NewScanner(s.in)
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)

	var buf strings.Builder
	for {
		if buf.Len() == 0 {
			fmt.Fprint(s.out, s.styled("CH", ansiOrange, ansiBold), s.styled("  godb", ansiWhite), s.styled("> ", ansiOrange, ansiBold))
		} else {
			fmt.Fprint(s.out, s.styled("  ...> ", ansiMuted))
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
				s.writeError("Error: %v\n", err)
			} else {
				fmt.Fprintln(s.out, s.styled("Compacted.", ansiGreen))
			}
		default:
			s.writeError("Error: unknown shell command %q (try .help)\n", input)
		}
		return false
	}

	cmd, err := sql.Parse(input)
	if err != nil {
		s.writeError("Error: %v\n", err)
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
		s.writeError("Error: %v\n", err)
		return false
	}
	s.print(res)
	return false
}

func (s *Shell) verify() {
	r, err := s.db.Verify()
	if err != nil {
		s.writeError("Verify failed: %v\n", err)
		return
	}
	fmt.Fprintf(s.out, "File size:       %d bytes\n", r.FileSize)
	fmt.Fprintf(s.out, "Valid data:      %d bytes\n", r.GoodBytes)
	fmt.Fprintf(s.out, "Records:         %d (%d batches)\n", r.Records, r.Batches)
	fmt.Fprintf(s.out, "Live keys:       %d\n", r.LiveKeys)
	if r.TrailingBytes > 0 {
		fmt.Fprintf(s.out, "%s\n", s.styled(fmt.Sprintf("Warning: %d trailing bytes (torn write or uncommitted batch); they are ignored and removed the next time the file is opened.", r.TrailingBytes), ansiYellow))
	} else {
		fmt.Fprintln(s.out, s.styled("OK: no problems found.", ansiGreen))
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
	line := func(vals []string, heading bool) {
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
		if heading {
			fmt.Fprintln(s.out, s.styled(b.String(), ansiOrange, ansiBold))
			return
		}
		fmt.Fprintln(s.out, b.String())
	}
	line(res.Columns, true)
	seps := make([]string, len(widths))
	for i, w := range widths {
		seps[i] = strings.Repeat("-", w)
	}
	fmt.Fprintln(s.out, s.styled(strings.Join(seps, "-+-"), ansiMuted))
	for _, r := range cells {
		line(r, false)
	}
	switch {
	case res.Message != "":
		fmt.Fprintln(s.out, s.styled(res.Message, ansiGreen))
	case len(res.Rows) == 1:
		fmt.Fprintln(s.out, s.styled("(1 row)", ansiMuted))
	default:
		fmt.Fprintln(s.out, s.styled(fmt.Sprintf("(%d rows)", len(res.Rows)), ansiMuted))
	}
}

const (
	ansiReset  = "\x1b[0m"
	ansiBold   = "\x1b[1m"
	ansiOrange = "\x1b[38;5;208m"
	ansiWhite  = "\x1b[97m"
	ansiMuted  = "\x1b[38;5;245m"
	ansiGreen  = "\x1b[38;5;114m"
	ansiRed    = "\x1b[38;5;203m"
	ansiYellow = "\x1b[38;5;221m"
)

func colorOutput(out io.Writer) bool {
	if _, disabled := os.LookupEnv("NO_COLOR"); disabled {
		return false
	}
	if strings.EqualFold(os.Getenv("TERM"), "dumb") {
		return false
	}
	file, ok := out.(*os.File)
	if !ok {
		return false
	}
	info, err := file.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

func (s *Shell) styled(text string, styles ...string) string {
	if !s.color || len(styles) == 0 {
		return text
	}
	return strings.Join(styles, "") + text + ansiReset
}

func (s *Shell) writeError(format string, args ...any) {
	fmt.Fprint(s.out, s.styled(fmt.Sprintf(format, args...), ansiRed))
}

func (s *Shell) banner() {
	border := "+------------------------------------------------------+"
	fmt.Fprintln(s.out, s.styled(border, ansiOrange))
	fmt.Fprintln(s.out, s.bannerLine("CODEHOLIC / SQL - local database console", ansiOrange, ansiBold))
	fmt.Fprintln(s.out, s.bannerLine("Type HELP for commands; EXIT to quit.", ansiMuted))
	fmt.Fprintln(s.out, s.styled(border, ansiOrange))
}

func (s *Shell) bannerLine(text string, styles ...string) string {
	padding := 52 - utf8.RuneCountInString(text)
	if padding < 0 {
		text = string([]rune(text)[:52])
		padding = 0
	}
	return s.styled("|", ansiOrange) +
		" " +
		s.styled(text, styles...) +
		strings.Repeat(" ", padding+1) +
		s.styled("|", ansiOrange)
}
