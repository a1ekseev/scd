package manifest

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/a1ekseev/scd/internal/common/inputsrc"
	"github.com/a1ekseev/scd/internal/common/subscription"
	"github.com/a1ekseev/scd/internal/common/vless"
	"github.com/a1ekseev/scd/internal/scd/config"
	"github.com/a1ekseev/scd/internal/scd/normalize"
)

const (
	root          = "../../../"
	corpusDir     = root + "testdata/reference/scd/corpus/"
	generateDir   = root + "testdata/reference/scd/generate/"
	vpnSample     = root + "testdata/reference/scd/fixtures/vpn-sample.txt"
	realShapesDir = root + "docker/fixtures/real-shapes/"
	realShapeGo   = root + "docker/scd/config.real-shape.go.yml"
)

var (
	testNow = time.Date(2026, 10, 17, 8, 32, 15, 445_000_000, time.FixedZone("x", 3*3600))
	allow   = normalize.Policy{AllowInsecure: true, TagPrefix: "vless"}
	deny    = normalize.Policy{TagPrefix: "vless"}
)

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// link returns a tcp+tls VLESS link with user n and the given label.
func link(n int, label string) string {
	return fmt.Sprintf("vless://00000000-0000-4000-8000-%012d@h%d.example.test:443?type=tcp&security=tls#%s", n, n, label)
}

// links builds a manifest of tcp+tls links with the given labels.
func links(t *testing.T, labels ...string) Manifest {
	t.Helper()
	var lines []string
	for i, l := range labels {
		lines = append(lines, link(i+1, l))
	}
	m := Build(strings.Join(lines, "\n"), "test", deny, testNow)
	if len(m.Entries) != len(labels) {
		t.Fatalf("built %d entries of %d: %+v", len(m.Entries), len(labels), m.Skipped)
	}
	return m
}

func tags(m Manifest) []string {
	var out []string
	for _, e := range m.Entries {
		out = append(out, e.Tag)
	}
	return out
}

func codes(w []normalize.Warning) []string {
	out := []string{}
	for _, x := range w {
		out = append(out, x.Code)
	}
	return out
}

func readSummary(t *testing.T, path string) Summary {
	t.Helper()
	var ref struct {
		Summary Summary `json:"summary"`
	}
	if err := json.Unmarshal([]byte(readFile(t, path)), &ref); err != nil {
		t.Fatal(err)
	}
	return ref.Summary
}

func TestScanVpnSample(t *testing.T) { // P1
	if n := len(subscription.ScanLines(readFile(t, vpnSample))); n <= 10 {
		t.Fatalf("%d lines", n)
	}
}

func TestBuildVpnSampleAllowPolicy(t *testing.T) { // P2
	m := Build(readFile(t, vpnSample), "vpn-sample", allow, testNow)
	if m.Summary.Parsed != len(m.Entries) || len(m.Entries) != 12 || len(m.Skipped) != 0 {
		t.Fatalf("parsed %d entries %d skipped %+v", m.Summary.Parsed, len(m.Entries), m.Skipped)
	}
	profiles := map[string]bool{}
	insecure := 0
	for _, e := range m.Entries {
		profiles[e.Profile] = true
		if e.Profile == "ws-tls" {
			if !slices.Equal(codes(e.Warnings), []string{normalize.WarnAllowInsecureIgnored}) {
				t.Fatalf("line %d warnings %v", e.Line, e.Warnings)
			}
			insecure++
		} else if len(e.Warnings) != 0 {
			t.Fatalf("line %d warnings %v", e.Line, e.Warnings)
		}
	}
	for _, p := range []string{"tcp-tls", "ws-tls", "tcp-reality-vision"} {
		if !profiles[p] {
			t.Fatalf("profile %s missing: %v", p, profiles)
		}
	}
	if insecure != 2 {
		t.Fatalf("%d ws-tls entries", insecure)
	}
}

func TestBuildVpnSampleDenyPolicy(t *testing.T) {
	m := Build(readFile(t, vpnSample), "vpn-sample", deny, testNow)
	if len(m.Entries) != 10 || m.Summary.Parsed != 10 || m.Summary.TotalLines != 12 {
		t.Fatalf("entries %d summary %+v", len(m.Entries), m.Summary)
	}
	for _, e := range m.Entries {
		if e.Profile == "ws-tls" {
			t.Fatalf("ws-tls entry on line %d", e.Line)
		}
	}
	if len(m.Skipped) != 2 || m.Summary.Skipped != 2 || m.Summary.UnsupportedValue != 2 {
		t.Fatalf("skipped %+v summary %+v", m.Skipped, m.Summary)
	}
	for i, s := range m.Skipped {
		if s.Line != 9+i || s.Code != vless.CodeUnsupportedValue || !strings.Contains(s.Reason, "allowInsecure=1 requires pcs or vcn") {
			t.Fatalf("skip %+v", s)
		}
	}
}

