package labelfilter

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/a1ekseev/scd/internal/common/schema"
)

const corpusPath = "../../../testdata/reference/common/label-filter.json"

func parse(t *testing.T, v map[string]any) (Filter, schema.Issues) {
	t.Helper()
	var is schema.Issues
	f := Parse(schema.Root(v, &is))
	return f, is
}

func mustParse(t *testing.T, v map[string]any) Filter {
	t.Helper()
	f, is := parse(t, v)
	if len(is) != 0 {
		t.Fatalf("issues: %#v", is)
	}
	return f
}

// TestLabelFilterRealShapeEquivalence checks that the Go include/exclude forms
// of the owner's production filters (D29) decide like the TS lookahead regexes
// recorded in testdata (task 0.6) on every label of the corpus.
func TestLabelFilterRealShapeEquivalence(t *testing.T) {
	data, err := os.ReadFile(corpusPath)
	if err != nil {
		t.Fatal(err)
	}
	var corpus []struct {
		Label          string `json:"label"`
		Source         string `json:"source"`
		En             bool   `json:"en"`
		Extra          bool   `json:"extra"`
		ExtraWhitelist bool   `json:"extraWhitelist"`
	}
	if err := json.Unmarshal(data, &corpus); err != nil {
		t.Fatal(err)
	}
	if len(corpus) == 0 {
		t.Fatal("empty corpus")
	}

	en := mustParse(t, map[string]any{"labelExcludeRegex": "/Extra|WhiteList/i"})
	extra := mustParse(t, map[string]any{"labelIncludeRegex": "/Extra/i", "labelExcludeRegex": "/WhiteList/i"})
	extraWhitelist := mustParse(t, map[string]any{"labelIncludeRegex": []any{"/Extra/i", "/WhiteList/i"}})

	// JS `.` does not match "\n", so the TS `.*` in lookaheads and `.*$` stops at
	// a line break; RE2 include/exclude look at the whole label. Go behaviour is
	// kept (12 §6.3); every label with a real "\n" must be listed here.
	const why = "JS `.` не матчит `\\n`"
	allowedDiffs := map[string]string{
		"Extra\nWhiteList": why + ": TS extraWhitelist=false, Go include [/Extra/i,/WhiteList/i] = true (12 §6.3)",
		"WhiteList\nExtra": why + ": TS extraWhitelist=false, Go include [/Extra/i,/WhiteList/i] = true",
		"Extra\nfoo":       why + ": TS extra=false (`.*$` не доходит до конца), Go include /Extra/i exclude /WhiteList/i = true",
		"foo\nbar":         why + ": TS en=false (`.*$` не доходит до конца), Go exclude /Extra|WhiteList/i = true",
	}

	sources := map[string]int{}
	for _, c := range corpus {
		sources[c.Source]++
		if strings.Contains(c.Label, "\n") {
			if _, ok := allowedDiffs[c.Label]; !ok {
				t.Errorf("label %q contains \\n but is not in allowedDiffs", c.Label)
			}
		}
		got := [3]bool{en.Match(c.Label), extra.Match(c.Label), extraWhitelist.Match(c.Label)}
		want := [3]bool{c.En, c.Extra, c.ExtraWhitelist}
		reason, allowed := allowedDiffs[c.Label]
		switch {
		case allowed && got == want:
			t.Errorf("label %q is in allowedDiffs (%s) but Go now matches TS %v", c.Label, reason, want)
		case !allowed && got != want:
			t.Errorf("label %q (%s): Go [en extra extraWhitelist] = %v, TS = %v", c.Label, c.Source, got, want)
		}
		delete(allowedDiffs, c.Label)
	}
	for label := range allowedDiffs {
		t.Errorf("allowedDiffs label %q is not in the corpus", label)
	}
	if sources["remnawave"] != 4 || sources["provider2"] != 50 || sources["edge"] == 0 {
		t.Errorf("corpus sources = %v", sources)
	}
}

