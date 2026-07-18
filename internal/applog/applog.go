// Package applog configures the application's structured logger (#258).
//
// The access log already emits JSON to stdout with a request-id (see
// internal/server); this package does the same for the *application* logger —
// startup, migrations, auto-import, reload, and the ≥500 lines the HTTP server
// records. It writes JSON (or text) records to a chosen stream, optionally teed
// into the WebUI's in-memory ring, at a level from config.
//
// Crucially it also bridges the standard library `log` package. The app logs
// via log.Printf across cmd/ and internal/server; rather than rewrite every call
// site, Setup routes stdlib log through slog so those lines become the same
// structured records. Docker's json-file driver rotates stdout, so the durable
// log survives a crash — the 2000-line ring is only a UI convenience now.
package applog

import (
	"context"
	"io"
	"log"
	"log/slog"
	"strings"
)

// Setup builds a slog handler (JSON by default, "text" on request) writing to
// out — teed into ring when non-nil — installs it as slog's default, and routes
// the standard log package through it at info level. It returns the logger.
//
// format is "json" (default) or "text"; level is debug|info|warn|error
// (default info). Bridged stdlib lines carry no inherent level, so they emit at
// info: raising the level above info therefore also drops plain log.Printf
// output, which is the documented trade-off of a level filter.
func Setup(out, ring io.Writer, format, level string) *slog.Logger {
	w := out
	if ring != nil {
		w = io.MultiWriter(out, ring)
	}
	opts := &slog.HandlerOptions{Level: ParseLevel(level)}
	var h slog.Handler
	if strings.EqualFold(strings.TrimSpace(format), "text") {
		h = slog.NewTextHandler(w, opts)
	} else {
		h = slog.NewJSONHandler(w, opts)
	}
	logger := slog.New(h)
	slog.SetDefault(logger)

	// Bridge stdlib log → slog. Clear log's own flags so it doesn't prefix a
	// second timestamp inside the JSON message; each written line is one record.
	log.SetFlags(0)
	log.SetOutput(&bridge{logger: logger})
	return logger
}

// bridge turns each stdlib log line into one info-level slog record.
type bridge struct{ logger *slog.Logger }

func (b *bridge) Write(p []byte) (int, error) {
	msg := strings.TrimRight(string(p), "\n")
	b.logger.LogAttrs(context.Background(), slog.LevelInfo, msg)
	return len(p), nil
}

// ParseLevel maps a level name to slog.Level, defaulting to info for empty or
// unrecognized input so a typo never silences the logs entirely.
func ParseLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
