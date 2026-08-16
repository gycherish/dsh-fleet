package proxy

import (
	"io"
	"log/slog"
)

// discardLogger keeps test output readable: these handlers log on paths the
// tests deliberately exercise.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