func TestLabelFilterStringOrList(t *testing.T) {
	str := mustParse(t, map[string]any{"labelIncludeRegex": " /de/i ", "labelExcludeRegex": "X"})
	list := mustParse(t, map[string]any{"labelIncludeRegex": []any{"/de/i"}, "labelExcludeRegex": []any{"X"}})
	for name, f := range map[string]Filter{"string": str, "list": list} {
		t.Run(name, func(t *testing.T) {
			if len(f.Include) != 1 || len(f.Exclude) != 1 || f.IncludeSrc[0] != "/de/i" || f.ExcludeSrc[0] != "X" {
				t.Fatalf("filter = %#v", f)
			}
			if !f.Match("🇩🇪 DE-1") || f.Match("DE-1-X") || f.Match("NL-1") {
				t.Fatal("Match")
			}
		})
	}

	t.Run("include AND", func(t *testing.T) {
		f := mustParse(t, map[string]any{"labelIncludeRegex": []any{"a", "b"}})
		if !f.Match("xbxa") || f.Match("aaa") {
			t.Fatal("include must require all")
		}
	})
	t.Run("exclude OR", func(t *testing.T) {
		f := mustParse(t, map[string]any{"labelExcludeRegex": []any{"a", "b"}})
		if f.Match("b") || f.Match("a") || !f.Match("c") {
			t.Fatal("exclude must reject on any")
		}
	})
	t.Run("empty", func(t *testing.T) {
		f := mustParse(t, map[string]any{})
		if !f.Empty() || !f.Match("anything") {
			t.Fatalf("empty filter = %#v", f)
		}
		if str.Empty() {
			t.Fatal("non-empty reported Empty")
		}
		b, err := json.Marshal(f)
		if err != nil || string(b) != "{}" {
			t.Fatalf("json = %s %v", b, err)
		}
	})
	t.Run("json", func(t *testing.T) {
		b, err := json.Marshal(list)
		if err != nil {
			t.Fatal(err)
		}
		if want := `{"labelIncludeRegex":["/de/i"],"labelExcludeRegex":["X"]}`; string(b) != want {
			t.Fatalf("json = %s, want %s", b, want)
		}
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(map[string]Filter{"f": mustParse(t, map[string]any{"labelExcludeRegex": "<&>"})}); err != nil {
			t.Fatal(err)
		}
		if want := `{"f":{"labelExcludeRegex":["<&>"]}}` + "\n"; buf.String() != want {
			t.Fatalf("json = %q, want %q", buf.String(), want)
		}
	})
}

func TestLabelFilterIssuePaths(t *testing.T) {
	var is schema.Issues
	root := schema.Root(map[string]any{
		"outputs": []any{
			map[string]any{"labelIncludeRegex": "/a/g", "labelExcludeRegex": []any{"ok", " ", "(", 5}},
			map[string]any{"labelIncludeRegex": []any{}, "labelExcludeRegex": "  "},
		},
	}, &is)
	outs := root.Array("outputs", true, 1)
	f := Parse(outs[0])
	Parse(outs[1])
	if len(f.Exclude) != 1 || f.ExcludeSrc[0] != "ok" || len(f.Include) != 0 {
		t.Fatalf("filter = %#v", f)
	}
	want := []schema.Issue{
		{Path: "outputs.0.labelIncludeRegex", Message: `unsupported regex flag "g" (only i, m, s are allowed)`},
		{Path: "outputs.0.labelExcludeRegex.1", Message: "must not be empty"},
		{Path: "outputs.0.labelExcludeRegex.3", Message: "expected string, got int"},
		{Path: "outputs.0.labelExcludeRegex.2", Message: "error parsing regexp: missing closing ): `(`"},
		{Path: "outputs.1.labelIncludeRegex", Message: "must not be empty"},
		{Path: "outputs.1.labelExcludeRegex", Message: "must not be empty"},
	}
	if len(is) != len(want) {
		t.Fatalf("issues = %#v", is)
	}
	for i := range want {
		if is[i] != want[i] {
			t.Errorf("issue %d = %#v, want %#v", i, is[i], want[i])
		}
	}
}

func TestLabelFilterLookaheadRejected(t *testing.T) {
	for _, src := range []string{"/^(?!.*Extra)(?!.*WhiteList).*$/i", "/(?=.*Extra)(?=.*WhiteList)/i", `(a)\1`} {
		t.Run(src, func(t *testing.T) {
			f, is := parse(t, map[string]any{"labelIncludeRegex": []any{"ok", src}})
			if len(is) != 1 || is[0].Path != "labelIncludeRegex.1" {
				t.Fatalf("issues = %#v", is)
			}
			if !strings.HasPrefix(is[0].Message, "lookahead, lookbehind and backreferences are not supported (RE2 syntax): ") {
				t.Fatalf("message = %q", is[0].Message)
			}
			if strings.Contains(is[0].Message, "labelExcludeRegex") {
				t.Fatalf("no special rewrite hints (D31): %q", is[0].Message)
			}
			if len(f.Include) != 1 {
				t.Fatalf("filter = %#v", f)
			}
		})
	}
}
