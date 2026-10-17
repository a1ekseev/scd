package vless

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/a1ekseev/scd/internal/common/inputsrc"
	"github.com/a1ekseev/scd/internal/common/subscription"
)

const (
	corpusDir     = "../../../testdata/reference/scd/corpus/"
	realShapesDir = "../../../docker/fixtures/real-shapes/"
	uuid          = "00000000-0000-4000-8000-000000000001"
)

// line builds a subscription line the way subscription.ScanLines does.
func line(n int, raw string) subscription.Line {
	return subscription.Line{Number: n, Raw: raw, Trimmed: strings.TrimSpace(raw)}
}

func p(k, v string) Param { return Param{Key: k, Value: v} }

func TestParseOK(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		address string
		port    int
		id      string
		label   string
		params  []Param
		ignored []string
	}{
		{
			name: "uppercase scheme and keys, leading-zero port", raw: "VLESS://" + uuid + "@Example.COM:0443?Type=TCP&Security=TLS#x",
			address: "Example.COM", port: 443, id: uuid, label: "x", params: []Param{p("type", "TCP"), p("security", "TLS")},
		},
		{
			name: "label keeps later #", raw: "vless://" + uuid + "@h:443#🇩🇪 Берлин#2",
			address: "h", port: 443, id: uuid, label: "🇩🇪 Берлин#2",
		},
		{
			name: "label percent-decoded", raw: "vless://" + uuid + "@h:443#Hello%20World",
			address: "h", port: 443, id: uuid, label: "Hello World",
		},
		{
			name: "no fragment", raw: "vless://" + uuid + "@h:443",
			address: "h", port: 443, id: uuid, label: "",
		},
		{
			name: "duplicate key: last value, first position", raw: "vless://" + uuid + "@h:443?sni=a&type=tcp&sni=b",
			address: "h", port: 443, id: uuid, params: []Param{p("sni", "b"), p("type", "tcp")},
		},
		{
			name: "query decoding", raw: "vless://" + uuid + "@h:443?path=/a+b%2Fc",
			address: "h", port: 443, id: uuid, params: []Param{p("path", "/a b/c")},
		},
		{
			name: "semicolon is not a separator", raw: "vless://" + uuid + "@h:443?path=/a;x",
			address: "h", port: 443, id: uuid, params: []Param{p("path", "/a;x")},
		},
		{
			name: "invalid escape kept raw, empty segments skipped", raw: "vless://" + uuid + "@h:443?&path=/a%zz&&type=ws&",
			address: "h", port: 443, id: uuid, params: []Param{p("path", "/a%zz"), p("type", "ws")},
		},
		{
			name: "IPv6 literal without brackets", raw: "vless://" + uuid + "@[2001:db8::1]:443",
			address: "2001:db8::1", port: 443, id: uuid,
		},
		{
			name: "non-UUID id (D18)", raw: "vless://my-user@h:443",
			address: "h", port: 443, id: "my-user",
		},
		{
			name: "uppercase UUID", raw: "vless://00000000-0000-4000-8000-00000000ABCD@h:443",
			address: "h", port: 443, id: "00000000-0000-4000-8000-00000000ABCD",
		},
		{
			name: "unknown params ignored (D28)", raw: "vless://" + uuid + "@h:443?foo=bar&ech=x&zz=1&FOO=2",
			address: "h", port: 443, id: uuid, params: []Param{p("ech", "x")}, ignored: []string{"foo", "zz"},
		},
		{
			name: "cs and heartbeatPeriod are known (D33)", raw: "vless://" + uuid + "@h:443?cs=TLS_AES_128_GCM_SHA256&heartbeatPeriod=30",
			address: "h", port: 443, id: uuid, params: []Param{p("cs", "TLS_AES_128_GCM_SHA256"), p("heartbeatperiod", "30")},
		},
		{
			name: "invalid UTF-8 label sanitized", raw: "vless://" + uuid + "@h:443#a%FFb",
			address: "h", port: 443, id: uuid, label: "a\uFFFDb",
		},
		{
			name: "password dropped, URL path ignored", raw: "vless://u:pw@h:443/p?type=ws#l",
			address: "h", port: 443, id: "u", label: "l", params: []Param{p("type", "ws")},
		},
		{
			name: "surrounding white space trimmed", raw: "  vless://" + uuid + "@h:443#ws  ",
			address: "h", port: 443, id: uuid, label: "ws",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			l, skip := Parse(line(7, c.raw))
			if skip != nil {
				t.Fatalf("unexpected skip %+v", *skip)
			}
			if l.Line != 7 || l.Raw != c.raw {
				t.Errorf("Line/Raw = %d/%q", l.Line, l.Raw)
			}
			if l.Address != c.address || l.Port != c.port || l.ID != c.id || l.Label != c.label {
				t.Errorf("got address=%q port=%d id=%q label=%q", l.Address, l.Port, l.ID, l.Label)
			}
			if !reflect.DeepEqual(l.Params, c.params) {
				t.Errorf("Params = %v, want %v", l.Params, c.params)
			}
			if !reflect.DeepEqual(l.Ignored, c.ignored) {
				t.Errorf("Ignored = %q, want %q", l.Ignored, c.ignored)
			}
		})
	}
}

