package main

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/UserExistsError/conpty"
)

func TestConPTYRunsPowerShell(t *testing.T) {
	console, err := conpty.Start(
		`powershell.exe -NoLogo -NoProfile -Command "Write-Output 'CODEHOLIC_CONPTY_OK'"`,
		conpty.ConPtyDimensions(80, 24),
	)
	if err != nil {
		t.Fatalf("start PowerShell in ConPTY: %v", err)
	}
	defer console.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	type readResult struct {
		output []byte
		err    error
	}
	outputCh := make(chan readResult, 1)
	go func() {
		var output []byte
		buffer := make([]byte, 4096)
		for {
			n, err := console.Read(buffer)
			output = append(output, buffer[:n]...)
			if err != nil || strings.Contains(string(output), "CODEHOLIC_CONPTY_OK") {
				outputCh <- readResult{output: output, err: err}
				return
			}
		}
	}()

	var result readResult
	select {
	case result = <-outputCh:
	case <-ctx.Done():
		t.Fatal(fmt.Errorf("timed out waiting for PowerShell output: %w", ctx.Err()))
	}
	if result.err != nil {
		t.Fatalf("read ConPTY output: %v", result.err)
	}
	exitCode, err := console.Wait(ctx)
	if err != nil {
		t.Fatalf("wait for PowerShell: %v", err)
	}
	if exitCode != 0 {
		t.Fatalf("PowerShell exit code = %d", exitCode)
	}
	if !strings.Contains(string(result.output), "CODEHOLIC_CONPTY_OK") {
		t.Fatalf("PowerShell output did not contain marker: %q", result.output)
	}
}
