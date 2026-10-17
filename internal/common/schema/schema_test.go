package schema

import (
	"maps"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

func parse(t *testing.T, src string) any {
	t.Helper()
	var v any
	if err := yaml.Unmarshal([]byte(src), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func wantIssues(t *testing.T, got Issues, want ...Issue) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("issues = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("issue %d = %#v, want %#v (all: %#v)", i, got[i], want[i], got)
		}
	}
}

func TestRequiredMissing(t *testing.T) {
	var is Issues
	r := Root(parse(t, "subscriptions:\n  - input: a\n  - id: b\n    input: c\nruntime:\n"), &is)
	subs := r.Array("subscriptions", true, 1)
	for _, s := range subs {
		s.String("id", StrOpt{Required: true})
	}
	r.Object("runtime", true)
	r.Object("resources", true)
	wantIssues(t, is,
		Issue{"subscriptions.0.id", "required"},
		Issue{"runtime", "required"},
		Issue{"resources", "required"},
	)
	if r.Path() != "" || subs[1].Path() != "subscriptions.1" {
		t.Fatalf("paths %q %q", r.Path(), subs[1].Path())
	}
}

func TestWrongType(t *testing.T) {
	var is Issues
	r := Root(parse(t, "s: 5\no: [1]\na: {x: 1}\nb: str\ni: x\nl: [a, 3]\nel: [1]\n"), &is)
	if _, ok := r.String("s", StrOpt{}); ok {
		t.Fatal("String ok for int")
	}
	if r.Object("o", false) != nil {
		t.Fatal("Object for array")
	}
	if r.Array("a", false, 0) != nil {
		t.Fatal("Array for object")
	}
	r.Bool("b", false)
	r.Int("i", IntOpt{})
	if got := r.StringList("l", StrOpt{}); !slices.Equal(got, []string{"a"}) {
		t.Fatalf("StringList = %#v", got)
	}
	el := r.Array("el", false, 0)
	el[0].String("x", StrOpt{Required: true}) // nil-safe
	wantIssues(t, is,
		Issue{"s", "expected string, got int"},
		Issue{"o", "expected object, got array"},
		Issue{"a", "expected array, got object"},
		Issue{"b", "expected boolean, got string"},
		Issue{"i", "expected integer, got string"},
		Issue{"l.1", "expected string, got int"},
		Issue{"el.0", "expected object, got int"},
	)

	t.Run("root", func(t *testing.T) {
		var is Issues
		if Root([]any{}, &is) != nil {
			t.Fatal("Root of array")
		}
		wantIssues(t, is, Issue{"", "expected object, got array"})
	})
	t.Run("null is absent", func(t *testing.T) {
		var is Issues
		r := Root(parse(t, "a:\nb: ~\n"), &is)
		if r.Has("a") || r.Has("b") || r.Has("c") {
			t.Fatal("Has for null/missing")
		}
		r.String("a", StrOpt{Required: true})
		wantIssues(t, is, Issue{"a", "required"})
	})
}

func TestTrimAndMin(t *testing.T) {
	var is Issues
	r := Root(parse(t, "a: '  x  '\nb: '   '\nc: ab\nd: '  '\ne: '  t '\n"), &is)
	if v, ok := r.String("a", StrOpt{Trim: true, MinLen: 1}); !ok || v != "x" {
		t.Fatalf("a = %q %v", v, ok)
	}
	if _, ok := r.String("b", StrOpt{Trim: true, MinLen: 1}); ok {
		t.Fatal("b ok")
	}
	if _, ok := r.String("c", StrOpt{MinLen: 3}); ok {
		t.Fatal("c ok")
	}
	if v, ok := r.String("d", StrOpt{Trim: true, EmptyToAbsent: true, MinLen: 1}); ok || v != "" {
		t.Fatalf("d = %q %v", v, ok)
	}
	if v, ok := r.String("e", StrOpt{}); !ok || v != "  t " {
		t.Fatalf("e = %q (no trim)", v)
	}
	if v, ok := r.String("missing", StrOpt{Default: new("def")}); !ok || v != "def" {
		t.Fatalf("default = %q %v", v, ok)
	}
	if v, ok := r.String("missing", StrOpt{}); ok || v != "" {
		t.Fatalf("optional missing = %q %v", v, ok)
	}
	wantIssues(t, is,
		Issue{"b", "must not be empty"},
		Issue{"c", "must be at least 3 characters long"},
	)
}

func TestStringList(t *testing.T) {
	var is Issues
	r := Root(parse(t, "a: [' x ', y]\nb: []\nc: [ok, ' ']\nd: str\n"), &is)
	if got := r.StringList("a", StrOpt{Trim: true, MinLen: 1}); len(got) != 2 || got[0] != "x" || got[1] != "y" {
		t.Fatalf("a = %#v", got)
	}
	r.StringList("b", StrOpt{})
	r.StringList("c", StrOpt{Trim: true, MinLen: 1})
	r.StringList("d", StrOpt{})
	if got := r.StringList("missing", StrOpt{}); got != nil {
		t.Fatalf("missing = %#v", got)
	}
	r.StringList("missing", StrOpt{Required: true})
	wantIssues(t, is,
		Issue{"b", "must not be empty"},
		Issue{"c.1", "must not be empty"},
		Issue{"d", "expected array, got string"},
		Issue{"missing", "required"},
	)
}

func TestStringOrList(t *testing.T) {
	var is Issues
	r := Root(parse(t, "s: ' x '\nl: [a, ' ', ' b ']\ne: []\nn: 5\n"), &is)
	if got := r.StringOrList("s", StrOpt{Trim: true, MinLen: 1}); !slices.Equal(got, []StringElem{{"s", "x"}}) {
		t.Fatalf("s = %#v", got)
	}
	if got := r.StringOrList("l", StrOpt{Trim: true, MinLen: 1}); !slices.Equal(got, []StringElem{{"l.0", "a"}, {"l.2", "b"}}) {
		t.Fatalf("l = %#v", got)
	}
	r.StringOrList("e", StrOpt{})
	r.StringOrList("n", StrOpt{})
	if got := r.StringOrList("missing", StrOpt{}); got != nil {
		t.Fatalf("missing = %#v", got)
	}
	r.StringOrList("missing", StrOpt{Required: true})
	wantIssues(t, is,
		Issue{"l.1", "must not be empty"},
		Issue{"e", "must not be empty"},
		Issue{"n", "expected string or array, got int"},
		Issue{"missing", "required"},
	)
}

func TestIntBounds(t *testing.T) {
	var is Issues
	r := Root(parse(t, "a: 0\nb: 25\nc: 12\n"), &is)
	opt := IntOpt{Min: new(1), Max: new(24)}
	r.Int("a", opt)
	r.Int("b", opt)
	if got := r.Int("c", opt); got != 12 {
		t.Fatalf("c = %d", got)
	}
	if got := r.Int("d", IntOpt{Default: new(5000)}); got != 5000 {
		t.Fatalf("default = %d", got)
	}
	if got := r.Int("d", IntOpt{}); got != 0 {
		t.Fatalf("optional missing = %d", got)
	}
	r.Int("d", IntOpt{Required: true})
	wantIssues(t, is,
		Issue{"a", "must be at least 1"},
		Issue{"b", "must be at most 24"},
		Issue{"d", "required"},
	)
}

func TestFloatRejected(t *testing.T) {
	var is Issues
	r := Root(parse(t, "fetchTimeoutMs: 5000.0\nbig: 18446744073709551615\n"), &is)
	r.Int("fetchTimeoutMs", IntOpt{})
	r.Int("big", IntOpt{})
	wantIssues(t, is,
		Issue{"fetchTimeoutMs", "expected integer, got float"},
		Issue{"big", "integer out of range"},
	)
	if !strings.Contains(is[0].Message, "expected integer") {
		t.Fatal(is)
	}
}

func TestBoolRejectsYesString(t *testing.T) {
	var is Issues
	r := Root(parse(t, "a: yes\nb: false\n"), &is)
	if r.Bool("a", true) != true {
		t.Fatal("invalid bool must return default")
	}
	if r.Bool("b", true) != false {
		t.Fatal("b")
	}
	if r.Bool("c", true) != true {
		t.Fatal("default")
	}
	wantIssues(t, is, Issue{"a", "expected boolean, got string"})
}

func TestEnumDefault(t *testing.T) {
	var is Issues
	r := Root(parse(t, "format: b64\nlevel: debug\nn: 1\n"), &is)
	allowed := []string{"auto", "plain", "base64"}
	r.Enum("format", allowed, "auto")
	if got := r.Enum("missing", allowed, "auto"); got != "auto" {
		t.Fatalf("default = %q", got)
	}
	if got := r.Enum("level", []string{"info", "debug"}, "info"); got != "debug" {
		t.Fatalf("level = %q", got)
	}
	r.Enum("n", allowed, "auto")
	r.Enum("req", allowed, "")
	wantIssues(t, is,
		Issue{"format", "must be one of auto|plain|base64"},
		Issue{"n", "expected string, got int"},
		Issue{"req", "required"},
	)
}

func TestURL(t *testing.T) {
	var is Issues
	r := Root(parse(t, "a: https://h:1/p\nb: /rel\nc: 'http://'\nd: '::'\n"), &is)
	if v, ok := r.URL("a", true); !ok || v != "https://h:1/p" {
		t.Fatalf("a = %q %v", v, ok)
	}
	r.URL("b", true)
	r.URL("c", false)
	r.URL("d", false)
	r.URL("e", true)
	if _, ok := r.URL("f", false); ok {
		t.Fatal("optional missing ok")
	}
	wantIssues(t, is,
		Issue{"b", "invalid URL"},
		Issue{"c", "invalid URL"},
		Issue{"d", "invalid URL"},
		Issue{"e", "required"},
	)
}

func TestStrictUnknownKey(t *testing.T) {
	var is Issues
	r := Root(parse(t, "a: 1\nzz: 1\nyy: 2\nsub: {k: 1, x: 2}\n"), &is)
	r.Object("sub", false).Strict("k")
	r.Strict("a", "sub")
	var nilNode *Node
	nilNode.Strict("a")
	wantIssues(t, is,
		Issue{"sub", `unknown key "x"`},
		Issue{"", `unknown key "yy"`},
		Issue{"", `unknown key "zz"`},
	)
}

func TestAddf(t *testing.T) {
	var is Issues
	r := Root(map[string]any{"o": map[string]any{}}, &is)
	r.Object("o", true).Addf("k.0", "bad %d", 1)
	r.Addf("", "root %s", "issue")
	wantIssues(t, is, Issue{"o.k.0", "bad 1"}, Issue{"", "root issue"})
}

func TestIssuesErrFormat(t *testing.T) {
	if (Issues{}).Err("/c.yml") != nil {
		t.Fatal("empty issues must give nil")
	}
	is := Issues{{"subscriptions.0.id", "required"}, {"", `unknown key "x"`}}
	want := "invalid config /etc/c.yml:\n  - subscriptions.0.id: required\n  - <root>: unknown key \"x\""
	if got := is.Err("/etc/c.yml").Error(); got != want {
		t.Fatalf("Err =\n%s\nwant\n%s", got, want)
	}
}

func TestStringMap(t *testing.T) {
	var is Issues
	r := Root(parse(t, "ok:\n  fetchHeaders: {x-hwid: abc, X-Other: ' v '}\nbad:\n  fetchHeaders: {x-n: 5}\nlist:\n  fetchHeaders: [a]\n"), &is)
	got := r.Object("ok", true).StringMap("fetchHeaders")
	if want := map[string]string{"x-hwid": "abc", "X-Other": " v "}; !maps.Equal(got, want) {
		t.Fatalf("ok = %#v", got)
	}
	r.Object("bad", true).StringMap("fetchHeaders")
	if got := r.Object("list", true).StringMap("fetchHeaders"); got != nil {
		t.Fatalf("list = %#v", got)
	}
	if got := r.StringMap("missing"); got != nil {
		t.Fatalf("missing = %#v", got)
	}
	wantIssues(t, is,
		Issue{"bad.fetchHeaders.x-n", "expected string, got int"},
		Issue{"list.fetchHeaders", "expected object, got array"},
	)
}

func TestNilNodeIsSafe(t *testing.T) {
	var n *Node
	if n.Object("a", true) != nil || n.Array("a", true, 1) != nil || n.StringList("a", StrOpt{Required: true}) != nil ||
		n.StringMap("a") != nil || n.Has("a") || n.Path() != "" || n.Int("a", IntOpt{Default: new(3)}) != 3 ||
		!n.Bool("a", true) || n.Enum("a", []string{"x"}, "x") != "x" {
		t.Fatal("nil node")
	}
	if v, ok := n.String("a", StrOpt{Default: new("d")}); !ok || v != "d" {
		t.Fatal("nil String default")
	}
	if _, ok := n.URL("a", true); ok {
		t.Fatal("nil URL")
	}
	n.Addf("a", "x")
}

func TestStringListAllowEmpty(t *testing.T) {
	var is Issues
	r := Root(parse(t, "a: []\nb: [x, '']\n"), &is)
	if got := r.StringList("a", StrOpt{AllowEmptyList: true}); got == nil || len(got) != 0 {
		t.Fatalf("a = %#v, want empty non-nil", got)
	}
	if got := r.StringList("b", StrOpt{AllowEmptyList: true, MinLen: 1}); len(got) != 1 || got[0] != "x" {
		t.Fatalf("b = %#v", got)
	}
	wantIssues(t, is, Issue{"b.1", "must not be empty"})
}

func TestStringListElems(t *testing.T) {
	var is Issues
	r := Root(parse(t, "a: [x, 1, ' y ']\n"), &is)
	got := r.StringListElems("a", StrOpt{Trim: true})
	want := []StringElem{{"a.0", "x"}, {"a.2", "y"}}
	if !slices.Equal(got, want) {
		t.Fatalf("got %#v", got)
	}
	wantIssues(t, is, Issue{"a.1", "expected string, got int"})
}
