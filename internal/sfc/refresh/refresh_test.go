package refresh

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/a1ekseev/scd/internal/common/inputsrc"
	"github.com/a1ekseev/scd/internal/sfc/cache"
	"github.com/a1ekseev/scd/internal/sfc/config"
)

const (
	repoRoot = "../../.."
	uuid1    = "00000000-0000-4000-8000-000000000001"
)

var fixedNow = time.Date(2026, 8, 15, 10, 0, 0, 0, time.UTC)

// Lines of the plain remote source (04 §11 test 17).
var (
	lineDE     = "vless://" + uuid1 + "@de.example.com:443?type=tcp&security=tls&sni=de.example.com#%F0%9F%87%A9%F0%9F%87%AA%20Berlin%2C%20Germany"
	lineNL     = "vless://" + uuid1 + "@nl.example.com:443?type=ws&security=tls&path=%2Fws#%F0%9F%87%B3%F0%9F%87%B1%20Amsterdam%2C%20Netherlands"
	lineTrojan = "trojan://secret@tr.example.com:443#Germany%20trojan"
	lineDE2    = "vless://" + uuid1 + "@fra.example.com:443?type=tcp&security=tls#%D0%A4%D1%80%D0%B0%D0%BD%D0%BA%D1%84%D1%83%D1%80%D1%82%2C%20Germany"
)

const baseConfig = `
subscriptions:
  - id: main
    input: https://sub.example.com/main
    pathRoute: /profiles
    outputs:
      - id: out-germany
        name: germany
        labelIncludeRegex: '/Germany/i'
        userAgent: [Clash, Stash]
        profileTitle: German Profile
        profileUpdateInterval: 6
      - id: out-europe
        labelIncludeRegex: '/Germany|Netherlands/'
      - id: out-off
        enabled: false
        labelIncludeRegex: '/.*/'
runtime:
  refreshSchedule: "*/10 * * * *"
server:
  listen: 127.0.0.1:8080
`

// loadConfig writes yaml to config.yml in a temp dir (the source cache is
// written next to it) and loads it.
func loadConfig(t *testing.T, yaml string) config.Loaded {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(p, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	l, err := config.Load(p)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	return l
}

// logRecorder collects JSON log records.
type logRecorder struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logRecorder) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logRecorder) events(t *testing.T, event string) []map[string]any {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []map[string]any
	for line := range strings.Lines(l.buf.String()) {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("bad log line %q: %v", line, err)
		}
		if rec["event"] == event {
			out = append(out, rec)
		}
	}
	return out
}

// source is a fake Input: it decodes content like a real source (through
// inputsrc.Load on stdin, so the configured format applies) or fails with
// err. It records the options of each call per input.
type source struct {
	mu      sync.Mutex
	content string
	err     error
	calls   map[string][]inputsrc.Options
}

func (s *source) set(content string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.content, s.err = content, err
}

func (s *source) load(ctx context.Context, input string, o inputsrc.Options) (inputsrc.Loaded, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.calls == nil {
		s.calls = map[string][]inputsrc.Options{}
	}
	s.calls[input] = append(s.calls[input], o)
	if s.err != nil {
		return inputsrc.Loaded{}, s.err
	}
	o.Stdin = strings.NewReader(s.content)
	l, err := inputsrc.Load(ctx, "-", o)
	l.Source = input
	return l, err
}

