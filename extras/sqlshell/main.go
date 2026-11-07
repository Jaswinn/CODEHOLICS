// godb: a small crash-safe SQL database written in Go.
//
// Usage: godb [database-file]      (default: godb.data)
package main

import (
	"fmt"
	"os"

	"godb/extras/shell"
	"godb/internal/storage"
)

func main() {
	path := "godb.data"
	if len(os.Args) > 1 {
		path = os.Args[1]
	}
	db, err := storage.Open(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cannot open %s: %v\n", path, err)
		os.Exit(1)
	}
	code := 0
	if err := shell.New(db, os.Stdin, os.Stdout).Run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		code = 1
	}
	if err := db.Close(); err != nil {
		fmt.Fprintf(os.Stderr, "close: %v\n", err)
		code = 1
	}
	os.Exit(code)
}
