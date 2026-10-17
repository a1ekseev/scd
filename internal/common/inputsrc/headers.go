package inputsrc

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"maps"
	"slices"
	"strings"

	"golang.org/x/net/http/httpguts"
)

const redacted = "<redacted>"

// Headers are extra subscription request headers (D33 fetchHeaders).
// Values are secrets: JSON and log output show them as "<redacted>".
type Headers map[string]string

// MarshalJSON writes the keys with every value replaced by "<redacted>".
func (h Headers) MarshalJSON() ([]byte, error) {
	m := make(map[string]string, len(h))
	for _, k := range slices.Sorted(maps.Keys(h)) {
		m[k] = redacted
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false) // keep "<redacted>" readable; encoding/json sorts map keys
	if err := enc.Encode(m); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// LogValue implements slog.LogValuer with redacted values.
func (h Headers) LogValue() slog.Value {
	attrs := make([]slog.Attr, 0, len(h))
	for _, k := range slices.Sorted(maps.Keys(h)) {
		attrs = append(attrs, slog.String(k, redacted))
	}
	return slog.GroupValue(attrs...)
}

// Plain returns a copy with the real values. Use it only for the HTTP request
// and for runtime state with includeSecrets enabled.
func (h Headers) Plain() map[string]string {
	return maps.Clone(map[string]string(h))
}

// HeaderIssue describes one invalid fetchHeaders entry.
type HeaderIssue struct {
	Key, Message string
}

// CheckHeaders validates fetchHeaders and returns issues ordered by key.
func CheckHeaders(h map[string]string) []HeaderIssue {
	var issues []HeaderIssue
	seen := make(map[string]bool, len(h))
	for _, k := range slices.Sorted(maps.Keys(h)) {
		add := func(msg string) { issues = append(issues, HeaderIssue{Key: k, Message: msg}) }
		lower := strings.ToLower(k)
		if seen[lower] {
			add("duplicate header (case-insensitive)")
		}
		seen[lower] = true

		if !httpguts.ValidHeaderFieldName(k) {
			add("invalid header name")
			continue
		}
		switch lower {
		case "user-agent":
			add("use fetchUserAgent instead")
		case "host", "content-length", "transfer-encoding", "connection":
			add("header is not allowed")
		}
		v := h[k]
		switch {
		case strings.TrimSpace(v) == "":
			add("must not be empty")
		case strings.ContainsAny(v, "\r\n\x00"):
			add("must not contain control characters")
		}
	}
	return issues
}