func newRefresher(t *testing.T, cfg config.Loaded, src *source) (*Refresher, *logRecorder) {
	t.Helper()
	rec := &logRecorder{}
	return &Refresher{
		Cfg:   cfg,
		State: NewState(cfg.Config),
		Log:   slog.New(slog.NewJSONHandler(rec, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Now:   func() time.Time { return fixedNow },
		Input: src.load,
	}, rec
}

func lookup(t *testing.T, st *State, path string) *OutputState {
	t.Helper()
	o, ok := st.Lookup(path)
	if !ok {
		t.Fatalf("Lookup(%q) found nothing", path)
	}
	return o
}

func reportByID(t *testing.T, rep Report, id string) OutputReport {
	t.Helper()
	for _, o := range rep.Outputs {
		if o.ID == id {
			return o
		}
	}
	t.Fatalf("no report for %q in %+v", id, rep.Outputs)
	return OutputReport{}
}

// TestRefreshPlainRemote is 04 §11 test 17.
func TestRefreshPlainRemote(t *testing.T) {
	cfg := loadConfig(t, baseConfig)
	src := &source{content: strings.Join([]string{lineDE, lineTrojan, lineNL}, "\n")}
	r, _ := newRefresher(t, cfg, src)

	rep := r.Run(t.Context())

	if n := len(src.calls["https://sub.example.com/main"]); n != 1 {
		t.Fatalf("input loaded %d times, want 1", n)
	}
	if rep.SourceCount != 1 || rep.OutputCount != 2 || rep.Successful != 2 || rep.Failed != 0 {
		t.Fatalf("report counts = %+v", rep)
	}
	if rep.RefreshedAt != "2026-08-15T10:00:00.000Z" {
		t.Fatalf("RefreshedAt = %q", rep.RefreshedAt)
	}
	de := lookup(t, r.State, "/profiles/out-germany")
	if de.LastGoodPlain != lineDE || de.LastGoodLineCount != 1 {
		t.Fatalf("out-germany plain = %q (%d)", de.LastGoodPlain, de.LastGoodLineCount)
	}
	if de.PathRoute != "/profiles" || de.Name != "germany" || !slices.Equal(de.UserAgent, []string{"Clash", "Stash"}) ||
		de.ProfileTitle != "German Profile" || de.ProfileUpdateInterval != 6 || de.SubscriptionID != "main" {
		t.Fatalf("out-germany config fields = %+v", de)
	}
	if !de.LastSuccessAt.Equal(fixedNow) || !de.LastRefreshAt.Equal(fixedNow) || de.LastError != "" {
		t.Fatalf("out-germany times/error = %+v", de)
	}
	eu := lookup(t, r.State, "/profiles/out-europe")
	if want := lineDE + "\n" + lineNL; eu.LastGoodPlain != want {
		t.Fatalf("out-europe plain = %q, want %q", eu.LastGoodPlain, want)
	}
	if got, _ := base64.StdEncoding.DecodeString(eu.LastGoodBase64); string(got) != eu.LastGoodPlain {
		t.Fatalf("out-europe base64 does not decode to plain: %q", got)
	}
}

// TestRefreshBase64Auto is 04 §11 test 18.
func TestRefreshBase64Auto(t *testing.T) {
	cfg := loadConfig(t, baseConfig)
	src := &source{content: base64.StdEncoding.EncodeToString([]byte(lineDE))}
	r, _ := newRefresher(t, cfg, src)

	rep := r.Run(t.Context())

	if got := reportByID(t, rep, "out-germany"); !got.OK || got.MatchedLines != 1 {
		t.Fatalf("out-germany report = %+v", got)
	}
	de := lookup(t, r.State, "/profiles/out-germany")
	dec, err := base64.StdEncoding.DecodeString(de.LastGoodBase64)
	if err != nil || string(dec) != lineDE {
		t.Fatalf("output base64 decodes to %q, %v; want %q", dec, err, lineDE)
	}
}

// TestRefreshWritesCacheAndFallsBack is 04 §11 test 19.
func TestRefreshWritesCacheAndFallsBack(t *testing.T) {
	cfg := loadConfig(t, baseConfig)
	plain := lineDE + "\n" + lineNL
	src := &source{content: base64.StdEncoding.EncodeToString([]byte(plain))}
	r, rec := newRefresher(t, cfg, src)
	r.Run(t.Context())

	cachePath := filepath.Join(cfg.Dir, ".sfc-cache", "main.subscription")
	b, err := os.ReadFile(cachePath)
	if err != nil || string(b) != plain {
		t.Fatalf("cache = %q, %v; want decoded content", b, err)
	}
	if ev := rec.events(t, "subscription_source_cache_written"); len(ev) != 1 || ev[0]["path"] != cachePath {
		t.Fatalf("cache_written logs = %v", ev)
	}
	before := lookup(t, r.State, "/profiles/out-europe")

	src.set("", errors.New("network down"))
	rep := r.Run(t.Context())

	if rep.Successful != 2 || rep.Failed != 0 {
		t.Fatalf("report = %+v", rep)
	}
	for _, o := range rep.Outputs {
		if !o.UsedSourceCache || !o.OK || o.Error != "" {
			t.Fatalf("output report = %+v, want ok from source cache", o)
		}
	}
	after := lookup(t, r.State, "/profiles/out-europe")
	if after.LastGoodBase64 != before.LastGoodBase64 || after.LastError != "" {
		t.Fatalf("state after cache fallback = %+v", after)
	}
	ev := rec.events(t, "subscription_source_cache_used")
	if len(ev) != 1 || ev[0]["path"] != cachePath || ev[0]["error"] != "network down" ||
		ev[0]["parsedEntries"] != float64(2) || ev[0]["level"] != "WARN" {
		t.Fatalf("cache_used logs = %v", ev)
	}
	js, err := json.Marshal(rep.Outputs[0])
	if err != nil || !strings.Contains(string(js), `"usedSourceCache":true`) {
		t.Fatalf("report JSON = %s, %v", js, err)
	}
}

// TestRefreshNoCacheFails is 04 §11 test 20.
func TestRefreshNoCacheFails(t *testing.T) {
	cfg := loadConfig(t, baseConfig)
	src := &source{err: errors.New("network down")}
	r, rec := newRefresher(t, cfg, src)

	rep := r.Run(t.Context())

	if rep.Failed != 2 || rep.Successful != 0 {
		t.Fatalf("report = %+v", rep)
	}
	for _, o := range rep.Outputs {
		if o.UsedSourceCache || o.OK || o.UsedCachedValue || o.Error != "network down" {
			t.Fatalf("output report = %+v", o)
		}
	}
	if st := lookup(t, r.State, "/profiles/out-germany"); !strings.Contains(st.LastError, "network down") ||
		!st.LastFailureAt.Equal(fixedNow) || st.LastGoodBase64 != "" {
		t.Fatalf("state = %+v", st)
	}
	missed := rec.events(t, "subscription_source_cache_missed")
	if len(missed) != 1 {
		t.Fatalf("cache_missed logs = %v", missed)
	}
	m := missed[0]
	if m["level"] != "ERROR" || m["subscriptionId"] != "main" || m["cachePath"] != cache.Path(cfg.Path, "main") ||
		!strings.Contains(m["cacheError"].(string), "no such file") || !strings.Contains(m["error"].(string), "network down") {
		t.Fatalf("cache_missed log = %v", m)
	}
	if ev := rec.events(t, "subscription_refresh_failed"); len(ev) != 1 || ev[0]["error"] != "network down" {
		t.Fatalf("refresh_failed logs = %v", ev)
	}
	js, _ := json.Marshal(rep.Outputs[0])
	if strings.Contains(string(js), "usedSourceCache") {
		t.Fatalf("report JSON has usedSourceCache: %s", js)
	}
}

// TestRefreshEmptyFilterKeepsLastGood is 04 §11 test 21.
func TestRefreshEmptyFilterKeepsLastGood(t *testing.T) {
	cfg := loadConfig(t, baseConfig)
	src := &source{content: lineDE}
	r, _ := newRefresher(t, cfg, src)
	r.Run(t.Context())
	before := lookup(t, r.State, "/profiles/out-germany")

	src.set(lineNL, nil)
	rep := r.Run(t.Context())

	got := reportByID(t, rep, "out-germany")
	if got.OK || !got.UsedCachedValue || got.Error != "Filtered output is empty." || got.MatchedLines != 0 {
		t.Fatalf("out-germany report = %+v", got)
	}
	after := lookup(t, r.State, "/profiles/out-germany")
	if after.LastGoodBase64 != before.LastGoodBase64 || after.LastGoodPlain != lineDE ||
		!strings.Contains(after.LastError, "empty") {
		t.Fatalf("state = %+v", after)
	}
	if _, content, err := cache.Read(cfg.Path, "main"); err != nil || content != lineNL {
		t.Fatalf("cache = %q, %v; want the new source", content, err)
	}
}

// TestRefreshReplacesStateAtomically is 04 §11 test 24.
func TestRefreshReplacesStateAtomically(t *testing.T) {
	cfg := loadConfig(t, baseConfig)
	src := &source{content: lineDE}
	r, _ := newRefresher(t, cfg, src)
	r.Run(t.Context())
	r.State.mu.RLock()
	first := r.State.outputs["out-germany"]
	r.State.mu.RUnlock()

	src.set(lineDE2, nil)
	r.Run(t.Context())

	r.State.mu.RLock()
	second := r.State.outputs["out-germany"]
	r.State.mu.RUnlock()
	if first == second {
		t.Fatal("state object was mutated in place, want a new one")
	}
	if first.LastGoodPlain != lineDE {
		t.Fatalf("old state changed: %q", first.LastGoodPlain)
	}
	st := lookup(t, r.State, "/profiles/out-germany")
	if st.LastGoodPlain != lineDE2 || !strings.Contains(st.LastGoodPlain, "%D0%A4%D1%80") {
		t.Fatalf("plain = %q, want the Франкфурт line", st.LastGoodPlain)
	}
	if st.PathRoute != "/profiles" || st.ProfileTitle != "German Profile" || st.ProfileUpdateInterval != 6 ||
		!slices.Equal(st.UserAgent, []string{"Clash", "Stash"}) {
		t.Fatalf("config fields lost: %+v", st)
	}
}

// TestRefreshMalformedLabelSkipsLineOnly: a line with a malformed label
// (#100%) is dropped, the subscription does not fail (O11).
func TestRefreshMalformedLabelSkipsLineOnly(t *testing.T) {
	cfg := loadConfig(t, baseConfig)
	bad := "vless://" + uuid1 + "@bad.example.com:443?type=tcp#100% Germany"
	src := &source{content: bad + "\n" + lineDE}
	r, _ := newRefresher(t, cfg, src)

	rep := r.Run(t.Context())

	if got := reportByID(t, rep, "out-germany"); !got.OK || got.MatchedLines != 1 {
		t.Fatalf("report = %+v", got)
	}
	if st := lookup(t, r.State, "/profiles/out-germany"); st.LastGoodPlain != lineDE {
		t.Fatalf("plain = %q", st.LastGoodPlain)
	}
}

// TestRefreshUsesSharedParamWhitelist: links with parameters TS-sfc dropped
// (3x-ui REALITY spx/pqv/support-x25519mlkem768, xhttp extra/fm), unknown
// parameters (D28) and a non-UUID id (D18) reach the output unchanged.
func TestRefreshUsesSharedParamWhitelist(t *testing.T) {
	cfg := loadConfig(t, `
subscriptions:
  - id: wl
    input: https://sub.example.com/wl
    pathRoute: /s
    outputs:
      - id: all
        labelIncludeRegex: '/.*/'
runtime:
  refreshSchedule: "*/10 * * * *"
server:
  listen: 127.0.0.1:8080
`)
	lines := []string{
		"vless://" + uuid1 + "@r.example.com:443?type=tcp&security=reality&encryption=none&flow=xtls-rprx-vision" +
			"&sni=r.example.com&fp=chrome&pbk=PUBKEY&sid=ab12&spx=%2F&pqv=PQVKEY&support-x25519mlkem768=true#Reality",
		"vless://" + uuid1 + "@x.example.com:443?type=xhttp&security=tls&path=%2Fxh&mode=auto" +
			"&extra=%7B%22xPaddingBytes%22%3A%22100-1000%22%7D&fm=%7B%7D#XHTTP",
		"vless://" + uuid1 + "@f.example.com:443?type=tcp&security=tls&foo=1#Unknown",
		"vless://my-user@u.example.com:443?type=tcp&security=tls#NotUUID",
	}
	src := &source{content: strings.Join(append(lines[:2:2], append([]string{"trojan://x"}, lines[2:]...)...), "\n")}
	r, rec := newRefresher(t, cfg, src)

	rep := r.Run(t.Context())

	if got := reportByID(t, rep, "all"); !got.OK || got.MatchedLines != 4 {
		t.Fatalf("report = %+v", got)
	}
	if st := lookup(t, r.State, "/s/all"); st.LastGoodPlain != strings.Join(lines, "\n") {
		t.Fatalf("plain = %q", st.LastGoodPlain)
	}
	ev := rec.events(t, "subscription_line_param_ignored")
	if len(ev) != 1 {
		t.Fatalf("param_ignored logs = %v, want exactly one", ev)
	}
	if e := ev[0]; e["level"] != "DEBUG" || e["subscriptionId"] != "wl" || e["line"] != float64(4) ||
		!reflect.DeepEqual(e["params"], []any{"foo"}) {
		t.Fatalf("param_ignored log = %v", e)
	}
}

// TestRefreshPassesUserAgent: fetchUserAgent and fetchHeaders reach the
// loader (D27 O9, D33); without them the loader default applies.
func TestRefreshPassesUserAgent(t *testing.T) {
	cfg := loadConfig(t, `
subscriptions:
  - id: with
    input: https://sub.example.com/with
    format: plain
    fetchTimeoutMs: 1234
    fetchUserAgent: Happ/1.0
    fetchHeaders:
      x-hwid: secret-hwid
    pathRoute: /a
    outputs:
      - id: a
        labelIncludeRegex: '/.*/'
  - id: without
    input: https://sub.example.com/without
    pathRoute: /b
    outputs:
      - id: b
        labelIncludeRegex: '/.*/'
runtime:
  refreshSchedule: "*/10 * * * *"
server:
  listen: 127.0.0.1:8080
`)
	src := &source{content: lineDE}
	r, rec := newRefresher(t, cfg, src)
	r.Run(t.Context())

	with := src.calls["https://sub.example.com/with"]
	if len(with) != 1 {
		t.Fatalf("calls = %v", src.calls)
	}
	if o := with[0]; o.UserAgent != "Happ/1.0" || !maps.Equal(o.Headers.Plain(), map[string]string{"x-hwid": "secret-hwid"}) ||
		o.Format != "plain" || o.FetchTimeout != 1234*time.Millisecond {
		t.Fatalf("options = %+v", o)
	}
	without := src.calls["https://sub.example.com/without"]
	if len(without) != 1 {
		t.Fatalf("calls = %v", src.calls)
	}
	if o := without[0]; o.UserAgent != "" || o.Headers != nil || o.Format != "auto" || o.FetchTimeout != 5*time.Second {
		t.Fatalf("options = %+v", o)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if strings.Contains(rec.buf.String(), "secret-hwid") {
		t.Fatal("fetchHeaders value leaked into logs")
	}
}

func TestReportSortedByID(t *testing.T) {
	cfg := loadConfig(t, `
subscriptions:
  - id: main
    input: https://sub.example.com/main
    pathRoute: /s
    outputs:
      - id: b
        labelIncludeRegex: '/.*/'
      - id: a
        labelIncludeRegex: '/.*/'
      - id: _a
        labelIncludeRegex: '/.*/'
      - id: Z1
        labelIncludeRegex: '/.*/'
runtime:
  refreshSchedule: "*/10 * * * *"
server:
  listen: 127.0.0.1:8080
`)
	r, _ := newRefresher(t, cfg, &source{content: lineDE})
	rep := r.Run(t.Context())
	var ids []string
	for _, o := range rep.Outputs {
		ids = append(ids, o.ID)
	}
	if want := []string{"Z1", "_a", "a", "b"}; !slices.Equal(ids, want) {
		t.Fatalf("ids = %v, want %v", ids, want)
	}
}

// TestNewStateRegistersDisabledOutputs: disabled outputs and outputs of
// disabled subscriptions are known (served as 502, not 404) and keyed by
// the escaped id.
func TestNewStateRegistersDisabledOutputs(t *testing.T) {
	cfg := loadConfig(t, `
subscriptions:
  - id: on
    input: https://sub.example.com/on
    pathRoute: /s
    outputs:
      - id: x+y
        labelIncludeRegex: '/.*/'
      - id: off
        enabled: false
        labelIncludeRegex: '/.*/'
  - id: disabled
    enabled: false
    input: https://sub.example.com/off
    pathRoute: /d
    outputs:
      - id: d
        labelIncludeRegex: '/.*/'
runtime:
  refreshSchedule: "*/10 * * * *"
server:
  listen: 127.0.0.1:8080
`)
	st := NewState(cfg.Config)
	for _, p := range []string{"/s/x%2By", "/s/off", "/d/d"} {
		if o := lookup(t, st, p); o.LastGoodBase64 != "" {
			t.Fatalf("%s has a payload before refresh", p)
		}
	}
	for _, p := range []string{"/s/x+y", "/s/d", "/d/off"} {
		if _, ok := st.Lookup(p); ok {
			t.Fatalf("Lookup(%q) found an output", p)
		}
	}
	r, _ := newRefresher(t, cfg, &source{content: lineDE})
	if rep := r.Run(t.Context()); rep.SourceCount != 1 || rep.OutputCount != 1 {
		t.Fatalf("report = %+v", rep)
	}
}

// copyConfig copies a repo config into a temp dir (the source cache is
// written next to the copy, never into docker/sfc), applying replacements.
func copyConfig(t *testing.T, src string, replace ...string) config.Loaded {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repoRoot, src))
	if err != nil {
		t.Fatal(err)
	}
	text := string(b)
	for i := 0; i < len(replace); i += 2 {
		if strings.Count(text, replace[i]) != 1 {
			t.Fatalf("%s: %q must occur once", src, replace[i])
		}
		text = strings.Replace(text, replace[i], replace[i+1], 1)
	}
	return loadConfig(t, text)
}

func absFixture(t *testing.T, rel string) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join(repoRoot, rel))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// fileSource is a fake Input that loads the file *path with the requested
// options, whatever the configured input is.
func fileSource(path *string) func(context.Context, string, inputsrc.Options) (inputsrc.Loaded, error) {
	return func(ctx context.Context, _ string, o inputsrc.Options) (inputsrc.Loaded, error) {
		return inputsrc.Load(ctx, *path, o)
	}
}

