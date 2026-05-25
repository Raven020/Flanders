package logging

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// New must write structured lines to a real file (segregated from the TUI's
// stdout) and preserve key/value context — the journal/diagnostics depend on it.
func TestNewWritesFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "flanders.log")
	log, err := New(path, slog.LevelInfo)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	log.Info("starting", "version", "0.0.1")
	if err := log.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	out := string(data)
	if !strings.Contains(out, "starting") || !strings.Contains(out, "version=0.0.1") {
		t.Errorf("log file missing message/context, got: %q", out)
	}
}

// Leveling must suppress messages below the configured level so a non-debug run
// stays quiet, and emit those at/above it.
func TestLevelFiltering(t *testing.T) {
	var buf bytes.Buffer
	log := NewWriter(&buf, slog.LevelInfo)
	log.Debug("suppressed")
	if buf.Len() != 0 {
		t.Errorf("Debug emitted at Info level: %q", buf.String())
	}
	log.Info("emitted")
	if !strings.Contains(buf.String(), "emitted") {
		t.Errorf("Info not emitted: %q", buf.String())
	}
}

func TestParseLevel(t *testing.T) {
	ok := map[string]slog.Level{
		"":        slog.LevelInfo,
		"info":    slog.LevelInfo,
		"DEBUG":   slog.LevelDebug,
		" warn ":  slog.LevelWarn,
		"warning": slog.LevelWarn,
		"Error":   slog.LevelError,
	}
	for in, want := range ok {
		got, err := ParseLevel(in)
		if err != nil {
			t.Errorf("ParseLevel(%q) unexpected error: %v", in, err)
		}
		if got != want {
			t.Errorf("ParseLevel(%q) = %v, want %v", in, got, want)
		}
	}
	if _, err := ParseLevel("bogus"); err == nil {
		t.Errorf("ParseLevel(\"bogus\") = nil error, want error")
	}
}

// NewWriter must not own its writer: Close is a safe no-op so callers can reuse
// the buffer.
func TestNewWriterCloseNoOp(t *testing.T) {
	var buf bytes.Buffer
	log := NewWriter(&buf, slog.LevelInfo)
	if err := log.Close(); err != nil {
		t.Errorf("Close on NewWriter logger: %v", err)
	}
}
