package config

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/a1ekseev/scd/internal/common/jsonout"
)

const realShapeGo = "../../../docker/sfc/config.real-shape.go.yml"

// base is a minimal valid config; tests replace parts of it.
const base = `subscriptions:
  - id: main
    input: ./sub.txt
    pathRoute: /s
    outputs:
      - id: out
        labelIncludeRegex: '/Германия/i'
runtime:
  refreshSchedule: "*/10 * * * *"
server:
  listen: 0.0.0.0:8080
`

func write(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func mustLoad(t *testing.T, body string) Loaded {
	t.Helper()
	l, err := Load(write(t, body))
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func mustFail(t *testing.T, body string, want ...string) string {
	t.Helper()
	_, err := Load(write(t, body))
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	for _, w := range want {
		if !strings.Contains(msg, w) {
			t.Errorf("missing %q in:\n%s", w, msg)
		}
	}
	return msg
}

// replace swaps one exact fragment of base.
func replace(t *testing.T, old, repl string) string {
	t.Helper()
	if !strings.Contains(base, old) {
		t.Fatalf("fragment %q not in base", old)
	}
	return strings.Replace(base, old, repl, 1)
}

func marshal(t *testing.T, v any) string {
	t.Helper()
	b, err := jsonout.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var c bytes.Buffer
	if err := json.Compact(&c, b); err != nil {
		t.Fatal(err)
	}
	return c.String()
}

func TestLoadDefaultsAndResolve(t *testing.T) {
	l := mustLoad(t, base)
	cfg := l.Config
	if l.Dir != filepath.Dir(l.Path) || !filepath.IsAbs(l.Path) {
		t.Fatalf("path %q dir %q", l.Path, l.Dir)
	}
	s := cfg.Subscriptions[0]
	if s.Input != filepath.Join(l.Dir, "sub.txt") || !s.Enabled || s.Format != "auto" || s.FetchTimeoutMs != 5000 ||
		s.FetchUserAgent != "" || s.FetchHeaders != nil || s.PathRoute != "/s" {
		t.Fatalf("subscription %+v", s)
	}
	o := s.Outputs[0]
	if o.ID != "out" || !o.Enabled || o.Name != "" || len(o.Filter.Include) != 1 || o.Filter.Exclude != nil {
		t.Fatalf("output %+v", o)
	}
	if cfg.Runtime.RefreshSchedule != "*/10 * * * *" || cfg.Server.Listen != "0.0.0.0:8080" ||
		cfg.Logging.Level != "info" || cfg.Logging.Format != "json" {
		t.Fatalf("config %+v", cfg)
	}
}

func TestIDTrimmed(t *testing.T) {
	body := replace(t, "  - id: main\n", "  - id: '  main  '\n")
	if got := mustLoad(t, body).Config.Subscriptions[0].ID; got != "main" {
		t.Fatalf("id %q", got)
	}
}

// Final review M6: every config object rejects unknown keys (like scd and gtl).
func TestUnknownKeysRejected(t *testing.T) {
	cases := []struct{ name, body, want string }{
		{"root", base + "whatever: true\n", `<root>: unknown key "whatever"`},
		{"subscription", replace(t, "  - id: main\n", "  - id: main\n    extra: 1\n"), `subscriptions.0: unknown key "extra"`},
		{"output", replace(t, "      - id: out\n", "      - id: out\n        extra: 1\n"), `subscriptions.0.outputs.0: unknown key "extra"`},
		{"runtime", replace(t, "runtime:\n", "runtime:\n  extra: 1\n"), `runtime: unknown key "extra"`},
		{"server", replace(t, "server:\n", "server:\n  extra: 1\n"), `server: unknown key "extra"`},
		{"logging", base + "logging: {level: info, extra: 1}\n", `logging: unknown key "extra"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { mustFail(t, c.body, c.want) })
	}
}

func TestInputKeptForURLAndStdin(t *testing.T) {
	for _, in := range []string{"https://sub.example.com/x", "HTTP://h/x", "-", "/abs/file"} {
		got := mustLoad(t, replace(t, "./sub.txt", "'"+in+"'")).Config.Subscriptions[0].Input
		if got != in {
			t.Errorf("input %q -> %q", in, got)
		}
	}
}

func TestRejectsBrokenRegexAndDuplicateOutput(t *testing.T) {
	body := replace(t, "        labelIncludeRegex: '/Германия/i'\n",
		"        labelIncludeRegex: '/broken'\n      - id: out\n        labelIncludeRegex: x\n")
	mustFail(t, body,
		`subscriptions.0.outputs.0.labelIncludeRegex: regex literal must end with "/flags"`,
		`subscriptions.0.outputs.1.id: duplicate output id`)
}

func TestDuplicateIDsAcrossDisabled(t *testing.T) {
	body := `subscriptions:
  - id: main
    input: a
    pathRoute: /s
    outputs:
      - id: out
        labelIncludeRegex: x
  - id: main
    enabled: false
    input: b
    pathRoute: /t
    outputs:
      - id: out
        enabled: false
        labelExcludeRegex: y
runtime:
  refreshSchedule: "@hourly"
server:
  listen: :8080
`
	mustFail(t, body,
		`subscriptions.1.id: duplicate subscription id "main"`,
		`subscriptions.1.outputs.0.id: duplicate output id`)
}

func TestAtLeastOneEnabled(t *testing.T) {
	mustFail(t, replace(t, "    pathRoute: /s\n", "    pathRoute: /s\n    enabled: false\n"),
		"subscriptions: at least one subscription must be enabled")
	mustFail(t, replace(t, "      - id: out\n", "      - id: out\n        enabled: false\n"),
		"subscriptions: at least one output must be enabled")
}

func TestPathRouteRules(t *testing.T) {
	if got := mustLoad(t, replace(t, "pathRoute: /s", "pathRoute: /profiles")).Config.Subscriptions[0].PathRoute; got != "/profiles" {
		t.Fatalf("pathRoute %q", got)
	}
	mustFail(t, replace(t, "    pathRoute: /s\n", ""), "  - subscriptions.0.pathRoute: required")
	msg := mustFail(t, replace(t, "pathRoute: /s", "pathRoute: profiles/"), "subscriptions.0.pathRoute: must start with /")
	if !strings.Contains(msg, "subscriptions.0.pathRoute: must not end with /") {
		t.Fatal(msg)
	}
	mustFail(t, replace(t, "pathRoute: /s", "pathRoute: /"), "subscriptions.0.pathRoute: must not equal /")
	mustFail(t, replace(t, "pathRoute: /s", "pathRoute: ' '"), "subscriptions.0.pathRoute: must not be empty")
	// Final review M7: requests are matched on the escaped, cleaned path.
	for _, bad := range []string{"'/my sub'", "/a/../b", "/a/./b", "//a", "/a//b", "/a%20b", "/a/%2e%2e/b", "/s?x", "/ü"} {
		mustFail(t, replace(t, "pathRoute: /s", "pathRoute: "+bad), "subscriptions.0.pathRoute: must be a clean URL path")
	}
	// Re-review R2: "," and ";" are kept as is by EscapedPath, so they match.
	for _, good := range []string{"/s", "/a/b-c_d.e~f", "/s/v1", "/a,b", "/a;b", "/a:b@c"} {
		mustLoad(t, replace(t, "pathRoute: /s", "pathRoute: "+good))
	}
}

func TestUserAgentAndIntervalBounds(t *testing.T) {
	out := "        labelIncludeRegex: '/Германия/i'\n"
	mustFail(t, replace(t, out, out+"        userAgent: ['']\n        profileUpdateInterval: 25\n"),
		"subscriptions.0.outputs.0.userAgent.0: must not be empty",
		"subscriptions.0.outputs.0.profileUpdateInterval: must be at most 24")
	mustFail(t, replace(t, out, out+"        userAgent: []\n        profileUpdateInterval: 0\n"),
		"subscriptions.0.outputs.0.userAgent: must not be empty",
		"subscriptions.0.outputs.0.profileUpdateInterval: must be at least 1")
}

func TestProfileFieldsPreserved(t *testing.T) {
	out := "        labelIncludeRegex: '/Германия/i'\n"
	o := mustLoad(t, replace(t, out, out+"        userAgent: [Clash, ' Stash ']\n        profileTitle: Main Profile\n        profileUpdateInterval: 24\n")).Config.Subscriptions[0].Outputs[0]
	if !reflect.DeepEqual(o.UserAgent, []string{"Clash", "Stash"}) || o.ProfileTitle != "Main Profile" || o.ProfileUpdateInterval != 24 {
		t.Fatalf("output %+v", o)
	}
}

func TestEnvMissingBecomesEmpty(t *testing.T) {
	t.Setenv("SFC_TEST_NAME", "named")
	out := "        labelIncludeRegex: '/Германия/i'\n"
	o := mustLoad(t, replace(t, out, out+"        name: ${SFC_TEST_NAME}\n        profileTitle: '${SFC_TEST_MISSING_VAR}'\n")).Config.Subscriptions[0].Outputs[0]
	if o.Name != "named" || o.ProfileTitle != "" {
		t.Fatalf("output %+v", o)
	}
}

func TestEnvInNumberFieldIsTypeError(t *testing.T) {
	t.Setenv("TM", "5000")
	mustFail(t, replace(t, "    pathRoute: /s\n", "    pathRoute: /s\n    fetchTimeoutMs: ${TM}\n"),
		"subscriptions.0.fetchTimeoutMs: expected integer, got string")
}

func TestFetchTimeoutPositive(t *testing.T) {
	mustFail(t, replace(t, "    pathRoute: /s\n", "    pathRoute: /s\n    fetchTimeoutMs: 0\n"),
		"subscriptions.0.fetchTimeoutMs: must be at least 1")
}

func TestFormatAndEnabledTypes(t *testing.T) {
	mustFail(t, replace(t, "    pathRoute: /s\n", "    pathRoute: /s\n    format: json\n    enabled: 'yes'\n"),
		"subscriptions.0.format: must be one of auto|plain|base64",
		"subscriptions.0.enabled: expected boolean, got string")
}

func TestSFCRealShapeGoFormValid(t *testing.T) {
	l, err := Load(realShapeGo)
	if err != nil {
		t.Fatal(err)
	}
	s := l.Config.Subscriptions[0]
	if len(s.Outputs) != 3 || s.FetchTimeoutMs != 15000 || l.Config.Runtime.RefreshSchedule != "15 * * * *" {
		t.Fatalf("config %+v", l.Config)
	}
	// D29 table: the three filters of the owner's config.
	want := []string{
		`{"labelExcludeRegex":["/Extra|WhiteList/i"]}`,
		`{"labelIncludeRegex":["/Extra/i"],"labelExcludeRegex":["/WhiteList/i"]}`,
		`{"labelIncludeRegex":["/Extra/i","/WhiteList/i"]}`,
	}
	for i, o := range s.Outputs {
		if got := marshal(t, o.Filter); got != want[i] {
			t.Errorf("output %d filter %s, want %s", i, got, want[i])
		}
	}
	labels := []struct {
		label string
		out   [3]bool
	}{
		{"🇩🇪 Германия", [3]bool{true, false, false}},
		{"🇩🇪 Германия Extra", [3]bool{false, true, false}},
		{"🇷🇺 Россия WhiteList", [3]bool{false, false, false}},
		{"🇷🇺 Россия extra whitelist", [3]bool{false, false, true}},
	}
	for _, c := range labels {
		for i, o := range s.Outputs {
			if got := o.Filter.Match(c.label); got != c.out[i] {
				t.Errorf("%q output %d: %v", c.label, i, got)
			}
		}
	}
}

func TestOutputRequiresSomeFilter(t *testing.T) {
	mustFail(t, replace(t, "        labelIncludeRegex: '/Германия/i'\n", "        name: x\n"),
		"subscriptions.0.outputs.0: labelIncludeRegex or labelExcludeRegex is required")
	mustLoad(t, replace(t, "labelIncludeRegex: '/Германия/i'", "labelExcludeRegex: [a, b]"))
}

func TestRejectsLookaheadRegex(t *testing.T) {
	msg := mustFail(t, replace(t, "'/Германия/i'", "'/^(?!.*Extra).*$/i'"),
		"subscriptions.0.outputs.0.labelIncludeRegex: lookahead, lookbehind and backreferences are not supported")
	if strings.Contains(msg, "labelIncludeRegex or labelExcludeRegex is required") {
		t.Fatalf("invalid filter must not also be reported as missing:\n%s", msg)
	}
}

func TestRejectsEmptyRefreshSchedule(t *testing.T) {
	mustFail(t, replace(t, `"*/10 * * * *"`, `"  "`), "runtime.refreshSchedule: cron expression must not be empty")
	mustFail(t, replace(t, "runtime:\n  refreshSchedule: \"*/10 * * * *\"\n", ""), "  - runtime: required")
}

func TestRejectsLibraryRejectedCron(t *testing.T) {
	for _, expr := range []string{"0 0 L * *", "0 0 31 2 *"} {
		mustFail(t, replace(t, "*/10 * * * *", expr), "  - runtime.refreshSchedule: ")
	}
	if got := mustLoad(t, replace(t, "*/10 * * * *", "0 0 ? * *")).Config.Runtime.RefreshSchedule; got != "0 0 ? * *" {
		t.Fatalf("schedule %q", got)
	}
}

func TestListenValidated(t *testing.T) {
	mustFail(t, replace(t, "0.0.0.0:8080", "localhost"), `server.listen: listen address must be in "host:port" format`)
	mustFail(t, replace(t, "0.0.0.0:8080", "h:0"), "server.listen: listen address port must be an integer")
}

func TestFetchUserAgentOptional(t *testing.T) {
	l := mustLoad(t, base)
	if strings.Contains(marshal(t, l.Config), "fetchUserAgent") {
		t.Fatal("absent fetchUserAgent printed")
	}
	ua := mustLoad(t, replace(t, "    pathRoute: /s\n", "    pathRoute: /s\n    fetchUserAgent: ' Happ/1.0 '\n")).Config.Subscriptions[0].FetchUserAgent
	if ua != "Happ/1.0" {
		t.Fatalf("ua %q", ua)
	}
	mustFail(t, replace(t, "    pathRoute: /s\n", "    pathRoute: /s\n    fetchUserAgent: '  '\n"),
		"subscriptions.0.fetchUserAgent: must not be empty")
}

func TestLoggingFormatEnum(t *testing.T) {
	cfg := mustLoad(t, base+"logging:\n  format: pretty\n").Config
	if cfg.Logging.Format != "pretty" || cfg.Logging.Level != "info" {
		t.Fatalf("logging %+v", cfg.Logging)
	}
	mustFail(t, base+"logging:\n  format: text\n", "logging.format: must be one of json|pretty")
	mustFail(t, base+"logging:\n  level: loud\n", "logging.level: must be one of")
}

func TestEmptyNameDropped(t *testing.T) {
	out := "        labelIncludeRegex: '/Германия/i'\n"
	cfg := mustLoad(t, replace(t, out, out+"        name: '  '\n        profileTitle: ' '\n")).Config
	got := marshal(t, cfg.Subscriptions[0].Outputs[0])
	if strings.Contains(got, `"name"`) || strings.Contains(got, `"profileTitle"`) {
		t.Fatalf("empty fields printed: %s", got)
	}
}

func TestOutputJSONOrder(t *testing.T) {
	out := "        labelIncludeRegex: '/Германия/i'\n"
	o := mustLoad(t, replace(t, out, out+"        labelExcludeRegex: '<x>'\n        name: n\n        userAgent: [A]\n        profileTitle: T\n        profileUpdateInterval: 6\n")).Config.Subscriptions[0].Outputs[0]
	want := `{"id":"out","enabled":true,"name":"n","labelIncludeRegex":["/Германия/i"],"labelExcludeRegex":["<x>"],"userAgent":["A"],"profileTitle":"T","profileUpdateInterval":6}`
	if got := marshal(t, o); got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

func TestFetchHeaders(t *testing.T) {
	t.Setenv("HW", "s3cr3t")
	l := mustLoad(t, replace(t, "    pathRoute: /s\n", "    pathRoute: /s\n    fetchHeaders:\n      x-hwid: \"${HW}\"\n"))
	h := l.Config.Subscriptions[0].FetchHeaders
	if h.Plain()["x-hwid"] != "s3cr3t" {
		t.Fatalf("headers %v", h.Plain())
	}
	js := marshal(t, l.Config)
	if !strings.Contains(js, `"fetchHeaders":{"x-hwid":"<redacted>"}`) || strings.Contains(js, "s3cr3t") {
		t.Fatalf("print-config leaks or misses headers: %s", js)
	}

	mustFail(t, replace(t, "    pathRoute: /s\n", "    pathRoute: /s\n    fetchHeaders: {User-Agent: x}\n"),
		"subscriptions.0.fetchHeaders.User-Agent: use fetchUserAgent instead")
	mustFail(t, replace(t, "    pathRoute: /s\n", "    pathRoute: /s\n    fetchHeaders: {x-n: 5}\n"),
		"subscriptions.0.fetchHeaders.x-n: expected string, got int")
	mustFail(t, replace(t, "    pathRoute: /s\n", "    pathRoute: /s\n    fetchHeaders: {Host: h, x-e: ''}\n"),
		"subscriptions.0.fetchHeaders.Host: header is not allowed",
		"subscriptions.0.fetchHeaders.x-e: must not be empty")

	empty := mustLoad(t, replace(t, "    pathRoute: /s\n", "    pathRoute: /s\n    fetchHeaders: {}\n")).Config
	if empty.Subscriptions[0].FetchHeaders != nil || strings.Contains(marshal(t, empty), "fetchHeaders") {
		t.Fatal("empty fetchHeaders must be treated as absent")
	}
}

func TestReadErrorHasPath(t *testing.T) {
	_, err := Load("nope.yml")
	if err == nil || !strings.HasPrefix(err.Error(), "failed to read config nope.yml: ") {
		t.Fatalf("err %v", err)
	}
	p := write(t, "")
	_, err = Load(p)
	if err == nil || !strings.HasPrefix(err.Error(), "invalid config "+p+":") || !strings.Contains(err.Error(), "  - subscriptions: required") {
		t.Fatalf("err %v", err)
	}
}

// An output id is the secret part of the public output URL: print-config
// hides it unless --include-secrets, and a duplicate-id error does not echo it.
func TestPrintedRedactsOutputIDs(t *testing.T) {
	l := mustLoad(t, replace(t, "      - id: out\n", "      - id: s3cr3t-out\n"))
	if js := marshal(t, l.Config.Printed(false)); strings.Contains(js, "s3cr3t-out") || !strings.Contains(js, `"id":"<redacted>"`) {
		t.Fatalf("print-config leaks the output id: %s", js)
	}
	if js := marshal(t, l.Config.Printed(true)); !strings.Contains(js, `"id":"s3cr3t-out"`) {
		t.Fatalf("--include-secrets misses the output id: %s", js)
	}
	if got := l.Config.Subscriptions[0].Outputs[0].ID; got != "s3cr3t-out" {
		t.Fatalf("loaded id = %q", got)
	}

	dup := "      - id: s3cr3t-out\n        labelIncludeRegex: x\n      - id: s3cr3t-out\n        labelIncludeRegex: y\n"
	msg := mustFail(t, strings.Replace(base, "      - id: out\n        labelIncludeRegex: '/Германия/i'\n", dup, 1),
		"subscriptions.0.outputs.1.id: duplicate output id")
	if strings.Contains(msg, "s3cr3t-out") {
		t.Fatalf("error echoes the output id: %s", msg)
	}
}