func TestBuildSameCountryDifferentCities(t *testing.T) { // P7
	l := "vless://11111111-1111-4111-8111-111111111111@at1.example.test:8443?type=tcp&security=tls#🇦🇹 "
	m := Build(l+"Вена, Австрия\n"+l+"Зальцбург, Австрия", "s", deny, testNow)
	if len(m.Entries) != 2 || m.Entries[0].Tag == m.Entries[1].Tag {
		t.Fatalf("tags %v skipped %+v", tags(m), m.Skipped)
	}
}

func TestBuildSkipsDuplicateTags(t *testing.T) { // P8
	l := link(1, "🇦🇹 Вена")
	m := Build(l+"\n"+l, "s", deny, testNow)
	if len(m.Entries) != 1 || m.Summary.Parsed != 1 || m.Summary.Skipped != 1 || m.Summary.DuplicateTag != 1 {
		t.Fatalf("entries %d summary %+v", len(m.Entries), m.Summary)
	}
	s := m.Skipped[0]
	want := `Duplicate generated outbound tag "` + m.Entries[0].Tag + `".`
	if s.Code != vless.CodeDuplicateTag || s.Line != 2 || s.Reason != want || s.Label == nil || *s.Label != "🇦🇹 Вена" {
		t.Fatalf("skip %+v, want reason %q", s, want)
	}
}

func TestBuildSkipsNonVlessScheme(t *testing.T) { // P9
	m := Build("trojan://example", "s", deny, testNow)
	if len(m.Skipped) != 1 || m.Skipped[0].Code != vless.CodeUnsupportedScheme || m.Summary.UnsupportedScheme != 1 {
		t.Fatalf("skipped %+v summary %+v", m.Skipped, m.Summary)
	}
}

func TestBuildUnknownParamIsEntry(t *testing.T) { // P10, changed by D28
	m := Build(strings.Replace(link(1, "x"), "?", "?foo=bar&", 1), "s", deny, testNow)
	if len(m.Entries) != 1 || m.Summary.UnsupportedParam != 0 || m.Summary.Skipped != 0 {
		t.Fatalf("entries %d skipped %+v summary %+v", len(m.Entries), m.Skipped, m.Summary)
	}
	w := m.Entries[0].Warnings
	if !slices.Equal(codes(w), []string{normalize.WarnParamIgnored}) || !strings.Contains(w[0].Message, "foo") {
		t.Fatalf("warnings %+v", w)
	}
}

func TestBuildUnsupportedCombo(t *testing.T) { // P19
	m := Build(strings.Replace(link(1, "x"), "type=tcp", "type=grpc", 1), "s", deny, testNow)
	if len(m.Skipped) != 1 || m.Skipped[0].Code != vless.CodeUnsupportedCombo || m.Summary.UnsupportedCombo != 1 {
		t.Fatalf("skipped %+v summary %+v", m.Skipped, m.Summary)
	}
}

func TestBuildIgnoresTrailingEmptyLines(t *testing.T) { // P20
	m := Build(link(1, "x")+"\n", "s", deny, testNow)
	if m.Summary.TotalLines != 1 || m.Summary.Parsed != 1 || m.Summary.Skipped != 0 {
		t.Fatalf("summary %+v", m.Summary)
	}
}

func TestBuildManifestCRLFEqualsLF(t *testing.T) { // Review Focus 3
	lf := readFile(t, vpnSample)
	crlf := strings.ReplaceAll(lf, "\n", "\r\n")
	a, b := Build(lf, "s", allow, testNow), Build(crlf, "s", allow, testNow)
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("CRLF manifest differs:\n%v\n%v", tags(a), tags(b))
	}
	// Blank lines and trailing white space around the text do not change
	// the entries either, except for line numbers.
	padded := Build("\n  \n"+lf+"\n\n", "s", allow, testNow)
	if !slices.Equal(tags(a), tags(padded)) || padded.Entries[0].Line != 3 {
		t.Fatalf("padded tags %v", tags(padded))
	}
}