// reportJSON round-trips v through JSON for a semantic comparison.
func reportJSON(t *testing.T, v any) any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// redactReportIDs applies the one Go-only difference to a TS reference
// report: output ids are printed as "<redacted>" (they are secrets).
func redactReportIDs(doc any) any {
	for _, o := range doc.(map[string]any)["outputs"].([]any) {
		o.(map[string]any)["id"] = config.RedactedID
	}
	return doc
}

func readJSON(t *testing.T, rel string) any {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repoRoot, rel))
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestRefreshDockerExampleMatchesReference compares a refresh of
// docker/sfc/config.example.yml with the TS reference (task 0.3).
func TestRefreshDockerExampleMatchesReference(t *testing.T) {
	fixture := absFixture(t, "docker/fixtures/subscription.b64")
	cfg := copyConfig(t, "docker/sfc/config.example.yml",
		"input: ../fixtures/subscription.b64", "input: "+fixture)
	r, _ := newRefresher(t, cfg, nil)
	r.Input = fileSource(&fixture)

	rep := r.Run(t.Context())

	want := redactReportIDs(readJSON(t, "testdata/reference/sfc/refresh-docker-example.json"))
	if got := reportJSON(t, rep); !reflect.DeepEqual(got, want) {
		t.Fatalf("report:\n got %v\nwant %v", got, want)
	}
	for _, id := range []string{"test-all", "test-de", "test-nl"} {
		st := lookup(t, r.State, "/s/"+id)
		b64, err := os.ReadFile(filepath.Join(repoRoot, "testdata/reference/sfc/outputs", id+".b64"))
		if err != nil {
			t.Fatal(err)
		}
		if st.LastGoodBase64 != string(b64) {
			t.Errorf("%s base64 = %q, want %q", id, st.LastGoodBase64, b64)
		}
		plain, err := os.ReadFile(filepath.Join(repoRoot, "testdata/reference/sfc/outputs", id+".plain.txt"))
		if err != nil {
			t.Fatal(err)
		}
		if st.LastGoodPlain != string(plain) {
			t.Errorf("%s plain = %q, want %q", id, st.LastGoodPlain, plain)
		}
	}
}

