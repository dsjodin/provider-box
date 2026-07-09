// Package logs extracts recent error-level lines from tailed container logs.
// dns-sync (and the other Go services) emit slog JSON, so the primary path
// parses the "level" field; non-JSON lines fall back to a best-effort token
// match. This is a bounded tail, not a log store.
package logs

import (
	"encoding/json"
	"strings"
	"time"
)

// Entry is one error line attributed to a container.
type Entry struct {
	Container string
	Time      string // as-emitted timestamp when the line is structured JSON
	Level     string
	Message   string
	Raw       string // original line, for non-JSON fallback
}

type jsonLine struct {
	Time  string `json:"time"`
	Level string `json:"level"`
	Msg   string `json:"msg"`
}

// Extract returns the error-level entries from a container's tailed lines,
// oldest-first. Structured JSON lines are parsed for level>=error; other lines
// match a small set of error tokens.
func Extract(container string, lines []string) []Entry {
	var out []Entry
	for _, ln := range lines {
		ln = strings.TrimSpace(stripStreamPrefix(ln))
		if ln == "" {
			continue
		}
		if e, ok := parseJSON(container, ln); ok {
			if isErrorLevel(e.Level) {
				out = append(out, e)
			}
			continue
		}
		if isErrorText(ln) {
			out = append(out, Entry{Container: container, Level: "error", Message: ln, Raw: ln})
		}
	}
	return out
}

func parseJSON(container, ln string) (Entry, bool) {
	if !strings.HasPrefix(ln, "{") {
		return Entry{}, false
	}
	var j jsonLine
	if err := json.Unmarshal([]byte(ln), &j); err != nil || j.Level == "" {
		return Entry{}, false
	}
	t := j.Time
	if parsed, err := time.Parse(time.RFC3339Nano, j.Time); err == nil {
		t = parsed.Format(time.RFC3339)
	}
	return Entry{
		Container: container,
		Time:      t,
		Level:     j.Level,
		Message:   j.Msg,
		Raw:       ln,
	}, true
}

// isErrorLevel reports whether a slog-style level is error or worse. slog emits
// "ERROR" (and "ERROR+N" for higher); other loggers use FATAL/CRITICAL/PANIC.
func isErrorLevel(level string) bool {
	l := strings.ToUpper(strings.TrimSpace(level))
	switch {
	case strings.HasPrefix(l, "ERROR"), strings.HasPrefix(l, "ERR"):
		return true
	case l == "FATAL", l == "CRITICAL", l == "CRIT", l == "PANIC":
		return true
	default:
		return false
	}
}

func isErrorText(ln string) bool {
	l := strings.ToLower(ln)
	for _, tok := range []string{"level=error", "level=fatal", "[error]", "[fatal]", " error ", " fatal "} {
		if strings.Contains(l, tok) {
			return true
		}
	}
	return false
}

// stripStreamPrefix drops a leading RFC3339 timestamp that Docker prepends when
// logs are requested with timestamps; the dashboard does not request them, but
// tolerate the shape in case a caller does.
func stripStreamPrefix(ln string) string {
	if i := strings.IndexByte(ln, ' '); i > 0 {
		if _, err := time.Parse(time.RFC3339Nano, ln[:i]); err == nil {
			return ln[i+1:]
		}
	}
	return ln
}
