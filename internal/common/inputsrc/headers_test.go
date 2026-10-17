package inputsrc

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/a1ekseev/scd/internal/common/jsonout"
)

func TestHeadersRedacted(t *testing.T) {
	h := Headers{"x-hwid": "s3cr3t"}
	b, err := h.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"x-hwid":"<redacted>"}` {
		t.Fatalf("json = %s", b)
	}
	// json.Marshal re-escapes "<" and ">" in Marshaler output; the value is
	// still redacted and decodes to "<redacted>".
	b, err = json.Marshal(h)
	if err != nil {
		t.Fatal(err)
	}
	var back map[string]string
	if err := json.Unmarshal(b, &back); err != nil || back["x-hwid"] != "<redacted>" || strings.Contains(string(b), "s3cr3t") {
		t.Fatalf("json.Marshal = %s (%v)", b, err)
	}

	jb, err := jsonout.Marshal(h)
	if err != nil || !strings.Contains(string(jb), `"<redacted>"`) || strings.Contains(string(jb), "s3cr3t") {
		t.Fatalf("jsonout = %s (%v)", jb, err)
	}

	var buf bytes.Buffer
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("m", "headers", h)
	out := buf.String()
	if strings.Contains(out, "s3cr3t") || !strings.Contains(out, "<redacted>") {
		t.Fatalf("log output = %s", out)
	}

	if h.Plain()["x-hwid"] != "s3cr3t" {
		t.Fatal("Plain must return real values")
	}
	p := h.Plain()
	p["x-hwid"] = "changed"
	if h["x-hwid"] != "s3cr3t" {
		t.Fatal("Plain must return a copy")
	}
}

func TestCheckHeaders(t *testing.T) {
	tests := []struct {
		name string
		in   map[string]string
		key  string
		msg  string // empty = no issues
	}{
		{"ok", map[string]string{"x-hwid": "abc"}, "", ""},
		{"bad name", map[string]string{"bad name": "v"}, "bad name", "invalid header name"},
		{"empty value", map[string]string{"X-A": "  "}, "X-A", "must not be empty"},
		{"newline", map[string]string{"X-A": "a\nb"}, "X-A", "must not contain control characters"},
		{"ctl", map[string]string{"X-A": "a\x01b"}, "X-A", "must not contain control characters"},
		{"nul", map[string]string{"X-A": "a\x00b"}, "X-A", "must not contain control characters"},
		{"user agent", map[string]string{"User-Agent": "x"}, "User-Agent", "use fetchUserAgent instead"},
		{"host", map[string]string{"host": "x"}, "host", "header is not allowed"},
		{"content-length", map[string]string{"Content-Length": "1"}, "Content-Length", "header is not allowed"},
		{"duplicate", map[string]string{"X-Hwid": "a", "x-hwid": "b"}, "x-hwid", "duplicate header (case-insensitive)"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := CheckHeaders(tc.in)
			if tc.msg == "" {
				if len(got) != 0 {
					t.Fatalf("issues = %v", got)
				}
				return
			}
			for _, is := range got {
				if is.Key == tc.key && strings.Contains(is.Message, tc.msg) {
					return
				}
			}
			t.Fatalf("issues = %v, want key %q msg %q", got, tc.key, tc.msg)
		})
	}
}