// allowedDiff is an intended difference from a TS reference value at a
// JSON path ("outputs.0.ok"); a nil goWant means the key is absent.
type allowedDiff struct {
	path       string
	ts, goWant any
	reason     string
}

func applyDiffs(t *testing.T, doc any, diffs []allowedDiff) {
	t.Helper()
	for _, d := range diffs {
		keys := strings.Split(d.path, ".")
		node := doc
		for _, k := range keys[:len(keys)-1] {
			switch n := node.(type) {
			case map[string]any:
				node = n[k]
			case []any:
				idx, err := strconv.Atoi(k)
				if err != nil || idx < 0 || idx >= len(n) {
					t.Fatalf("bad index %q in %s", k, d.path)
				}
				node = n[idx]
			}
		}
		m, ok := node.(map[string]any)
		if !ok {
			t.Fatalf("%s: parent is not an object", d.path)
		}
		last := keys[len(keys)-1]
		if !reflect.DeepEqual(m[last], d.ts) {
			t.Fatalf("%s: TS reference has %v, allowedDiffs expects %v", d.path, m[last], d.ts)
		}
		if d.goWant == nil {
			delete(m, last)
		} else {
			m[last] = d.goWant
		}
	}
}

// TestRefreshRealShapes is 12 §9 item 11: the Go form of the owner's sfc
// config over the anonymized real subscriptions, compared with the TS
// reference (testdata/reference/sfc/real-shapes.json).
func TestRefreshRealShapes(t *testing.T) {
	cfg := copyConfig(t, "docker/sfc/config.real-shape.go.yml")
	var fixture string
	r, _ := newRefresher(t, cfg, nil)
	r.Input = fileSource(&fixture)
	ref := readJSON(t, "testdata/reference/sfc/real-shapes.json").(map[string]any)

	const reason = "D27 O7 / D28: shared parameter whitelist, xhttp/mode/extra lines are no longer dropped"
	tests := []struct {
		name, fixture string
		diffs         []allowedDiff
	}{
		{"provider2", "docker/fixtures/real-shapes/provider2.b64", nil},
		{"remnawave", "docker/fixtures/real-shapes/remnawave.txt", []allowedDiff{
			{"successful", 0.0, 1.0, reason},
			{"failed", 3.0, 2.0, reason},
			{"outputs.0.matchedLines", 0.0, 4.0, reason},
			{"outputs.0.ok", false, true, reason},
			{"outputs.0.error", "Filtered output is empty.", nil, reason},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture = absFixture(t, tt.fixture)
			rep := r.Run(t.Context())
			want := redactReportIDs(ref[tt.name])
			applyDiffs(t, want, tt.diffs)
			if got := reportJSON(t, rep); !reflect.DeepEqual(got, want) {
				t.Fatalf("report:\n got %v\nwant %v", got, want)
			}
		})
	}
	if _, err := os.Stat(cache.Path(cfg.Path, "main")); err != nil {
		t.Fatalf("source cache not written next to the config copy: %v", err)
	}
}

