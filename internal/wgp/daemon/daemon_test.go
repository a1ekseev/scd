package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/a1ekseev/scd/internal/common/candidate"
	"github.com/a1ekseev/scd/internal/common/wgpstatus"
	"github.com/a1ekseev/scd/internal/wgp/config"
	"github.com/a1ekseev/scd/internal/wgp/provider"
)

// scripted returns the next result of its script on every tick.
type scripted struct {
	lines []string
	errs  []error
	n     int
	state string
}

func (*scripted) ID() string           { return "p" }
func (*scripted) Kind() candidate.Kind { return candidate.KindWireGuard }
func (s *scripted) Tick(context.Context, provider.Store) (provider.Result, error) {
	err := s.errs[s.n]
	s.n++
	return provider.Result{Lines: s.lines, Status: wgpstatus.Provider{Issued: len(s.lines), State: s.state}}, err
}

// A provider error goes into its status, the previous file stays, and
// lastSuccessAt keeps the time of the last good tick.
func TestTickStatus(t *testing.T) {
	b, err := os.ReadFile("../../../docker/fixtures/wireguard/candidates.txt")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	p := &scripted{lines: strings.Split(string(b), "\n")[:1], errs: []error{nil, errors.New("api: 503")}}
	cfg := config.Config{
		Providers: []config.Provider{{ID: "p", Type: "x", Enabled: true, Schedule: "@hourly", Impl: p}},
		Outputs:   []config.Output{{ID: "o", Path: filepath.Join(dir, "o"), Provider: "p"}},
	}
	d, err := New(cfg, func(string) provider.Store { return nil }, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	d.tick(t.Context(), d.jobs[0])
	first := d.Status().Providers[0]
	if first.State != wgpstatus.StateOK || first.ID != "p" || first.Output != "o" || first.Type != "x" || first.Issued != 1 || first.LastSuccessAt.IsZero() {
		t.Fatalf("first %+v", first)
	}
	d.tick(t.Context(), d.jobs[0])
	st, err := wgpstatus.Read(dir, 1<<16)
	if err != nil {
		t.Fatal(err)
	}
	second := st.Providers[0]
	if second.State != wgpstatus.StateError || second.Error != "api: 503" || !second.LastSuccessAt.Equal(first.LastSuccessAt) || second.LastTickAt.Before(first.LastTickAt) {
		t.Fatalf("second %+v", second)
	}
	if got, _ := os.ReadFile(cfg.Outputs[0].Path); string(got) != p.lines[0]+"\n" {
		t.Fatalf("output %q", got)
	}

	// A tick cancelled by shutdown records nothing.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	before := d.Status().Providers[0]
	d.tick(ctx, d.jobs[0])
	if after := d.Status().Providers[0]; p.n != 2 || after.LastTickAt != before.LastTickAt {
		t.Fatalf("cancelled tick ran %d, status %+v", p.n, after)
	}
}

// A failed tick that reports needs_login keeps that state and its error,
// and the previous file stays.
func TestTickNeedsLogin(t *testing.T) {
	b, err := os.ReadFile("../../../docker/fixtures/wireguard/candidates.txt")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	p := &scripted{lines: strings.Split(string(b), "\n")[:1], errs: []error{nil, errors.New("session rejected")}}
	out := filepath.Join(dir, "o")
	cfg := config.Config{
		Providers: []config.Provider{{ID: "p", Type: "x", Enabled: true, Schedule: "@hourly", Impl: p}},
		Outputs:   []config.Output{{ID: "o", Path: out, Provider: "p"}},
	}
	d, err := New(cfg, func(string) provider.Store { return nil }, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	d.tick(t.Context(), d.jobs[0])
	p.lines, p.state = nil, wgpstatus.StateNeedsLogin
	d.tick(t.Context(), d.jobs[0])
	st, err := wgpstatus.Read(dir, 1<<16)
	if err != nil {
		t.Fatal(err)
	}
	if s := st.Providers[0]; s.State != wgpstatus.StateNeedsLogin || s.Error != "session rejected" || s.LastSuccessAt.IsZero() {
		t.Fatalf("status %+v", s)
	}
	if got, _ := os.ReadFile(out); len(got) == 0 {
		t.Fatal("a needs_login tick emptied the file")
	}
}

// No lines and no error: the output empties (no live candidate); an error
// with no lines keeps the file.
func TestTickNoLines(t *testing.T) {
	b, err := os.ReadFile("../../../docker/fixtures/wireguard/candidates.txt")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	p := &scripted{lines: strings.Split(string(b), "\n")[:1], errs: []error{nil, errors.New("api: 503"), nil}}
	out := filepath.Join(dir, "o")
	cfg := config.Config{
		Providers: []config.Provider{{ID: "p", Type: "x", Enabled: true, Schedule: "@hourly", Impl: p}},
		Outputs:   []config.Output{{ID: "o", Path: out, Provider: "p"}},
	}
	d, err := New(cfg, func(string) provider.Store { return nil }, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	d.tick(t.Context(), d.jobs[0])
	p.lines = nil
	d.tick(t.Context(), d.jobs[0])
	if s := d.Status().Providers[0]; s.State != wgpstatus.StateError || s.Error != "api: 503" {
		t.Fatalf("want the tick error: %+v", s)
	}
	if got, _ := os.ReadFile(out); len(got) == 0 {
		t.Fatal("a failed tick emptied the file")
	}
	d.tick(t.Context(), d.jobs[0])
	info, err := os.Stat(out)
	if err != nil || info.Size() != 0 || info.Mode().Perm() != 0o600 {
		t.Fatalf("output %v %v", info, err)
	}
	if s := d.Status().Providers[0]; s.State != wgpstatus.StateOK || s.Issued != 0 {
		t.Fatalf("status %+v", s)
	}
}

// Disabled providers have no loop.
func TestNewSkipsDisabled(t *testing.T) {
	cfg := config.Config{
		Providers: []config.Provider{{ID: "p", Enabled: false, Schedule: "@hourly", Impl: &scripted{}}},
		Outputs:   []config.Output{{ID: "o", Path: "/x/o", Provider: "p"}},
	}
	d, err := New(cfg, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil || len(d.jobs) != 0 {
		t.Fatalf("%v, %d jobs", err, len(d.jobs))
	}
}

// funcProvider runs f on every tick.
type funcProvider struct {
	f func(context.Context) (provider.Result, error)
}

func (funcProvider) ID() string           { return "p" }
func (funcProvider) Kind() candidate.Kind { return candidate.KindWireGuard }
func (p funcProvider) Tick(ctx context.Context, _ provider.Store) (provider.Result, error) {
	return p.f(ctx)
}

// fixtureLine is the first line of the wireguard candidate fixture.
func fixtureLine(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("../../../docker/fixtures/wireguard/candidates.txt")
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(string(b), "\n")[0]
}

// logDaemon returns a daemon of one provider logging JSON at debug into buf.
func logDaemon(t *testing.T, impl provider.Provider) (*Daemon, *bytes.Buffer, string) {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Config{
		Providers: []config.Provider{{ID: "p", Type: "x", Enabled: true, Schedule: "@hourly", Impl: impl}},
		Outputs:   []config.Output{{ID: "o", Path: filepath.Join(dir, "o"), Provider: "p"}},
	}
	var buf bytes.Buffer
	d, err := New(cfg, func(string) provider.Store { return nil }, slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	if err != nil {
		t.Fatal(err)
	}
	return d, &buf, dir
}

// logLines decodes the JSON log lines of buf.
func logLines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for line := range strings.Lines(buf.String()) {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("log line %q: %v", line, err)
		}
		out = append(out, m)
	}
	return out
}

// levels returns "LEVEL event" of every line except the given events.
func levels(lines []map[string]any, skip ...string) []string {
	var out []string
	for _, l := range lines {
		if ev, _ := l["event"].(string); !slices.Contains(skip, ev) {
			out = append(out, fmt.Sprintf("%s %s", l["level"], ev))
		}
	}
	return out
}

// A repeated tick error is logged once at error, then at debug; the next
// good tick logs provider_recovered. Every line carries the ids.
func TestTickFailureEdge(t *testing.T) {
	line := fixtureLine(t)
	errs := []error{errors.New("api: 503"), errors.New("api: 503"), errors.Join(errors.New("a"), errors.New("b")), nil}
	n := 0
	d, buf, _ := logDaemon(t, funcProvider{func(context.Context) (provider.Result, error) {
		n++
		return provider.Result{Lines: []string{line}}, errs[n-1]
	}})
	for range errs {
		d.tick(t.Context(), d.jobs[0])
	}
	lines := logLines(t, buf)
	want := []string{"ERROR provider_tick_failed", "DEBUG provider_tick_failed", "ERROR provider_tick_failed", "INFO output_published", "INFO provider_recovered"}
	if got := levels(lines); !slices.Equal(got, want) {
		t.Fatalf("log %q, want %q", got, want)
	}
	for _, l := range lines {
		if l["providerId"] != "p" || l["providerType"] != "x" || l["outputId"] != "o" {
			t.Errorf("ids of %v", l)
		}
	}
	if l := lines[2]; l["state"] != "error" || fmt.Sprint(l["causes"]) != "[a b]" {
		t.Errorf("joined failure %v", l)
	}
	if l := lines[4]; l["previousState"] != "error" {
		t.Errorf("recovered %v", l)
	}
}

// Warnings and provider_error are logged at their level when they change,
// else at debug.
func TestTickWarningAndErrorEdge(t *testing.T) {
	line := fixtureLine(t)
	results := []provider.Result{
		{Warnings: []string{"w1", "w2"}, Status: wgpstatus.Provider{State: wgpstatus.StateError, Error: "file a rejected"}},
		{Warnings: []string{"w1", "w2"}, Status: wgpstatus.Provider{State: wgpstatus.StateError, Error: "file a rejected"}},
		{Warnings: []string{"w1"}, Status: wgpstatus.Provider{State: wgpstatus.StateError, Error: "file b rejected"}},
		{},
	}
	n := 0
	d, buf, _ := logDaemon(t, funcProvider{func(context.Context) (provider.Result, error) {
		r := results[n]
		n++
		r.Lines = []string{line}
		return r, nil
	}})
	for range results {
		d.tick(t.Context(), d.jobs[0])
	}
	want := []string{
		"WARN provider_warning", "WARN provider_warning", "ERROR provider_error",
		"DEBUG provider_warning", "DEBUG provider_warning", "DEBUG provider_error",
		"WARN provider_warning", "ERROR provider_error",
		"INFO provider_recovered",
	}
	if got := levels(logLines(t, buf), "output_published"); !slices.Equal(got, want) {
		t.Fatalf("log %q, want %q", got, want)
	}
}

// Provider events are logged in order before the tick failure, also when
// the tick is cancelled, with the core ids that a provider cannot replace.
func TestTickEvents(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancelled := false
	d, buf, _ := logDaemon(t, funcProvider{func(context.Context) (provider.Result, error) {
		ev := []provider.Event{
			{Name: "thing_done", Msg: "Thing done.", Level: slog.LevelInfo, Slot: 2, Attrs: []slog.Attr{
				slog.String("providerId", "forged"), slog.String("event", "forged"), slog.Int("slot", 9),
				slog.String("outputId", "forged"), slog.String("providerType", "forged"), slog.Int("count", 3),
			}},
			{Name: "thing_lost", Msg: "Thing lost.", Level: slog.LevelWarn},
		}
		if cancelled {
			cancel()
			return provider.Result{Events: ev[1:]}, context.Canceled
		}
		return provider.Result{Events: ev}, errors.New("api: 503")
	}})
	d.tick(ctx, d.jobs[0])
	cancelled = true
	d.tick(ctx, d.jobs[0])
	lines := logLines(t, buf)
	want := []string{"INFO thing_done", "WARN thing_lost", "ERROR provider_tick_failed", "WARN thing_lost"}
	if got := levels(lines); !slices.Equal(got, want) {
		t.Fatalf("log %q, want %q", got, want)
	}
	if l := lines[0]; l["providerId"] != "p" || l["providerType"] != "x" || l["outputId"] != "o" || l["slot"] != 2.0 || l["count"] != 3.0 || l["msg"] != "Thing done." {
		t.Fatalf("event %v", l)
	}
	if strings.Contains(buf.String(), "forged") {
		t.Fatalf("a provider replaced a core id: %s", buf)
	}
	if _, ok := lines[1]["slot"]; ok {
		t.Fatalf("provider-wide event has a slot: %v", lines[1])
	}
}

// A status.json write failure is logged once, then at debug, and its end
// as status_write_recovered.
func TestStatusWriteEdge(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root writes into a read-only directory")
	}
	line := fixtureLine(t)
	d, buf, dir := logDaemon(t, funcProvider{func(context.Context) (provider.Result, error) {
		return provider.Result{Lines: []string{line}}, nil
	}})
	d.tick(t.Context(), d.jobs[0])
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	d.tick(t.Context(), d.jobs[0])
	d.tick(t.Context(), d.jobs[0])
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	d.tick(t.Context(), d.jobs[0])
	want := []string{"INFO output_published", "ERROR status_write_failed", "DEBUG status_write_failed", "INFO status_write_recovered"}
	if got := levels(logLines(t, buf)); !slices.Equal(got, want) {
		t.Fatalf("log %q, want %q", got, want)
	}
}

// needs_login over two ticks is exactly two lines: provider_tick_failed at
// error, then its repeat at debug; no other event is logged for it.
func TestNeedsLoginLogLines(t *testing.T) {
	d, buf, _ := logDaemon(t, funcProvider{func(context.Context) (provider.Result, error) {
		return provider.Result{Status: wgpstatus.Provider{State: wgpstatus.StateNeedsLogin}}, errors.New("no valid session, sign in again")
	}})
	d.tick(t.Context(), d.jobs[0])
	d.tick(t.Context(), d.jobs[0])
	lines := logLines(t, buf)
	if got, want := levels(lines), []string{"ERROR provider_tick_failed", "DEBUG provider_tick_failed"}; !slices.Equal(got, want) {
		t.Fatalf("log %q, want %q", got, want)
	}
	if lines[0]["state"] != wgpstatus.StateNeedsLogin {
		t.Fatalf("state %v", lines[0])
	}
}

// output_published is info when the candidates change and debug when only
// their metadata does (changed: candidates|metadata).
func TestOutputPublishedChange(t *testing.T) {
	line := fixtureLine(t)
	lines := []string{line, strings.Replace(line, "load=40", "load=41", 1), line}
	n := 0
	d, buf, _ := logDaemon(t, funcProvider{func(context.Context) (provider.Result, error) {
		n++
		return provider.Result{Lines: []string{lines[n-1]}}, nil
	}})
	for range lines {
		d.tick(t.Context(), d.jobs[0])
	}
	var got []string
	for _, l := range logLines(t, buf) {
		if l["event"] == "output_published" {
			got = append(got, fmt.Sprintf("%s %s", l["level"], l["changed"]))
		}
	}
	if want := []string{"INFO candidates", "DEBUG metadata", "DEBUG metadata"}; !slices.Equal(got, want) {
		t.Fatalf("output_published %q, want %q", got, want)
	}
}
