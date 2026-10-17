package cron

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

const (
	nextPath     = "../../../testdata/reference/common/cron-next.json"
	rejectedPath = "../../../testdata/reference/common/cron-rejected.json"
)

func TestMain(m *testing.M) {
	// The reference values were captured with TZ=UTC.
	time.Local = time.UTC
	os.Exit(m.Run())
}

func loadJSON(t *testing.T, path string, v any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, v); err != nil {
		t.Fatal(err)
	}
}

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func nextN(s Schedule, from time.Time, n int) []time.Time {
	out := make([]time.Time, 0, n)
	at := from
	for range n {
		at = s.Next(at)
		out = append(out, at)
	}
	return out
}

func mustParse(t *testing.T, expr string) Schedule {
	t.Helper()
	s, err := Parse(expr)
	if err != nil {
		t.Fatalf("Parse(%q): %v", expr, err)
	}
	return s
}

func TestParseMatchesReferenceNext(t *testing.T) {
	var cases []struct {
		Expr string   `json:"expr"`
		From string   `json:"from"`
		Next []string `json:"next"`
	}
	loadJSON(t, nextPath, &cases)
	if len(cases) == 0 {
		t.Fatal("empty reference")
	}
	for _, c := range cases {
		t.Run(c.Expr, func(t *testing.T) {
			s := mustParse(t, c.Expr)
			got := nextN(s, mustTime(t, c.From), len(c.Next))
			for i, want := range c.Next {
				if w := mustTime(t, want); !got[i].Equal(w) {
					t.Errorf("next[%d] = %s, want %s", i, got[i].UTC().Format(time.RFC3339), w.Format(time.RFC3339))
				}
			}
		})
	}
}

func TestParseRejects(t *testing.T) {
	var cases []struct {
		Expr string `json:"expr"`
	}
	loadJSON(t, rejectedPath, &cases)
	if len(cases) == 0 {
		t.Fatal("empty reference")
	}
	for _, c := range cases {
		t.Run(c.Expr, func(t *testing.T) {
			if _, err := Parse(c.Expr); err == nil {
				t.Fatalf("Parse(%q) accepted", c.Expr)
			}
		})
	}
	// Further cron-parser forms listed in docs/CONFIG-MIGRATION.md as rejected.
	for _, expr := range []string{"@secondly", "@weekends", "0 0 * * 5L"} {
		if _, err := Parse(expr); err == nil {
			t.Errorf("Parse(%q) accepted", expr)
		}
	}
	for _, expr := range []string{"0 0 31 2 *", "0 0 30 2 *"} {
		_, err := Parse(expr)
		if err == nil || err.Error() != "cron expression never matches" {
			t.Errorf("Parse(%q) error = %v, want never matches", expr, err)
		}
	}
	if _, err := Parse(""); err == nil || err.Error() != "cron expression must not be empty" {
		t.Errorf("Parse(\"\") error = %v", err)
	}
	// Library errors are passed through with the library's text.
	if _, err := Parse("* * * * 7"); err == nil || !strings.Contains(err.Error(), "above maximum") {
		t.Errorf("Parse(dow 7) error = %v, want library text", err)
	}
}

func TestParseAcceptsLibrarySyntax(t *testing.T) {
	for _, expr := range []string{"0 0 ? * *", "@every 5m", "CRON_TZ=UTC 0 0 * * *"} {
		mustParse(t, expr)
	}
}

func TestParseLinuxMacros(t *testing.T) {
	for _, expr := range []string{"@yearly", "@annually", "@monthly", "@weekly", "@daily", "@midnight", "@hourly"} {
		mustParse(t, expr)
	}
	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, pair := range [][2]string{{"@midnight", "@daily"}, {"@annually", "@yearly"}} {
		a := nextN(mustParse(t, pair[0]), from, 3)
		b := nextN(mustParse(t, pair[1]), from, 3)
		for i := range a {
			if !a[i].Equal(b[i]) {
				t.Errorf("%s next[%d] = %s, %s = %s", pair[0], i, a[i], pair[1], b[i])
			}
		}
	}
}

func TestParseClassicForms(t *testing.T) {
	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, expr := range []string{"1-30/5 * * * *", "5 4 * jan,jul sun", "0 9 * * mon-fri"} {
		mustParse(t, expr)
	}
	cases := []struct{ expr, want string }{
		{"0 0 29 2 *", "2028-02-29T00:00:00Z"},
		{"*/15 * * * * *", "2026-01-01T00:00:15Z"},
	}
	for _, c := range cases {
		got := mustParse(t, c.expr).Next(from)
		if w := mustTime(t, c.want); !got.Equal(w) {
			t.Errorf("%s next = %s, want %s", c.expr, got, w)
		}
	}
}