// Final review I1 (deferred 3.4): a cancelled refresh loads no further
// subscription.
func TestRefreshStopsAfterCancel(t *testing.T) {
	cfg := loadConfig(t, `
subscriptions:
  - id: a
    input: https://sub.example.com/a
    pathRoute: /a
    outputs:
      - id: out-a
        labelIncludeRegex: '/Germany/'
  - id: b
    input: https://sub.example.com/b
    pathRoute: /b
    outputs:
      - id: out-b
        labelIncludeRegex: '/Germany/'
runtime:
  refreshSchedule: "@hourly"
server:
  listen: 127.0.0.1:8080
`)
	src := &source{content: lineDE}
	r, _ := newRefresher(t, cfg, src)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	r.Input = func(c context.Context, input string, o inputsrc.Options) (inputsrc.Loaded, error) {
		cancel()
		return src.load(c, input, o)
	}

	rep := r.Run(ctx)

	if _, ok := src.calls["https://sub.example.com/b"]; ok {
		t.Errorf("subscription b was loaded after the cancel")
	}
	if len(rep.Outputs) != 1 || rep.Outputs[0].ID != "out-a" {
		t.Errorf("outputs = %+v, want only out-a", rep.Outputs)
	}
	if rep.Canceled != PhaseSubscriptions {
		t.Errorf("Canceled = %q, want %q", rep.Canceled, PhaseSubscriptions)
	}
}

