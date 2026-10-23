// Package logging builds the one structured logger the process uses.
package logging

import (
	"log/slog"
	"os"
)

// New returns a JSON logger at the configured level. JSON is deliberate: the
// access log and job events are machine-readable without adding an APM vendor.
func New(level slog.Level) *slog.Logger {
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
}
