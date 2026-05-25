// Package logging provides Flanders's leveled, file-backed logger.
//
// Why file-backed and not stdout: the TUI (specs/04-tui.md) owns the terminal,
// so diagnostic logs must never interleave with the dashboard. Logs are
// segregated to a file under .flanders/ (specs/01-ralph-loop.md §journal) so an
// unattended multi-day run leaves a readable trace without corrupting the TUI.
// Built on log/slog so leveling and structured key/value context come for free
// and every consumer shares one logger shape (single source of truth).
package logging

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
)

// Logger wraps an *slog.Logger together with the writer it owns, so callers can
// Close the underlying file. The embedded *slog.Logger supplies Debug/Info/
// Warn/Error directly.
type Logger struct {
	*slog.Logger
	w io.WriteCloser
}

// New opens (creating, then appending to) a log file at path and returns a
// leveled logger writing structured text to it. The caller owns Close.
func New(path string, level slog.Level) (*Logger, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open log %q: %w", path, err)
	}
	h := slog.NewTextHandler(f, &slog.HandlerOptions{Level: level})
	return &Logger{Logger: slog.New(h), w: f}, nil
}

// NewWriter builds a logger over an arbitrary writer (e.g. a bytes.Buffer in
// tests). The returned Logger does not own w, so Close is a no-op.
func NewWriter(w io.Writer, level slog.Level) *Logger {
	h := slog.NewTextHandler(w, &slog.HandlerOptions{Level: level})
	return &Logger{Logger: slog.New(h), w: nopCloser{w}}
}

// Close flushes and closes the underlying file (a no-op for NewWriter loggers).
func (l *Logger) Close() error { return l.w.Close() }

type nopCloser struct{ io.Writer }

func (nopCloser) Close() error { return nil }

// ParseLevel maps a config string (debug|info|warn|error, case-insensitive) to
// an slog.Level. An empty string defaults to info; anything else errors.
func ParseLevel(s string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "info":
		return slog.LevelInfo, nil
	case "debug":
		return slog.LevelDebug, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return slog.LevelInfo, fmt.Errorf("unknown log level %q", s)
	}
}
