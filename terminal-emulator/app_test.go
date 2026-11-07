package main

import (
	"context"
	"testing"
)

func TestTerminalDimensionValidation(t *testing.T) {
	app := NewApp()
	if _, err := app.StartTerminal(1, 1); err == nil {
		t.Fatal("StartTerminal accepted out-of-range dimensions")
	}
	if err := app.ResizeTerminal("missing", 1, 1); err == nil {
		t.Fatal("ResizeTerminal accepted out-of-range dimensions")
	}
}

func TestCollectSystemStats(t *testing.T) {
	stats, err := collectSystemStats(context.Background())
	if err != nil {
		t.Fatalf("collectSystemStats() error = %v", err)
	}
	if stats.MemoryTotalGB <= 0 {
		t.Fatalf("expected positive total memory, got %f", stats.MemoryTotalGB)
	}
	if stats.UptimeSeconds == 0 {
		t.Fatal("expected positive uptime")
	}
	if stats.CPU < 0 || stats.CPU > 100 {
		t.Fatalf("CPU usage out of range: %f", stats.CPU)
	}
}
