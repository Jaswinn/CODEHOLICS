package shell

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"godb/internal/storage"
)

func TestRunPlainTextSQLSession(t *testing.T) {
	db, err := storage.Open(filepath.Join(t.TempDir(), "shell.data"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	var output bytes.Buffer
	s := New(db, strings.NewReader(
		"CREATE TABLE users (name TEXT, age INT)\n"+
			"INSERT INTO users VALUES ('Asha', 31)\n"+
			"SELECT * FROM users\n"+
			"EXIT\n",
	), &output)
	if s.color {
		t.Fatal("buffered output should not enable ANSI colors")
	}
	if err := s.Run(); err != nil {
		t.Fatal(err)
	}

	got := output.String()
	for _, want := range []string{
		"CODEHOLIC",
		"Table users created.",
		"1 row(s) inserted.",
		"name | age",
		"Asha | 31",
		"(1 row)",
		"CH  godb>",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output does not contain %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "\x1b[") {
		t.Errorf("plain output contains ANSI escape codes:\n%s", got)
	}
}

func TestColoredPromptAndErrors(t *testing.T) {
	db, err := storage.Open(filepath.Join(t.TempDir(), "shell.data"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	var output bytes.Buffer
	s := New(db, strings.NewReader("NOT SQL\nEXIT\n"), &output)
	s.color = true
	if err := s.Run(); err != nil {
		t.Fatal(err)
	}

	got := output.String()
	if !strings.Contains(got, ansiOrange) {
		t.Error("expected orange ANSI styling in colored prompt")
	}
	if !strings.Contains(got, ansiRed+"Error:") {
		t.Error("expected red ANSI styling for command errors")
	}
	if !strings.Contains(got, "godb") {
		t.Error("expected godb prompt")
	}
}
