// Package logging configures a structured slog logger.
package logging

import (
	"io"
	"log/slog"
	"os"
)

// New returns a JSON slog.Logger writing to stderr at the given level.
func New(level slog.Level) *slog.Logger {
	return NewWith(os.Stderr, level)
}

// NewWith is the test-friendly variant that lets callers supply the writer.
func NewWith(w io.Writer, level slog.Level) *slog.Logger {
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level}))
}
