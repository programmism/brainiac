// Package logbuf is an in-memory, bounded, thread-safe log sink. It implements
// io.Writer so it can sit behind the standard logger and the HTTP access logger
// (via io.MultiWriter alongside stderr), keeping the last N lines available to
// the WebUI Logs tab (#166) without touching disk. Lines are redacted of obvious
// secrets before storage so exposing them never leaks a credential.
package logbuf

import (
	"bytes"
	"regexp"
	"strings"
	"sync"
)

// DefaultMax is the number of lines kept when New is called with n <= 0.
const DefaultMax = 2000

// Buffer keeps the most recent lines in a ring, discarding the oldest as new
// ones arrive.
type Buffer struct {
	mu      sync.Mutex
	lines   []string
	max     int
	partial []byte // bytes since the last newline, awaiting completion
}

// New builds a Buffer that retains up to n lines (DefaultMax if n <= 0).
func New(n int) *Buffer {
	if n <= 0 {
		n = DefaultMax
	}
	return &Buffer{max: n, lines: make([]string, 0, n)}
}

// secretPatterns redact obvious credentials so the WebUI/API never echoes one.
// Conservative by design: better to over-mask a token-shaped string than to
// leak a real secret through the log viewer.
var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{20,}`),                               // classic GitHub tokens
	regexp.MustCompile(`github_pat_[A-Za-z0-9_]{20,}`),                             // fine-grained PATs
	regexp.MustCompile(`(?i)(bearer\s+)[A-Za-z0-9._~+/=-]{8,}`),                    // Authorization: Bearer …
	regexp.MustCompile(`(?i)((?:token|password|secret|api[_-]?key)\s*[=:]\s*)\S+`), // key=value secrets
}

// Redact masks secret-shaped substrings in a single log line.
func Redact(s string) string {
	for i, re := range secretPatterns {
		switch i {
		case 2: // preserve the "Bearer " prefix, mask the token
			s = re.ReplaceAllString(s, `${1}[REDACTED]`)
		case 3: // preserve the key= prefix, mask the value
			s = re.ReplaceAllString(s, `${1}[REDACTED]`)
		default:
			s = re.ReplaceAllString(s, "[REDACTED]")
		}
	}
	return s
}

// Write accumulates bytes and stores each completed (newline-terminated) line.
// It satisfies io.Writer and never returns an error, so wrapping it in an
// io.MultiWriter can't break the real logging destination.
func (b *Buffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.partial = append(b.partial, p...)
	for {
		i := bytes.IndexByte(b.partial, '\n')
		if i < 0 {
			break
		}
		line := string(b.partial[:i])
		b.partial = b.partial[i+1:]
		b.append(strings.TrimRight(line, "\r"))
	}
	// Guard against an unbounded partial line (no newline ever arrives).
	if len(b.partial) > 64<<10 {
		b.append(string(b.partial))
		b.partial = b.partial[:0]
	}
	return len(p), nil
}

// append stores one line, dropping the oldest when at capacity.
func (b *Buffer) append(line string) {
	line = Redact(line)
	if len(b.lines) >= b.max {
		copy(b.lines, b.lines[1:])
		b.lines[len(b.lines)-1] = line
		return
	}
	b.lines = append(b.lines, line)
}

// Lines returns a copy of the most recent lines, up to limit (all when limit
// <= 0). Newest lines are last.
func (b *Buffer) Lines(limit int) []string {
	b.mu.Lock()
	defer b.mu.Unlock()

	n := len(b.lines)
	if limit > 0 && limit < n {
		out := make([]string, limit)
		copy(out, b.lines[n-limit:])
		return out
	}
	out := make([]string, n)
	copy(out, b.lines)
	return out
}
