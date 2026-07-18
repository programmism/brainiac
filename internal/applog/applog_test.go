package applog

import (
	"bytes"
	"encoding/json"
	"log"
	"log/slog"
	"strings"
	"testing"
)

func TestParseLevel(t *testing.T) {
	cases := map[string]slog.Level{
		"debug":   slog.LevelDebug,
		"info":    slog.LevelInfo,
		"warn":    slog.LevelWarn,
		"warning": slog.LevelWarn,
		"error":   slog.LevelError,
		"":        slog.LevelInfo, // default
		"bogus":   slog.LevelInfo, // unrecognized falls back, never silences
		" INFO ":  slog.LevelInfo, // trimmed + case-insensitive
	}
	for in, want := range cases {
		if got := ParseLevel(in); got != want {
			t.Errorf("ParseLevel(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestSetupJSONToWriter(t *testing.T) {
	var buf bytes.Buffer
	logger := Setup(&buf, nil, "json", "info")
	logger.Info("hello", "k", "v")

	var rec map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &rec); err != nil {
		t.Fatalf("output is not JSON: %v — %q", err, buf.String())
	}
	if rec["msg"] != "hello" || rec["level"] != "INFO" || rec["k"] != "v" {
		t.Fatalf("unexpected record: %+v", rec)
	}
}

func TestSetupTeesIntoRing(t *testing.T) {
	var out, ring bytes.Buffer
	logger := Setup(&out, &ring, "json", "info")
	logger.Info("teed")
	if !strings.Contains(ring.String(), "teed") {
		t.Fatalf("ring did not receive the log line: %q", ring.String())
	}
	if !strings.Contains(out.String(), "teed") {
		t.Fatalf("primary out did not receive the log line: %q", out.String())
	}
}

func TestSetupBridgesStdlibLog(t *testing.T) {
	var buf bytes.Buffer
	Setup(&buf, nil, "json", "info")
	// A plain log.Printf call must come out as a structured record, not raw text.
	log.Printf("bridged %d", 42)

	var rec map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &rec); err != nil {
		t.Fatalf("bridged stdlib line is not JSON: %v — %q", err, buf.String())
	}
	if rec["msg"] != "bridged 42" || rec["level"] != "INFO" {
		t.Fatalf("unexpected bridged record: %+v", rec)
	}
}

func TestSetupLevelFilters(t *testing.T) {
	var buf bytes.Buffer
	logger := Setup(&buf, nil, "json", "error")
	logger.Info("dropped")
	logger.Error("kept")
	s := buf.String()
	if strings.Contains(s, "dropped") {
		t.Fatalf("info line should have been filtered at error level: %q", s)
	}
	if !strings.Contains(s, "kept") {
		t.Fatalf("error line should have survived: %q", s)
	}
}

func TestSetupTextFormat(t *testing.T) {
	var buf bytes.Buffer
	logger := Setup(&buf, nil, "text", "info")
	logger.Info("plain", "k", "v")
	s := buf.String()
	// slog's text handler renders key=value, not JSON.
	if !strings.Contains(s, "msg=plain") || !strings.Contains(s, "k=v") {
		t.Fatalf("text format not applied: %q", s)
	}
	if strings.HasPrefix(strings.TrimSpace(s), "{") {
		t.Fatalf("expected text, got JSON: %q", s)
	}
}