func TestParseSkip(t *testing.T) {
	const (
		noScheme  = "The line does not contain a supported URI scheme."
		badURI    = "The line is not a valid URI."
		badLabel  = "The line label is not valid percent-encoding."
		noID      = "The VLESS URI does not contain a user id."
		noAddress = "The VLESS URI does not contain a valid address and port."
	)
	cases := []struct {
		name   string
		raw    string
		code   string
		reason string
		label  *string
	}{
		{"other scheme", "trojan://x", CodeUnsupportedScheme, `Unsupported scheme "trojan:".`, nil},
		{"other scheme uppercase", "HTTPS://x/#l", CodeUnsupportedScheme, `Unsupported scheme "https:".`, nil},
		{"no //", "vless:" + uuid + "@h:443", CodeUnsupportedScheme, noScheme, nil},
		{"plain text", "just text", CodeUnsupportedScheme, noScheme, nil},
		{"space in host", "vless://u@ho st:443", CodeInvalidURI, badURI, nil},
		{"not a URL", "foo bar ://", CodeInvalidURI, badURI, nil},
		{"bare % in label (O11)", "vless://" + uuid + "@h:443#100%", CodeInvalidURI, badLabel, nil},
		{"extra colon in host:port", "vless://u@h:443:80", CodeInvalidURI, badURI, nil},
		{"unbracketed IPv6", "vless://u@2001:db8::1:443", CodeInvalidURI, badURI, nil},
		{"IPv6 zone id", "vless://u@[fe80::1%25eth0]:443", CodeInvalidURI, badURI, nil},
		{"empty id", "vless://@h:443#l", CodeMissingRequiredField, noID, new("l")},
		{"no userinfo", "vless://h:443", CodeMissingRequiredField, noID, new("")},
		{"no port", "vless://u@h", CodeMissingRequiredField, noAddress, new("")},
		{"empty port", "vless://u@h:#x", CodeMissingRequiredField, noAddress, new("x")},
		{"port 0", "vless://u@h:0", CodeMissingRequiredField, noAddress, new("")},
		{"port 65536", "vless://u@h:65536", CodeMissingRequiredField, noAddress, new("")},
		{"huge port", "vless://u@h:99999999999999999999999", CodeMissingRequiredField, noAddress, new("")},
		{"empty host", "vless://u@:443", CodeMissingRequiredField, noAddress, new("")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, skip := Parse(line(3, c.raw))
			if skip == nil {
				t.Fatal("expected skip")
			}
			want := Skip{Line: 3, Raw: c.raw, Label: c.label, Code: c.code, Reason: c.reason}
			if !reflect.DeepEqual(*skip, want) {
				t.Errorf("skip = %+v, want %+v", *skip, want)
			}
		})
	}
}