func TestBuildSkipsXrayRejected(t *testing.T) {
	l := "vless://7c0e4b8a-2f3d-4e6a-9b1c-5d8f0a2e4c61@reality.example.test:443?type=tcp&security=reality" +
		"&sni=www.example.test&fp=chrome&pbk=kp_D9wlmhWWw8QVA9OCZwedG12QDFk68zaolbEh6c1M&sid=abc#r"
	m := Build(l, "s", deny, testNow)
	if len(m.Entries) != 0 || len(m.Skipped) != 1 || m.Summary.UnsupportedValue != 1 {
		t.Fatalf("entries %d skipped %+v", len(m.Entries), m.Skipped)
	}
	s := m.Skipped[0]
	if s.Code != vless.CodeUnsupportedValue || s.Reason != "Xray rejected the outbound configuration." ||
		len(s.Details) != 1 || !strings.Contains(s.Details[0], `invalid "shortId"`) || s.Label == nil || *s.Label != "r" {
		t.Fatalf("skip %+v", s)
	}
}

// expectRow is one row of d26-expect.json.
type expectRow struct {
	Line           int      `json:"line"`
	Expect         string   `json:"expect"`
	Stage          string   `json:"stage"`
	ReasonCode     string   `json:"reasonCode"`
	ExpectContains string   `json:"expectContains"`
	Profile        string   `json:"profile"`
	Warnings       []string `json:"warnings"`
}

func TestBuildD26Corpus(t *testing.T) {
	var rows []expectRow
	if err := json.Unmarshal([]byte(readFile(t, corpusDir+"d26-expect.json")), &rows); err != nil {
		t.Fatal(err)
	}
	text := readFile(t, corpusDir+"d26-links.txt")
	for _, p := range []normalize.Policy{deny, allow} {
		m := Build(text, "d26", p, testNow)
		if m.Summary.TotalLines != len(rows) || m.Summary.Parsed+m.Summary.Skipped != len(rows) {
			t.Fatalf("policy %+v: summary %+v, %d rows", p, m.Summary, len(rows))
		}
		entries := map[int]Entry{}
		for _, e := range m.Entries {
			entries[e.Line] = e
		}
		skips := map[int]vless.Skip{}
		for _, s := range m.Skipped {
			skips[s.Line] = s
		}
		for _, r := range rows {
			e, isEntry := entries[r.Line]
			s, isSkip := skips[r.Line]
			switch {
			case r.Expect == "entry":
				if !isEntry {
					t.Errorf("policy %+v line %d: got skip %+v, want entry", p, r.Line, s)
					continue
				}
				if e.Profile != r.Profile || !slices.Equal(codes(e.Warnings), r.Warnings) {
					t.Errorf("policy %+v line %d: profile %s warnings %v, want %s %v", p, r.Line, e.Profile, codes(e.Warnings), r.Profile, r.Warnings)
				}
			case !isSkip:
				t.Errorf("policy %+v line %d: got entry %s, want skip", p, r.Line, e.Profile)
			case s.Code != r.ReasonCode:
				t.Errorf("policy %+v line %d: code %s, want %s", p, r.Line, s.Code, r.ReasonCode)
			case r.Stage != "parse" && !strings.Contains(s.Reason+"\n"+strings.Join(s.Details, "\n"), r.ExpectContains):
				t.Errorf("policy %+v line %d: %q %q does not contain %q", p, r.Line, s.Reason, s.Details, r.ExpectContains)
			case r.Stage == "xray" && s.Reason != reasonXrayRejected:
				t.Errorf("policy %+v line %d: reason %q", p, r.Line, s.Reason)
			}
		}
	}
}

func TestBuild3xuiRealityLink(t *testing.T) { // Review Focus 7
	l := strings.Split(readFile(t, corpusDir+"d26-links.txt"), "\n")[71]
	for _, p := range []string{"spx=", "pqv=", "support-x25519mlkem768=true", "flow=xtls-rprx-vision"} {
		if !strings.Contains(l, p) {
			t.Fatalf("line 72 lacks %s", p)
		}
	}
	m := Build(l, "s", deny, testNow)
	if len(m.Entries) != 1 || m.Entries[0].Profile != "tcp-reality-vision" || len(m.Entries[0].Warnings) != 0 {
		t.Fatalf("entries %+v skipped %+v", m.Entries, m.Skipped)
	}
	r := m.Entries[0].JSONOutbound.StreamSettings.RealitySettings
	if r == nil || r.SpiderX == "" || r.Mldsa65Verify == "" {
		t.Fatalf("realitySettings %+v", r)
	}
}

// realShapeFilters returns the filters of the first subscription of the
// Go form of the owner's config.
func realShapeFilters(t *testing.T) *config.SourceFiltersConfig {
	t.Helper()
	l, _, err := config.Load(realShapeGo)
	if err != nil {
		t.Fatal(err)
	}
	f := l.Config.Subscriptions[0].Sources[0].Filters
	if f == nil || len(f.CountryAllowlist) != 24 || len(f.Label.Exclude) != 1 {
		t.Fatalf("filters %+v", f)
	}
	return f
}