// Final review I2: the subscription URL (a token in the path and query) never
// reaches the logs or the report (D33, like scd ruling 6.1).
func TestRefreshDoesNotLeakSubscriptionURL(t *testing.T) {
	const secret, qsecret = "PATHSECRET123", "QUERYSECRET456"
	var fail atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if fail.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(w, lineDE)
	}))
	defer srv.Close()
	input := srv.URL + "/sub/" + secret + "?token=" + qsecret

	cfg := loadConfig(t, `
subscriptions:
  - id: main
    input: `+input+`
    pathRoute: /p
    outputs:
      - id: out
        labelIncludeRegex: '/Germany/'
runtime:
  refreshSchedule: "@hourly"
server:
  listen: 127.0.0.1:8080
`)
	rec := &logRecorder{}
	r := &Refresher{
		Cfg: cfg, State: NewState(cfg.Config), Now: func() time.Time { return fixedNow },
		Log: slog.New(slog.NewJSONHandler(rec, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
	check := func(stage string, rep Report) {
		t.Helper()
		b, err := json.Marshal(rep)
		if err != nil {
			t.Fatal(err)
		}
		rec.mu.Lock()
		logs := rec.buf.String()
		rec.mu.Unlock()
		st, _ := json.Marshal(r.State.get("out"))
		for _, s := range []string{secret, qsecret} {
			for name, text := range map[string]string{"logs": logs, "report": string(b), "state": string(st)} {
				if strings.Contains(text, s) {
					t.Errorf("%s: %s contain %s:\n%s", stage, name, s, text)
				}
			}
		}
	}

	rep := r.Run(t.Context()) // success: subscription_refreshed.source
	if rep.Failed != 0 || len(rec.events(t, "subscription_refreshed")) != 1 {
		t.Fatalf("first refresh: %+v", rep)
	}
	check("success", rep)

	fail.Store(true) // load failure with the source cache
	rep = r.Run(t.Context())
	if len(rec.events(t, "subscription_source_cache_used")) != 1 {
		t.Fatalf("cache not used: %+v", rep)
	}
	check("cache", rep)

	if err := os.RemoveAll(cache.Dir(cfg.Path)); err != nil {
		t.Fatal(err)
	}
	rep = r.Run(t.Context()) // load failure without the cache: report error
	if rep.Failed != 1 || !strings.Contains(rep.Outputs[0].Error, "/<redacted>") {
		t.Fatalf("failed refresh: %+v", rep)
	}
	check("failure", rep)
}

// Output ids are the secret part of the public output URLs: the printed
// report and the logs name outputs instead (named, else "<redacted>").
func TestOutputIDsStayOutOfReportAndLogs(t *testing.T) {
	src := &source{}
	src.set(lineDE+"\n", nil)
	r, rec := newRefresher(t, loadConfig(t, baseConfig), src)

	rep := r.Run(t.Context())

	b, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	rec.mu.Lock()
	logs := rec.buf.String()
	rec.mu.Unlock()
	for _, id := range []string{"out-germany", "out-europe", "out-off"} {
		if strings.Contains(string(b), id) || strings.Contains(logs, id) {
			t.Errorf("output id %s leaked:\nreport %s\nlogs %s", id, b, logs)
		}
	}
	ev := rec.events(t, "subscription_refreshed")
	if len(ev) != 1 || !reflect.DeepEqual(ev[0]["outputs"], []any{"germany", "<redacted>"}) {
		t.Fatalf("subscription_refreshed = %v, want outputs [germany <redacted>]", ev)
	}
	if rep.Outputs[0].ID == config.RedactedID {
		t.Fatalf("the in-memory report must keep the real id: %+v", rep.Outputs[0])
	}
}

// Final review I1: a load cut short by the cancellation fails no output and
// keeps the state.
func TestRefreshCanceledLoadIsNotAFailure(t *testing.T) {
	cfg := loadConfig(t, baseConfig)
	src := &source{content: lineDE}
	r, rec := newRefresher(t, cfg, src)
	r.Run(t.Context())
	before := r.State.get("out-germany")

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	r.Input = func(c context.Context, _ string, _ inputsrc.Options) (inputsrc.Loaded, error) {
		cancel()
		return inputsrc.Loaded{}, c.Err()
	}
	rep := r.Run(ctx)
	if rep.Failed != 0 || len(rep.Outputs) != 0 || rep.Canceled != PhaseLoad {
		t.Errorf("report = %+v", rep)
	}
	if after := r.State.get("out-germany"); after.LastError != "" || after.LastRefreshAt != before.LastRefreshAt {
		t.Errorf("state changed: %+v", after)
	}
	if n := len(rec.events(t, "subscription_refresh_failed")) + len(rec.events(t, "subscription_source_cache_used")) +
		len(rec.events(t, "subscription_refresh_canceled")); n != 0 {
		t.Errorf("failure events = %d", n)
	}
}