func TestSkipJSON(t *testing.T) {
	b, err := json.Marshal(Skip{Line: 1, Raw: "r", Code: CodeInvalidURI, Reason: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(b), `{"line":1,"raw":"r","reasonCode":"invalid_uri","reason":"x"}`; got != want {
		t.Errorf("got %s, want %s", got, want)
	}
	b, err = json.Marshal(Skip{Line: 2, Raw: "r", Label: new("l"), Code: CodeUnsupportedValue, Reason: "y", Details: []string{"d"}})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(b), `{"line":2,"raw":"r","label":"l","reasonCode":"unsupported_value","reason":"y","details":["d"]}`; got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

func TestLinkGet(t *testing.T) {
	l, skip := Parse(line(1, "vless://"+uuid+"@h:443?serviceName=svc&path="))
	if skip != nil {
		t.Fatal(*skip)
	}
	for _, key := range []string{"servicename", "serviceName"} {
		if v, ok := l.Get(key); !ok || v != "svc" {
			t.Errorf("Get(%q) = %q, %v", key, v, ok)
		}
	}
	if v, ok := l.Get("path"); !ok || v != "" {
		t.Errorf("Get(path) = %q, %v", v, ok)
	}
	if _, ok := l.Get("sni"); ok {
		t.Error("Get(sni) found a missing key")
	}
}

func TestAllowedParamsAccepted(t *testing.T) {
	variants := []func(string) string{strings.ToLower, strings.ToUpper, mixedCase}
	for _, key := range AllowedParams {
		for _, v := range variants {
			k := v(key)
			t.Run(k, func(t *testing.T) {
				l, skip := Parse(line(1, "vless://"+uuid+"@h:443?"+k+"=1#x"))
				if skip != nil {
					t.Fatalf("unexpected skip %+v", *skip)
				}
				if len(l.Ignored) != 0 {
					t.Errorf("Ignored = %q", l.Ignored)
				}
				if got, ok := l.Get(key); !ok || got != "1" {
					t.Errorf("Get(%q) = %q, %v", key, got, ok)
				}
			})
		}
	}
	for _, key := range []string{"serviceName", "support-x25519mlkem768", "x_padding_bytes", "fm", "pqv", "ech", "cs", "heartbeatperiod"} {
		if !slices.Contains(AllowedParams, strings.ToLower(key)) {
			t.Errorf("AllowedParams lacks %q", key)
		}
	}
}

// mixedCase upper-cases every other letter: "servicename" → "SeRvIcEnAmE".
func mixedCase(s string) string {
	b := []byte(s)
	for i := 0; i < len(b); i += 2 {
		if b[i] >= 'a' && b[i] <= 'z' {
			b[i] -= 'a' - 'A'
		}
	}
	return string(b)
}

// outcome is a comparable summary of a parse result.
type outcome struct {
	ok    bool
	code  string // when !ok
	label *string
}

func (o outcome) String() string {
	s := "ok"
	if !o.ok {
		s = "skip " + o.code
	}
	if o.label != nil {
		s += fmt.Sprintf(" label=%q", *o.label)
	}
	return s
}

// matches reports whether got matches want; a nil label in want matches any label.
func (o outcome) matches(got outcome) bool {
	if o.ok != got.ok || (!o.ok && o.code != got.code) {
		return false
	}
	return o.label == nil || (got.label != nil && *got.label == *o.label)
}

func goOutcome(l subscription.Line) outcome {
	link, skip := Parse(l)
	if skip != nil {
		return outcome{code: skip.Code, label: skip.Label}
	}
	return outcome{ok: true, label: &link.Label}
}

// tsStage tells where a TS skip was produced: "parse" (the URI itself) or
// "normalize" (after Parse). An unknown reason returns "".
func tsStage(code, reason string) string {
	switch code {
	case CodeUnsupportedScheme, CodeInvalidURI:
		return "parse"
	case "unsupported_param", CodeUnsupportedValue, CodeUnsupportedCombo:
		return "normalize"
	case CodeMissingRequiredField:
		switch reason {
		case "The VLESS URI does not contain a valid address and port.",
			"The VLESS URI does not contain a valid UUID.":
			return "parse"
		case `REALITY VLESS requires parameter "sid".`,
			"WebSocket VLESS requires a path.":
			return "normalize"
		}
	}
	return ""
}

func TestParseCorpusAgainstTS(t *testing.T) {
	data, err := os.ReadFile(corpusDir + "ts-parse.json")
	if err != nil {
		t.Fatal(err)
	}
	var corpus []struct {
		Line int    `json:"line"`
		Raw  string `json:"raw"`
		TS   struct {
			OK         *bool    `json:"ok"`
			ReasonCode string   `json:"reasonCode"`
			Reason     string   `json:"reason"`
			Label      *string  `json:"label"`
			Details    []string `json:"details"`
			Error      string   `json:"error"`
		} `json:"ts"`
	}
	if err := json.Unmarshal(data, &corpus); err != nil {
		t.Fatal(err)
	}
	// Go results that differ from TS on purpose.
	allowedDiffs := map[int]struct {
		got    string
		reason string
	}{
		22: {`skip missing_required_field label="x"`, "D9: net/url accepts any digit port; the 1..65535 range check gives the address-and-port reason"},
		23: {`skip missing_required_field label="x"`, "D9: net/url accepts an empty host; Parse reports the missing address"},
		30: {`ok label="x"`, "D18: the id is any non-empty string, the nil UUID included"},
		44: {"skip invalid_uri", "O11: bare % in the label is a skip, not a TS exception"},
		45: {"ok label=\"a\uFFFDb\"", "label is cosmetic: invalid UTF-8 from %FF is replaced with U+FFFD, the link is kept (TS threw URI malformed)"},
		46: {`ok label="nel"`, "D9 / 01 §17.4: strings.TrimSpace strips U+0085"},
		52: {`ok label="x"`, "D18 + D9: net/url splits userinfo at the last @, id \"a@b\" is non-empty"},
		55: {`ok label="a\tb"`, "D9: the fragment is cut before url.Parse, no WHATWG tab stripping"},
		59: {"skip invalid_uri", "D9: net/url rejects a percent-escaped ASCII byte in the host"},
		62: {"skip invalid_uri", "D9 / 01 §17.4: strings.TrimSpace keeps U+FEFF; inputsrc.Load strips a leading BOM of the whole input"},
	}
	for _, c := range corpus {
		var want outcome
		switch {
		case c.TS.OK == nil:
			want = outcome{code: "TS error: " + c.TS.Error}
		case *c.TS.OK:
			want = outcome{ok: true, label: c.TS.Label}
		default:
			switch tsStage(c.TS.ReasonCode, c.TS.Reason) {
			case "parse":
				want = outcome{code: c.TS.ReasonCode, label: c.TS.Label}
			case "normalize":
				// Normalisation-stage skip (unknown params included, D28):
				// Parse must accept the URI.
				want = outcome{ok: true, label: c.TS.Label}
			default:
				t.Errorf("line %d: unclassified TS skip %s %q", c.Line, c.TS.ReasonCode, c.TS.Reason)
				continue
			}
		}
		lines := subscription.ScanLines(c.Raw)
		if len(lines) != 1 {
			t.Fatalf("line %d: ScanLines gave %d lines", c.Line, len(lines))
		}
		if c.TS.ReasonCode == "unsupported_param" {
			// D28: TS's unknown keys minus AllowedParams are Go's Ignored keys.
			var wantIgnored []string
			for _, k := range c.TS.Details {
				if !slices.Contains(AllowedParams, k) {
					wantIgnored = append(wantIgnored, k)
				}
			}
			link, _ := Parse(lines[0])
			if !slices.Equal(link.Ignored, wantIgnored) {
				t.Errorf("line %d: Ignored = %q, want %q", c.Line, link.Ignored, wantIgnored)
			}
		}
		got := goOutcome(lines[0])
		d, allowed := allowedDiffs[c.Line]
		delete(allowedDiffs, c.Line)
		switch {
		case want.matches(got) && allowed:
			t.Errorf("line %d: in allowedDiffs (%s) but Go matches TS: %v", c.Line, d.reason, got)
		case want.matches(got):
		case !allowed:
			t.Errorf("line %d %q: Go %v, TS %v", c.Line, c.Raw, got, want)
		case got.String() != d.got:
			t.Errorf("line %d: Go %v, allowedDiffs expects %s", c.Line, got, d.got)
		}
	}
	for n := range allowedDiffs {
		t.Errorf("allowedDiffs line %d is not in the corpus", n)
	}
}

func TestParseD26CorpusStageParse(t *testing.T) {
	data, err := os.ReadFile(corpusDir + "d26-expect.json")
	if err != nil {
		t.Fatal(err)
	}
	var expect []struct {
		Line       int      `json:"line"`
		Expect     string   `json:"expect"`
		ReasonCode string   `json:"reasonCode"`
		Stage      string   `json:"stage"`
		Warnings   []string `json:"warnings"`
	}
	if err := json.Unmarshal(data, &expect); err != nil {
		t.Fatal(err)
	}
	stageParse := map[int]string{}
	paramIgnored := map[int]bool{}
	for _, e := range expect {
		if e.Expect == "skip" && e.Stage == "parse" {
			stageParse[e.Line] = e.ReasonCode
		}
		paramIgnored[e.Line] = slices.Contains(e.Warnings, "param_ignored")
	}
	if !slices.Contains(slices.Collect(maps.Values(paramIgnored)), true) {
		t.Fatal("d26-expect.json has no param_ignored row")
	}
	if len(stageParse) == 0 {
		t.Fatal("d26-expect.json has no stage=parse skip rows")
	}
	links, err := os.ReadFile(corpusDir + "d26-links.txt")
	if err != nil {
		t.Fatal(err)
	}
	lines := subscription.ScanLines(string(links))
	if len(lines) != len(expect) {
		t.Fatalf("d26-links.txt has %d lines, d26-expect.json %d rows", len(lines), len(expect))
	}
	for _, l := range lines {
		link, skip := Parse(l)
		code, wantSkip := stageParse[l.Number]
		switch {
		case wantSkip && skip == nil:
			t.Errorf("line %d: want skip %s, got link", l.Number, code)
		case wantSkip && skip.Code != code:
			t.Errorf("line %d: skip %s, want %s", l.Number, skip.Code, code)
		case !wantSkip && skip != nil:
			t.Errorf("line %d: unexpected skip %+v", l.Number, *skip)
		case !wantSkip && (len(link.Ignored) != 0) != paramIgnored[l.Number]:
			t.Errorf("line %d: Ignored = %q, param_ignored expected: %v", l.Number, link.Ignored, paramIgnored[l.Number])
		}
	}
}

func readLinks(t *testing.T, text string) []Link {
	t.Helper()
	var out []Link
	for _, l := range subscription.ScanLines(text) {
		link, skip := Parse(l)
		if skip != nil {
			t.Errorf("line %d: unexpected skip %+v", l.Number, *skip)
			continue
		}
		if len(link.Ignored) != 0 {
			t.Errorf("line %d: Ignored = %q", l.Number, link.Ignored)
		}
		out = append(out, link)
	}
	return out
}

func keys(l Link) []string {
	out := make([]string, len(l.Params))
	for i, p := range l.Params {
		out[i] = p.Key
	}
	return out
}

func labels(links []Link) []string {
	out := make([]string, len(links))
	for i, l := range links {
		out[i] = l.Label
	}
	return out
}

func TestParseRealShapes(t *testing.T) {
	read := func(name string) string {
		b, err := os.ReadFile(realShapesDir + name)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	remnawave := readLinks(t, read("remnawave.txt"))
	provider2 := readLinks(t, read("provider2.txt"))
	if n := len(remnawave) + len(provider2); n != 54 {
		t.Fatalf("got %d links, want 54", n)
	}
	wantRemnawave := []string{"encryption", "type", "path", "mode", "extra", "security", "sni", "fp", "alpn"}
	for _, l := range remnawave {
		if got := keys(l); !slices.Equal(got, wantRemnawave) {
			t.Errorf("remnawave line %d keys %v", l.Line, got)
		}
		if path, _ := l.Get("path"); !strings.HasPrefix(path, "/") {
			t.Errorf("remnawave line %d path %q not decoded", l.Line, path)
		}
		if extra, _ := l.Get("extra"); !json.Valid([]byte(extra)) {
			t.Errorf("remnawave line %d extra is not JSON: %q", l.Line, extra)
		}
	}
	if !slices.Contains(labels(remnawave), "🇩🇪DE-1-X") {
		t.Errorf("remnawave labels %q lack 🇩🇪DE-1-X", labels(remnawave))
	}
	for _, l := range provider2 {
		if got := keys(l); !slices.Equal(got, []string{"type", "path", "security", "alpn"}) {
			t.Errorf("provider2 line %d keys %v", l.Line, got)
		}
	}
	if !slices.Contains(labels(provider2), "🇨🇴 Colombia, Bogotá") {
		t.Errorf("provider2 labels lack 🇨🇴 Colombia, Bogotá")
	}

	decoded, ok := inputsrc.DecodeAuto(read("provider2.b64"))
	if !ok {
		t.Fatal("provider2.b64 was not decoded as base64")
	}
	if fromB64 := readLinks(t, decoded); !reflect.DeepEqual(fromB64, provider2) {
		t.Errorf("provider2.b64 links differ from provider2.txt (%d vs %d)", len(fromB64), len(provider2))
	}
}