func loadRealShape(t *testing.T, name string) string {
	t.Helper()
	l, err := inputsrc.Load(context.Background(), realShapesDir+name, inputsrc.Options{})
	if err != nil {
		t.Fatal(err)
	}
	return l.Content
}

func TestBuildRealShapeProvider2MatchesTS(t *testing.T) { // 12 §9 п.8
	var ref struct {
		Entries []struct {
			Tag string `json:"tag"`
		} `json:"entries"`
		Summary Summary `json:"summary"`
	}
	if err := json.Unmarshal([]byte(readFile(t, generateDir+"real-shapes-provider2.manifest.json")), &ref); err != nil {
		t.Fatal(err)
	}
	var want []string
	for _, e := range ref.Entries {
		want = append(want, e.Tag)
	}
	m := Build(loadRealShape(t, "provider2.b64"), "<input>", deny, testNow)
	if got := tags(m); len(want) != 50 || !slices.Equal(got, want) {
		t.Fatalf("tags differ from TS:\n got %v\nwant %v", got, want)
	}
	if m.Summary != ref.Summary {
		t.Fatalf("summary %+v, want %+v", m.Summary, ref.Summary)
	}
	for _, e := range m.Entries {
		if len(e.Warnings) != 0 {
			t.Fatalf("line %d warnings %v", e.Line, e.Warnings)
		}
	}
	f := ApplyFilters(m, realShapeFilters(t))
	if len(f.Entries) != 25 || f.Summary.Parsed != 25 || f.Summary.FilteredByCountry != 25 ||
		f.Summary.FilteredByLabelRegex != 0 || f.Summary.Filtered != 25 {
		t.Fatalf("filtered: %d entries, summary %+v", len(f.Entries), f.Summary)
	}
}

func TestBuildRealShapeRemnawave(t *testing.T) {
	// TS parses 0 of these lines (real-shapes-remnawave.error.txt); Go
	// accepts all 4 on purpose (D26, D28).
	m := Build(loadRealShape(t, "remnawave.txt"), "<input>", deny, testNow)
	if len(m.Entries) != 4 || len(m.Skipped) != 0 {
		t.Fatalf("entries %d skipped %+v", len(m.Entries), m.Skipped)
	}
	for _, e := range m.Entries {
		if e.Profile != "xhttp-tls" || len(e.Warnings) != 0 {
			t.Fatalf("line %d: %s %v", e.Line, e.Profile, e.Warnings)
		}
	}
	if f := ApplyFilters(m, realShapeFilters(t)); len(f.Entries) != 4 || f.Summary.Filtered != 0 {
		t.Fatalf("filtered: %d entries, summary %+v", len(f.Entries), f.Summary)
	}
}

func TestBuildLabelWithServerDescription(t *testing.T) { // 12 §10 п.3
	// Remnawave appends ?serverDescription= to the label for extended
	// clients; like TS, it ends up in the city and the tag (not fixed).
	m := Build(link(1, "🇩🇪 Berlin?serverDescription=QUJD"), "s", deny, testNow)
	if len(m.Entries) != 1 {
		t.Fatalf("skipped %+v", m.Skipped)
	}
	e := m.Entries[0]
	if e.City != "Berlin?serverDescription=QUJD" || !strings.HasPrefix(e.Tag, "vless-de-berlin-serverdescription-qujd-tcp-tls-") {
		t.Fatalf("city %q tag %q", e.City, e.Tag)
	}
}

func TestBuildCountsSummary(t *testing.T) {
	lines := []string{
		link(1, "a"),          // entry
		"trojan://x",          // unsupported_scheme
		"vless://x@ho st:443", // invalid_uri
		"vless://00000000-0000-4000-8000-000000000001@h.example.test", // missing_required_field
		strings.Replace(link(2, "b"), "type=tcp", "type=kcp", 1),      // unsupported_combo
		strings.Replace(link(3, "c"), "?", "?encryption=None&", 1),    // unsupported_value
		link(1, "a"), // duplicate_tag
		"",
		strings.Replace(link(4, "d"), "?", "?zz=1&", 1), // entry with param_ignored
	}
	m := Build(strings.Join(lines, "\n"), "s", deny, testNow)
	want := Summary{
		TotalLines: 8, Parsed: 2, Skipped: 6, UnsupportedScheme: 1, InvalidURI: 1, MissingRequiredField: 1,
		UnsupportedCombo: 1, UnsupportedValue: 1, DuplicateTag: 1,
	}
	if m.Summary != want {
		t.Fatalf("summary %+v\nwant %+v\nskipped %+v", m.Summary, want, m.Skipped)
	}
}

