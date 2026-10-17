package logging

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestNewLevels(t *testing.T) {
	cases := []struct {
		name, level, msgLevel string
		want                  bool
	}{
		{"trace at trace", "trace", "trace", true},
		{"trace at debug", "debug", "trace", false},
		{"debug at debug", "debug", "debug", true},
		{"info at warn", "warn", "info", false},
		{"error at error", "error", "error", true},
		{"error at fatal", "fatal", "error", false},
		{"fatal at fatal", "fatal", "fatal", true},
		{"fatal at silent", "silent", "fatal", false},
		{"error at silent", "silent", "error", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			l, err := New(&buf, tc.level, "json")
			if err != nil {
				t.Fatal(err)
			}
			switch tc.msgLevel {
			case "trace":
				l.Log(t.Context(), LevelTrace, "m")
			case "debug":
				l.Debug("m")
			case "info":
				l.Info("m")
			case "error":
				l.Error("m")
			case "fatal":
				l.Log(t.Context(), LevelFatal, "m")
			}
			if got := buf.Len() > 0; got != tc.want {
				t.Fatalf("written=%v want %v (%q)", got, tc.want, buf.String())
			}
		})
	}
}

func TestNewLevelNames(t *testing.T) {
	var buf bytes.Buffer
	l, err := New(&buf, "trace", "json")
	if err != nil {
		t.Fatal(err)
	}
	l.Log(t.Context(), LevelTrace, "a")
	l.Log(t.Context(), LevelFatal, "b")
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	for i, want := range []string{"TRACE", "FATAL"} {
		var m map[string]any
		if err := json.Unmarshal([]byte(lines[i]), &m); err != nil {
			t.Fatal(err)
		}
		if m["level"] != want {
			t.Errorf("line %d level=%v want %s", i, m["level"], want)
		}
	}
}

func TestNewJSONHasEventAttr(t *testing.T) {
	var buf bytes.Buffer
	l, err := New(&buf, "info", "json")
	if err != nil {
		t.Fatal(err)
	}
	l.Info("Hello.", "event", "x", "n", 3)
	var m map[string]any
	if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	if m["event"] != "x" || m["msg"] != "Hello." || m["level"] != "INFO" || m["n"] != float64(3) {
		t.Fatalf("unexpected record: %v", m)
	}
	if _, ok := m["time"].(string); !ok {
		t.Fatalf("time missing: %v", m)
	}
}

func TestNewPrettyIsText(t *testing.T) {
	var buf bytes.Buffer
	l, err := New(&buf, "info", "pretty")
	if err != nil {
		t.Fatal(err)
	}
	l.Info("Hello.", "event", "x")
	s := buf.String()
	if strings.HasPrefix(s, "{") || !strings.Contains(s, "level=INFO") || !strings.Contains(s, "event=x") {
		t.Fatalf("unexpected pretty output: %q", s)
	}
}

func TestNewRejectsUnknownLevel(t *testing.T) {
	_, err := New(&bytes.Buffer{}, "loud", "json")
	if err == nil || !strings.Contains(err.Error(), `unsupported log level "loud"`) {
		t.Fatalf("err = %v", err)
	}
}

func TestNewRejectsUnknownFormat(t *testing.T) {
	_, err := New(&bytes.Buffer{}, "info", "text")
	want := `unsupported log format "text" (expected json or pretty)`
	if err == nil || err.Error() != want {
		t.Fatalf("err = %v, want %s", err, want)
	}
}

type prettyItem struct {
	ID      string `json:"id"`
	Message string `json:"message,omitempty"`
}

type prettyStringer struct{ A int }

func (prettyStringer) String() string { return "stringer-text" }

func TestPrettyRendersCompositeValuesAsJSON(t *testing.T) {
	var buf bytes.Buffer
	l, err := New(&buf, "info", "pretty")
	if err != nil {
		t.Fatal(err)
	}
	l.Info("Hello.",
		"event", "x",
		"subscriptions", []prettyItem{{ID: "a", Message: "m"}, {ID: "b"}},
		"causes", []string{"first", "second cause"},
		"counts", map[string]int{"b": 2, "a": 1},
		"item", prettyItem{ID: "c"},
		"err", errors.New("plain error"),
		"str", prettyStringer{A: 1},
		"at", time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC),
		"took", 1500*time.Millisecond,
		"raw", []byte("hi"),
		"html", []string{"<redacted>", "a&b"},
	)
	s := buf.String()
	for _, want := range []string{
		`subscriptions="[{\"id\":\"a\",\"message\":\"m\"},{\"id\":\"b\"}]"`,
		`causes="[\"first\",\"second cause\"]"`,
		`counts="{\"a\":1,\"b\":2}"`,
		`item="{\"id\":\"c\"}"`,
		`err="plain error"`,
		`str=stringer-text`,
		`at=2026-10-17T12:00:00.000Z`,
		`took=1.5s`,
		`raw="hi"`,
		`html="[\"<redacted>\",\"a&b\"]"`, // final review M5: no HTML escaping
	} {
		if !strings.Contains(s, want) {
			t.Errorf("pretty output lacks %s:\n%s", want, s)
		}
	}
	if strings.Contains(s, "ID:") || strings.Contains(s, "Message:") {
		t.Errorf("pretty output has Go field names:\n%s", s)
	}
}

func TestJSONKeepsCompositeValuesNested(t *testing.T) {
	var buf bytes.Buffer
	l, err := New(&buf, "info", "json")
	if err != nil {
		t.Fatal(err)
	}
	l.Info("Hello.", "subscriptions", []prettyItem{{ID: "a"}}, "causes", []string{"x"})
	var m map[string]any
	if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	subs, ok := m["subscriptions"].([]any)
	if !ok || len(subs) != 1 || subs[0].(map[string]any)["id"] != "a" {
		t.Errorf("subscriptions = %#v, want nested array", m["subscriptions"])
	}
	if causes, ok := m["causes"].([]any); !ok || len(causes) != 1 || causes[0] != "x" {
		t.Errorf("causes = %#v, want nested array", m["causes"])
	}
}