func TestBuildMatchesTSForLegacyProfiles(t *testing.T) {
	var ref struct {
		Entries []struct {
			Tag  string `json:"tag"`
			Line int    `json:"line"`
		} `json:"entries"`
		Skipped []vless.Skip `json:"skipped"`
	}
	if err := json.Unmarshal([]byte(readFile(t, generateDir+"vpn-sample.manifest.json")), &ref); err != nil {
		t.Fatal(err)
	}
	var want []string
	for _, e := range ref.Entries {
		want = append(want, e.Tag)
	}
	text := readFile(t, vpnSample)
	// With allowInsecure: allow every TS entry is an entry with the same tag.
	m := Build(text, "s", allow, testNow)
	if !slices.Equal(tags(m), want) || len(m.Skipped) != len(ref.Skipped) {
		t.Fatalf("tags %v\nwant %v\nskipped %+v", tags(m), want, m.Skipped)
	}
	if want := readSummary(t, generateDir+"vpn-sample.manifest.json"); m.Summary != want {
		t.Fatalf("summary %+v, want %+v", m.Summary, want)
	}
	// Under the default deny policy the two ws links with allowInsecure=1 and
	// no pcs/vcn are skipped (D15, intended divergence); the rest is equal.
	m = Build(text, "s", deny, testNow)
	var kept []string
	for _, e := range ref.Entries {
		if e.Line != 9 && e.Line != 10 {
			kept = append(kept, e.Tag)
		}
	}
	if !slices.Equal(tags(m), kept) {
		t.Fatalf("deny tags %v\nwant %v", tags(m), kept)
	}
}

func TestManifestJSON(t *testing.T) {
	m := Build("trojan://x", "src", deny, testNow)
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"version":1,"generatedAt":"2026-10-17T05:32:15.445Z","sourceFile":"src","entries":[],` +
		`"skipped":[{"line":1,"raw":"trojan://x","reasonCode":"unsupported_scheme","reason":"Unsupported scheme \"trojan:\"."}],` +
		`"summary":{"totalLines":1,"parsed":0,"skipped":1,"filtered":0,"filteredByCountry":0,"filteredByLabelRegex":0,` +
		`"unsupportedScheme":1,"unsupportedParam":0,"unsupportedValue":0,"unsupportedCombo":0,"invalidUri":0,` +
		`"missingRequiredField":0,"duplicateTag":0}}`
	if string(b) != want {
		t.Fatalf("got\n%s\nwant\n%s", b, want)
	}
	lb, err := json.Marshal(m.ParseLog())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(lb), `{"generatedAt":"2026-10-17T05:32:15.445Z","summary":{"totalLines":1,`) ||
		!strings.HasSuffix(string(lb), `"skipped":[{"line":1,"raw":"trojan://x","reasonCode":"unsupported_scheme","reason":"Unsupported scheme \"trojan:\"."}]}`) {
		t.Fatalf("parse log %s", lb)
	}
	if b, _ := json.Marshal(Build("", "src", deny, testNow).ParseLog()); !strings.HasSuffix(string(b), `"skipped":[]}`) {
		t.Fatalf("empty parse log %s", b)
	}

	e := Build(strings.Replace(link(1, "🇩🇪 Berlin"), "?", "?zz=1&", 1), "src", deny, testNow).Entries[0]
	eb, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	var obj map[string]json.RawMessage
	_ = json.Unmarshal(eb, &obj)
	for _, k := range []string{"kind", "tag", "label", "profile", "line", "country", "city", "normalized", "jsonOutbound", "warnings"} {
		if _, ok := obj[k]; !ok {
			t.Fatalf("entry JSON lacks %s: %s", k, eb)
		}
		keys = append(keys, `"`+k+`":`)
	}
	pos := -1
	for _, k := range keys {
		i := strings.Index(string(eb), k)
		if i < pos {
			t.Fatalf("key %s out of order: %s", k, eb)
		}
		pos = i
	}
	if len(obj) != len(keys) {
		t.Fatalf("extra keys: %s", eb)
	}
	plain, _ := json.Marshal(Build(link(1, ""), "src", deny, testNow).Entries[0])
	for _, k := range []string{`"country"`, `"city"`, `"warnings"`} {
		if strings.Contains(strings.SplitN(string(plain), `"normalized"`, 2)[0], k) {
			t.Fatalf("empty %s not omitted: %s", k, plain)
		}
	}
}
