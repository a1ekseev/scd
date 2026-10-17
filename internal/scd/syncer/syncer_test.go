package syncer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/a1ekseev/scd/internal/common/inputsrc"
	"github.com/a1ekseev/scd/internal/common/jsonout"
	"github.com/a1ekseev/scd/internal/common/logging"
	"github.com/a1ekseev/scd/internal/scd/apply"
	"github.com/a1ekseev/scd/internal/scd/config"
	"github.com/a1ekseev/scd/internal/scd/state"
	"github.com/a1ekseev/scd/internal/scd/xrayapi"
	"github.com/a1ekseev/scd/internal/scd/xrayapi/xrayfake"
)

const (
	vienna = "vless://7d1b6590-1069-4372-92be-8d0a0ae6eaf5@example.com:443?type=tcp&security=tls#🇦🇹 Вена, Австрия"
	berlin = "vless://7d1b6590-1069-4372-92be-8d0a0ae6eaf6@example.com:443?type=tcp&security=tls#🇩🇪 Берлин, Германия"
	trojan = "trojan://secret@example.com:443#Trojan"

	addrA = "127.0.0.1:8080"
	addrB = "127.0.0.1:8081"
)

// lockedBuffer is a goroutine-safe log sink.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// entries parses the JSON log lines.
func (l *lockedBuffer) entries(t *testing.T) []map[string]any {
	t.Helper()
	var out []map[string]any
	for line := range strings.Lines(l.String()) {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("log line %q: %v", line, err)
		}
		out = append(out, m)
	}
	return out
}

func (l *lockedBuffer) events(t *testing.T, event string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, e := range l.entries(t) {
		if e["event"] == event {
			out = append(out, e)
		}
	}
	return out
}

// xrays hands out clients of fake Xray servers by target address and
// records every dial.
type xrays struct {
	t       *testing.T
	mu      sync.Mutex
	servers map[string]*xrayfake.Server
	dialed  []string
}

func newXrays(t *testing.T, addrs ...string) *xrays {
	x := &xrays{t: t, servers: map[string]*xrayfake.Server{}}
	for _, a := range addrs {
		s, _ := xrayfake.Start(t)
		x.servers[a] = s
	}
	return x
}

func (x *xrays) dial(addr string, timeout time.Duration) (xrayapi.Client, error) {
	x.mu.Lock()
	x.dialed = append(x.dialed, addr)
	s := x.servers[addr]
	x.mu.Unlock()
	if s == nil {
		return nil, fmt.Errorf("no fake Xray at %s", addr)
	}
	return s.Dialer()(addr, timeout)
}

func (x *xrays) dials() []string {
	x.mu.Lock()
	defer x.mu.Unlock()
	return slices.Clone(x.dialed)
}

func count(calls []string, prefix string) int {
	n := 0
	for _, c := range calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

func target(addr string) config.SubscriptionTargetConfig {
	return config.SubscriptionTargetConfig{
		Address: addr, TimeoutMs: 5000,
		FixedOutbounds: []string{}, FixedInbounds: []string{}, FixedRouting: []string{},
		ObservatorySubjectSelectorPrefix: "x-observe-",
	}
}

func withSocks(t config.SubscriptionTargetConfig, start, end int) config.SubscriptionTargetConfig {
	t.InboundSocks = &config.InboundSocksConfig{Listen: "127.0.0.1", PortRange: config.PortRange{Start: start, End: end}}
	return t
}

func sub(id, input string, t config.SubscriptionTargetConfig) config.SubscriptionConfig {
	return config.SubscriptionConfig{ID: id, Input: input, Enabled: true, Format: "auto", FetchTimeoutMs: 5000, AllowInsecure: "deny", Target: t}
}

func cfg(kinds []apply.Kind, subs ...config.SubscriptionConfig) config.Loaded {
	var r config.ResourceConfig
	for _, k := range kinds {
		switch k {
		case apply.KindOutbound:
			r.Outbounds.Enabled = true
		case apply.KindInbound:
			r.Inbounds.Enabled = true
		case apply.KindRouting:
			r.Routing.Enabled = true
		}
	}
	return config.Loaded{Path: "/tmp/config.yml", Config: config.AppConfig{
		Subscriptions: subs,
		Runtime:       config.RuntimeConfig{Mode: "run-once"},
		Logging:       config.LoggingConfig{Level: "debug", Format: "json"},
		Resources:     r,
	}}
}

var (
	allKinds     = apply.Order
	outboundOnly = []apply.Kind{apply.KindOutbound}
)

// contents is a fake LoadInput: input → content, or an error for inputs
// listed in fail. It counts the loads.
type contents struct {
	mu    sync.Mutex
	text  map[string]string
	fail  map[string]error
	loads int
}

func (c *contents) load(_ context.Context, input string, _ inputsrc.Options) (inputsrc.Loaded, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.loads++
	if err := c.fail[input]; err != nil {
		return inputsrc.Loaded{}, err
	}
	text, ok := c.text[input]
	if !ok {
		return inputsrc.Loaded{}, errors.New("input source " + input + " returned empty content")
	}
	return inputsrc.Loaded{Source: input, Content: text, Encoding: "plain"}, nil
}

func (c *contents) set(input, text string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.text[input] = text
}

type fixture struct {
	s    *Syncer
	x    *xrays
	in   *contents
	logs *lockedBuffer
}

func newFixture(t *testing.T, l config.Loaded, text map[string]string, addrs ...string) *fixture {
	t.Helper()
	logs := &lockedBuffer{}
	log, err := logging.New(logs, "debug", "json")
	if err != nil {
		t.Fatal(err)
	}
	x := newXrays(t, addrs...)
	in := &contents{text: text, fail: map[string]error{}}
	return &fixture{
		s:    &Syncer{Config: l, Memory: state.New(), Log: log, Dial: x.dial, LoadInput: in.load},
		x:    x,
		in:   in,
		logs: logs,
	}
}

func (f *fixture) run(t *testing.T) Report {
	t.Helper()
	r, err := f.s.Run(t.Context())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return r
}

func hashes(t *testing.T, m *state.Memory, key string) map[apply.Kind]string {
	t.Helper()
	var h map[apply.Kind]string
	if !m.View(key, func(tg *state.Target) { h = tg.Hashes }) {
		t.Fatalf("no state for %s", key)
	}
	return h
}

func TestSkipsXrayWhenUnchanged(t *testing.T) {
	f := newFixture(t, cfg(outboundOnly, sub("source-1", "/s1", target(addrA))), map[string]string{"/s1": vienna}, addrA)

	first := f.run(t)
	second := f.run(t)

	if first.Failed != 0 || second.Failed != 0 {
		t.Fatalf("failed = %d, %d", first.Failed, second.Failed)
	}
	if second.Unchanged != 1 {
		t.Errorf("second unchanged = %d, want 1", second.Unchanged)
	}
	if got := second.Targets[0].UnchangedKinds; !slices.Equal(got, []apply.Kind{apply.KindOutbound}) {
		t.Errorf("unchangedKinds = %v", got)
	}
	if len(second.Targets[0].Resources) != 0 {
		t.Errorf("unchanged target has resources: %v", second.Targets[0].Resources)
	}
	calls := f.x.servers[addrA].Calls()
	if n := count(calls, "ListOutbounds"); n != 1 {
		t.Errorf("ListOutbounds calls = %d, want 1", n)
	}
	if n := count(calls, "RemoveOutbound"); n != 0 {
		t.Errorf("RemoveOutbound calls = %d, want 0", n)
	}
	if got := f.x.dials(); len(got) != 1 {
		t.Errorf("dials = %v, want one", got)
	}
	if n := len(f.logs.events(t, "skipped_no_changes")); n != 1 {
		t.Errorf("skipped_no_changes events = %d, want 1", n)
	}
}

func TestAppliesSubscriptionsIndependently(t *testing.T) {
	l := cfg(allKinds, sub("source-b", "/b", withSocks(target(addrB), 21000, 21010)), sub("source-a", "/a", withSocks(target(addrA), 20000, 20010)))
	f := newFixture(t, l, map[string]string{"/a": vienna, "/b": berlin}, addrA, addrB)

	r := f.run(t)

	if r.Failed != 0 {
		t.Fatalf("failed = %d: %+v", r.Failed, r)
	}
	if len(r.Targets) != 2 || r.Targets[0].TargetAddress != addrA || r.Targets[1].TargetAddress != addrB {
		t.Fatalf("targets = %+v", r.Targets)
	}
	if got := f.x.dials(); !slices.Equal(got, []string{addrA, addrB}) {
		t.Errorf("dials = %v, want [%s %s]", got, addrA, addrB)
	}
	for _, a := range []string{addrA, addrB} {
		if n := len(f.x.servers[a].OutboundTags()); n != 1 {
			t.Errorf("%s outbounds = %d, want 1", a, n)
		}
		if n := len(f.x.servers[a].InboundTags()); n != 1 {
			t.Errorf("%s inbounds = %d, want 1", a, n)
		}
		if n := len(f.x.servers[a].Rules()); n != 1 {
			t.Errorf("%s rules = %d, want 1", a, n)
		}
	}
	if r.Added != 6 {
		t.Errorf("added = %d, want 6", r.Added)
	}
	built := f.logs.events(t, "manifest_built")
	if len(built) != 6 {
		t.Fatalf("manifest_built events = %d, want 6 (one per kind)", len(built))
	}
	for _, e := range built {
		if e["managedCount"] != float64(1) {
			t.Errorf("manifest_built %v managedCount = %v, want 1", e["kind"], e["managedCount"])
		}
	}
}

func TestLogsFailedApplyItems(t *testing.T) {
	f := newFixture(t, cfg(outboundOnly, sub("source-1", "/s1", target(addrA))), map[string]string{"/s1": vienna}, addrA)
	const msg = "xray api rejected dynamic routing rule"
	f.x.servers[addrA].FailNext("AddOutbound", status.Error(codes.Unknown, msg))

	r := f.run(t)

	if r.Failed != 1 {
		t.Fatalf("failed = %d, want 1", r.Failed)
	}
	failed := f.logs.events(t, "apply_failed")
	if len(failed) != 1 {
		t.Fatalf("apply_failed events = %d, want 1", len(failed))
	}
	e := failed[0]
	if e["level"] != "ERROR" {
		t.Errorf("level = %v", e["level"])
	}
	if got, _ := json.Marshal(e["errorMessages"]); string(got) != `["`+msg+`"]` {
		t.Errorf("errorMessages = %s", got)
	}
	items, _ := e["failedItems"].([]any)
	if len(items) != 1 {
		t.Fatalf("failedItems = %v", e["failedItems"])
	}
	item := items[0].(map[string]any)
	if item["status"] != "failed" || item["message"] != msg || item["id"] == "" {
		t.Errorf("failedItems[0] = %v", item)
	}
	if h := hashes(t, f.s.Memory, state.Key("source-1", addrA)); len(h) != 0 {
		t.Errorf("hashes saved after a failed apply: %v", h)
	}
}

func TestOverlapReturnsEmptyReport(t *testing.T) {
	f := newFixture(t, cfg(outboundOnly, sub("source-1", "/s1", target(addrA))), map[string]string{"/s1": vienna}, addrA)
	entered := make(chan struct{})
	release := make(chan struct{})
	inner := f.s.LoadInput
	var once sync.Once
	f.s.LoadInput = func(ctx context.Context, input string, o inputsrc.Options) (inputsrc.Loaded, error) {
		once.Do(func() { // only the first run blocks
			close(entered)
			<-release
		})
		return inner(ctx, input, o)
	}

	done := make(chan Report)
	go func() {
		r, _ := f.s.Run(t.Context())
		done <- r
	}()
	<-entered

	second, err := f.s.Run(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	close(release)
	first := <-done

	if second.Failed != 0 || second.Targets == nil || len(second.Targets) != 0 || second.Skipped == nil || len(second.Skipped) != 0 {
		t.Errorf("overlap report = %+v, want empty with [] lists", second)
	}
	if second.AppliedAt == "" {
		t.Error("overlap report has no appliedAt")
	}
	if f.in.loads != 1 {
		t.Errorf("loads = %d, want 1 (the overlapping run must not load)", f.in.loads)
	}
	if len(first.Targets) != 1 || first.Failed != 0 {
		t.Errorf("first report = %+v", first)
	}
	w := f.logs.events(t, "sync_skipped_overlap")
	if len(w) != 1 || w[0]["level"] != "WARN" || w[0]["msg"] != "Sync is already running in this process; skipping overlap run." {
		t.Errorf("sync_skipped_overlap = %v", w)
	}
	// The guard is released after the run.
	if _, err := f.s.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	if f.in.loads != 2 {
		t.Errorf("loads after a later run = %d, want 2", f.in.loads)
	}
}

func TestOtherSubscriptionsContinueAfterLoadFailure(t *testing.T) {
	l := cfg(outboundOnly, sub("source-a", "/a", target(addrA)), sub("source-b", "/b", target(addrB)))
	f := newFixture(t, l, map[string]string{"/b": vienna}, addrA, addrB)
	f.in.fail["/a"] = errors.New("Input source timed out")

	r := f.run(t)

	if r.Failed != 1 {
		t.Fatalf("failed = %d, want 1", r.Failed)
	}
	if len(r.Targets) != 2 || r.Targets[0].TargetAddress != addrA || r.Targets[1].TargetAddress != addrB {
		t.Fatalf("targets = %+v", r.Targets)
	}
	a := r.Targets[0]
	if a.Failed != 1 || a.SourceID != "source-a" || len(a.Resources) != 1 || a.Resources[0].Items[0].Message != "Input source timed out" ||
		a.Resources[0].Items[0].ID != "outbound" || a.Resources[0].DurationMs != 0 {
		t.Errorf("failed target = %+v", a)
	}
	if a.UnchangedKinds == nil || a.Skipped == nil {
		t.Error("failed target lists must be [] not null")
	}
	if r.Targets[1].Failed != 0 {
		t.Errorf("second target failed = %d", r.Targets[1].Failed)
	}
	if got := f.x.dials(); !slices.Equal(got, []string{addrB}) {
		t.Errorf("dials = %v, want only %s", got, addrB)
	}
	if n := len(f.logs.events(t, "subscription_fetch_failed")); n != 1 {
		t.Errorf("subscription_fetch_failed events = %d", n)
	}
}

func TestNoXrayCallsWhenNoValidEntries(t *testing.T) {
	l := cfg(outboundOnly, sub("source-a", "/a", target(addrA)), sub("source-b", "/b", target(addrB)))
	f := newFixture(t, l, map[string]string{"/a": trojan, "/b": vienna}, addrA, addrB)

	r := f.run(t)

	if len(r.Targets) != 2 || r.Targets[0].Failed != 1 || r.Targets[1].Failed != 0 {
		t.Fatalf("targets = %+v", r.Targets)
	}
	if msg := r.Targets[0].Resources[0].Items[0].Message; !strings.Contains(msg, `Subscription source "source-a" produced no valid outbound entries.`) {
		t.Errorf("message = %q", msg)
	}
	if got := f.x.dials(); !slices.Equal(got, []string{addrB}) {
		t.Errorf("dials = %v, want only %s", got, addrB)
	}
	if len(f.x.servers[addrA].Calls()) != 0 {
		t.Errorf("Xray at %s was called", addrA)
	}
	if n := len(f.logs.events(t, "manifest_validation_failed")); n != 1 {
		t.Errorf("manifest_validation_failed events = %d", n)
	}
}

func TestNoXrayCallsWhenFiltersRemoveAll(t *testing.T) {
	s := sub("source-1", "/s1", target(addrA))
	s.Filters = &config.SubscriptionFiltersConfig{CountryAllowlist: []string{"US"}}
	f := newFixture(t, cfg(outboundOnly, s), map[string]string{"/s1": vienna}, addrA)

	r := f.run(t)

	if r.Failed != 1 || len(r.Targets) != 1 {
		t.Fatalf("report = %+v", r)
	}
	if got := f.x.dials(); len(got) != 0 {
		t.Errorf("dials = %v, want none", got)
	}
}

func TestPartialFailureKeepsHashesAndMonitors(t *testing.T) {
	l := cfg(allKinds, sub("source-1", "/s1", withSocks(target(addrA), 20000, 20010)))
	f := newFixture(t, l, map[string]string{"/s1": vienna}, addrA)
	key := state.Key("source-1", addrA)
	if r := f.run(t); r.Failed != 0 {
		t.Fatalf("first run failed: %+v", r)
	}
	before := hashes(t, f.s.Memory, key)
	f.s.Memory.Update(key, func(tg *state.Target) { tg.Tunnels[0].Monitor.State = state.TunnelHealthy })

	f.in.set("/s1", vienna+"\n"+berlin)
	srv := f.x.servers[addrA]
	srv.FailNext("AddRule", status.Error(codes.Unknown, "rule rejected"))
	r := f.run(t)

	if r.Failed == 0 {
		t.Fatal("second run did not fail")
	}
	if got := len(r.Targets[0].Resources); got != 3 {
		t.Errorf("resources = %d, want all 3 kinds applied", got)
	}
	f.s.Memory.View(key, func(tg *state.Target) {
		if tg.Generation != 1 {
			t.Errorf("generation = %d, want 1", tg.Generation)
		}
		if len(tg.Tunnels) != 1 || tg.Tunnels[0].Monitor.State != state.TunnelHealthy {
			t.Errorf("tunnels changed: %d, %+v", len(tg.Tunnels), tg.Tunnels[0].Monitor)
		}
	})
	if got := hashes(t, f.s.Memory, key); fmt.Sprint(got) != fmt.Sprint(before) {
		t.Errorf("hashes = %v, want unchanged %v", got, before)
	}

	// The next run applies every kind again and succeeds.
	n := len(srv.Calls())
	r = f.run(t)
	if r.Failed != 0 || r.Unchanged != 0 || len(r.Targets[0].Resources) != 3 {
		t.Fatalf("retry report = %+v", r)
	}
	calls := srv.Calls()[n:]
	for _, c := range []string{"ListOutbounds", "ListInbounds", "ListRule"} {
		if count(calls, c) != 1 {
			t.Errorf("retry calls %v lack %s", calls, c)
		}
	}
	f.s.Memory.View(key, func(tg *state.Target) {
		if tg.Generation != 2 || len(tg.Tunnels) != 2 {
			t.Errorf("after retry generation = %d, tunnels = %d", tg.Generation, len(tg.Tunnels))
		}
	})
}

func TestFullSuccessResetsMonitorsAndBumpsGeneration(t *testing.T) {
	l := cfg(allKinds, sub("source-1", "/s1", withSocks(target(addrA), 20000, 20010)))
	f := newFixture(t, l, map[string]string{"/s1": vienna}, addrA)
	key := state.Key("source-1", addrA)
	f.run(t)
	f.s.Memory.Update(key, func(tg *state.Target) {
		tg.Tunnels[0].Monitor = state.TunnelMonitor{State: state.TunnelDegraded, LastError: "probe failed", ConsecutiveFailures: 2}
		tg.Balancer.State = state.BalancerHealthy
	})

	f.in.set("/s1", vienna+"\n"+berlin)
	if r := f.run(t); r.Failed != 0 {
		t.Fatalf("report = %+v", r)
	}

	f.s.Memory.View(key, func(tg *state.Target) {
		if tg.Generation != 2 {
			t.Errorf("generation = %d, want 2", tg.Generation)
		}
		if len(tg.Tunnels) != 2 {
			t.Fatalf("tunnels = %d", len(tg.Tunnels))
		}
		for _, tn := range tg.Tunnels {
			if tn.Monitor != (state.TunnelMonitor{State: state.TunnelIdle}) {
				t.Errorf("monitor = %+v, want idle", tn.Monitor)
			}
		}
		if tg.Balancer.State != state.BalancerHealthy {
			t.Errorf("balancer state = %q, want kept healthy", tg.Balancer.State)
		}
		if len(tg.Hashes) != 3 {
			t.Errorf("hashes = %v", tg.Hashes)
		}
	})
}

func TestSyncPortRangeOverflowFailsOnlyThatTarget(t *testing.T) {
	l := cfg(allKinds,
		sub("source-a", "/a", withSocks(target(addrA), 20000, 20000)),
		sub("source-b", "/b", withSocks(target(addrB), 20000, 20010)))
	f := newFixture(t, l, map[string]string{"/a": vienna + "\n" + berlin, "/b": vienna + "\n" + berlin}, addrA, addrB)

	r, err := f.s.Run(t.Context())
	if err != nil {
		t.Fatalf("Run: %v (the overflow must fail only its target, D23)", err)
	}
	if len(r.Targets) != 2 {
		t.Fatalf("targets = %+v", r.Targets)
	}
	a, b := r.Targets[0], r.Targets[1]
	if a.TargetAddress != addrA || a.Failed != 3 || len(a.Resources) != 3 {
		t.Fatalf("overflow target = %+v", a)
	}
	for i, res := range a.Resources {
		if res.Kind != apply.Order[i] || !strings.Contains(res.Items[0].Message, "portRange can allocate only 1 ports, but 2 tunnels were generated") {
			t.Errorf("resource %d = %+v", i, res)
		}
	}
	if b.Failed != 0 || len(b.Resources) != 3 {
		t.Errorf("other target = %+v", b)
	}
	if got := f.x.dials(); !slices.Equal(got, []string{addrB}) {
		t.Errorf("dials = %v, want only %s", got, addrB)
	}
	if n := len(f.logs.events(t, "apply_failed")); n != 3 {
		t.Errorf("apply_failed events = %d, want 3", n)
	}
	if n := len(f.logs.events(t, "sync_failed")); n != 0 {
		t.Errorf("sync_failed logged")
	}
}

func TestFailedLoadsFirstInTargets(t *testing.T) {
	l := cfg(outboundOnly, sub("a-ok", "/ok", target(addrA)), sub("z-bad", "/bad", target(addrB)))
	f := newFixture(t, l, map[string]string{"/ok": vienna}, addrA, addrB)
	f.in.fail["/bad"] = errors.New("boom")

	r := f.run(t)

	if len(r.Targets) != 2 || r.Targets[0].SubscriptionID != "z-bad" || r.Targets[1].SubscriptionID != "a-ok" {
		t.Fatalf("targets order = %+v, want failed load first", r.Targets)
	}
}

func TestSkippedNotDuplicated(t *testing.T) {
	l := cfg(allKinds, sub("source-1", "/s1", withSocks(target(addrA), 20000, 20010)))
	f := newFixture(t, l, map[string]string{"/s1": vienna + "\n" + trojan}, addrA)

	for run := range 2 {
		r := f.run(t)
		if len(r.Skipped) != 1 || len(r.Targets[0].Skipped) != 1 {
			t.Errorf("run %d: skipped = %d, target skipped = %d, want 1 each (one manifest, not one per kind)",
				run, len(r.Skipped), len(r.Targets[0].Skipped))
		}
		if r.Skipped[0].Raw != trojan {
			t.Errorf("skipped = %+v", r.Skipped)
		}
	}
	for _, e := range f.logs.events(t, "manifest_built") {
		if e["skipped"] != float64(1) {
			t.Errorf("manifest_built skipped = %v", e["skipped"])
		}
	}
}

func TestEntryWarningsLogged(t *testing.T) {
	const (
		wsVision = "vless://7d1b6590-1069-4372-92be-8d0a0ae6eaf1@example.com:443?type=ws&security=tls&path=%2Fws&flow=xtls-rprx-vision#🇦🇹 Вена WS"
		unknown  = "vless://7d1b6590-1069-4372-92be-8d0a0ae6eaf2@example.com:443?type=tcp&security=tls&foo=1#🇦🇹 Вена Foo"
		noisy    = "vless://7d1b6590-1069-4372-92be-8d0a0ae6eaf3@example.com:443?type=tcp&security=tls&path=%2F#🇦🇹 Вена Path"
	)
	cases := []struct {
		name, link, code, level string
	}{
		{"ws vision", wsVision, "flow_cleared", "WARN"},
		{"unknown param", unknown, "param_ignored", "DEBUG"},
		{"inapplicable param", noisy, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, cfg(outboundOnly, sub("source-1", "/s1", target(addrA))), map[string]string{"/s1": tc.link}, addrA)
			if r := f.run(t); r.Failed != 0 {
				t.Fatalf("report = %+v", r)
			}
			w := f.logs.events(t, "manifest_entry_warning")
			if tc.code == "" {
				if len(w) != 0 {
					t.Errorf("manifest_entry_warning = %v, want none", w)
				}
				return
			}
			if len(w) != 1 {
				t.Fatalf("manifest_entry_warning = %v, want one", w)
			}
			e := w[0]
			if e["code"] != tc.code || e["level"] != tc.level || e["subscriptionId"] != "source-1" || e["targetAddress"] != addrA ||
				!strings.HasPrefix(e["tag"].(string), "vless-") || e["message"] == "" {
				t.Errorf("manifest_entry_warning = %v", e)
			}
		})
	}
}

// leakCheck fails when any secret appears in the logs or the JSON report.
func leakCheck(t *testing.T, logs string, r Report, secrets ...string) {
	t.Helper()
	var b bytes.Buffer
	if err := jsonout.Write(&b, r); err != nil {
		t.Fatal(err)
	}
	for _, s := range secrets {
		if strings.Contains(logs, s) {
			t.Errorf("logs contain %q:\n%s", s, logs)
		}
		if strings.Contains(b.String(), s) {
			t.Errorf("report contains %q:\n%s", s, b.String())
		}
	}
}

func TestSyncLogsDoNotLeakFetchHeaders(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("hwid=" + r.Header.Get("x-hwid")))
	}))
	t.Cleanup(srv.Close)
	s := sub("source-1", srv.URL+"/sub", target(addrA))
	s.FetchHeaders = inputsrc.Headers{"x-hwid": "s3cr3t"}
	f := newFixture(t, cfg(allKinds, s), nil, addrA)
	f.s.LoadInput = inputsrc.Load

	r := f.run(t)

	if r.Failed != 3 {
		t.Fatalf("failed = %d, want 3", r.Failed)
	}
	leakCheck(t, f.logs.String(), r, "s3cr3t")
}

func TestSyncLogsDoNotLeakSubscriptionURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "good") {
			_, _ = w.Write([]byte(vienna))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	const badTok, goodTok = "BadT0kenAbc123", "GoodT0kenXyz789"
	l := cfg(outboundOnly,
		sub("a-bad", srv.URL+"/api/sub/"+badTok+"?token=q"+badTok, target(addrA)),
		sub("b-good", srv.URL+"/api/good/"+goodTok+"?token=q"+goodTok, target(addrB)))
	f := newFixture(t, l, nil, addrA, addrB)
	f.s.LoadInput = inputsrc.Load

	r := f.run(t)

	if len(r.Targets) != 2 || r.Targets[0].Failed != 1 || r.Targets[1].Failed != 0 {
		t.Fatalf("report = %+v", r)
	}
	leakCheck(t, f.logs.String(), r, badTok, goodTok)
	want := srv.URL + "/<redacted>"
	if msg := r.Targets[0].Resources[0].Items[0].Message; !strings.Contains(msg, want) {
		t.Errorf("load error = %q, want the redacted URL %s", msg, want)
	}
	fetched := f.logs.events(t, "subscription_fetched")
	if len(fetched) != 1 {
		t.Fatalf("subscription_fetched = %v", fetched)
	}
	subs := fetched[0]["subscriptions"].([]any)
	if len(subs) != 1 || subs[0].(map[string]any)["source"] != want {
		t.Errorf("subscription_fetched subscriptions = %v", subs)
	}
	ff := f.logs.events(t, "subscription_fetch_failed")
	if len(ff) != 1 || ff[0]["source"] != want {
		t.Errorf("subscription_fetch_failed = %v", ff)
	}
}

func TestApplyNotCanceledByContext(t *testing.T) {
	f := newFixture(t, cfg(allKinds, sub("source-1", "/s1", withSocks(target(addrA), 20000, 20010))), map[string]string{"/s1": vienna}, addrA)
	srv := f.x.servers[addrA]
	srv.Delay("AddOutbound", 300*time.Millisecond)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	type result struct {
		r   Report
		err error
	}
	done := make(chan result)
	go func() {
		r, err := f.s.Run(ctx)
		done <- result{r, err}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for count(srv.Calls(), "AddOutbound") == 0 {
		if time.Now().After(deadline) {
			t.Fatal("AddOutbound was never called")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	res := <-done

	if res.err != nil {
		t.Fatalf("Run: %v", res.err)
	}
	if res.r.Failed != 0 {
		t.Fatalf("apply was interrupted: %+v", res.r)
	}
	if h := hashes(t, f.s.Memory, state.Key("source-1", addrA)); len(h) != 3 {
		t.Errorf("hashes = %v, want all 3 saved", h)
	}
	if len(srv.Rules()) != 1 || len(srv.InboundTags()) != 1 {
		t.Errorf("apply did not reach the end: rules %v inbounds %v", srv.Rules(), srv.InboundTags())
	}
}

func TestReportJSONShape(t *testing.T) {
	f := newFixture(t, cfg(outboundOnly, sub("source-1", "/s1", target(addrA))), map[string]string{"/s1": vienna}, addrA)
	r := f.run(t)
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, k := range []string{`"appliedAt":`, `"durationMs":`, `"added":1`, `"replaced":0`, `"removed":0`, `"unchanged":0`, `"failed":0`, `"targets":[{"subscriptionId":"source-1","targetAddress":"127.0.0.1:8080","sourceId":"source-1","unchangedKinds":[],"failed":0,"resources":[{"kind":"outbound"`, `"skipped":[]`} {
		if !strings.Contains(s, k) {
			t.Errorf("report JSON lacks %s:\n%s", k, s)
		}
	}
	if !strings.HasPrefix(s, `{"appliedAt":`) {
		t.Errorf("key order: %s", s)
	}
}

// Review 6.6: a sync that reaches a target marks it SyncSeen even when the
// apply fails (TS creates the target state at that point).
func TestSyncMarksTargetSeenEvenOnFailedApply(t *testing.T) {
	f := newFixture(t, cfg(outboundOnly, sub("source-1", "/s1", target(addrA))), map[string]string{"/s1": vienna}, addrA)
	f.x.servers[addrA].FailNext("AddOutbound", status.Error(codes.Unknown, "rejected"))
	if r := f.run(t); r.Failed != 1 {
		t.Fatalf("failed = %d, want 1", r.Failed)
	}
	var seen, topo bool
	if !f.s.Memory.View(state.Key("source-1", addrA), func(tg *state.Target) { seen, topo = tg.SyncSeen, tg.Topology != nil }) {
		t.Fatal("no target state")
	}
	if !seen || topo {
		t.Errorf("SyncSeen = %v, topology installed = %v; want true, false", seen, topo)
	}
}

// Final review I1: a cancellation lets the apply in flight finish but starts
// no apply on the remaining targets.
func TestCanceledSyncStartsNoFurtherApply(t *testing.T) {
	l := cfg(allKinds, sub("source-a", "/a", withSocks(target(addrA), 20000, 20010)), sub("source-b", "/b", withSocks(target(addrB), 21000, 21010)))
	f := newFixture(t, l, map[string]string{"/a": vienna, "/b": berlin}, addrA, addrB)
	srvA, srvB := f.x.servers[addrA], f.x.servers[addrB]
	srvA.Delay("AddOutbound", 300*time.Millisecond)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	done := make(chan Report)
	go func() {
		r, _ := f.s.Run(ctx)
		done <- r
	}()
	deadline := time.Now().Add(5 * time.Second)
	for count(srvA.Calls(), "AddOutbound") == 0 {
		if time.Now().After(deadline) {
			t.Fatal("AddOutbound was never called on A")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	r := <-done

	if len(srvA.Rules()) != 1 || len(srvA.InboundTags()) != 1 {
		t.Errorf("apply on A did not reach the end: rules %v inbounds %v", srvA.Rules(), srvA.InboundTags())
	}
	if calls := srvB.Calls(); len(calls) != 0 {
		t.Errorf("target B got RPCs after the cancel: %v", calls)
	}
	if got := f.x.dials(); !slices.Equal(got, []string{addrA}) {
		t.Errorf("dials = %v, want only %s", got, addrA)
	}
	if r.Failed != 0 {
		t.Errorf("report failed = %d: %+v", r.Failed, r)
	}
	if n := len(f.logs.events(t, "apply_failed")); n != 0 {
		t.Errorf("apply_failed events = %d, want 0", n)
	}
}

// Final review I1: a cancellation stops loading the remaining subscriptions.
func TestCanceledSyncStopsLoading(t *testing.T) {
	l := cfg(allKinds, sub("source-a", "/a", target(addrA)), sub("source-b", "/b", target(addrB)))
	f := newFixture(t, l, map[string]string{"/a": vienna, "/b": berlin}, addrA, addrB)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	inner := f.s.LoadInput
	f.s.LoadInput = func(c context.Context, input string, o inputsrc.Options) (inputsrc.Loaded, error) {
		cancel()
		return inner(c, input, o)
	}

	r, err := f.s.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if f.in.loads != 1 {
		t.Errorf("loads = %d, want 1", f.in.loads)
	}
	if d := f.x.dials(); len(d) != 0 {
		t.Errorf("dials after cancel = %v", d)
	}
	if r.Failed != 0 || len(f.logs.events(t, "subscription_fetch_failed")) != 0 {
		t.Errorf("cancel reported as failure: %+v", r)
	}
}

// Final review I1: a load cut short by the cancellation is not a failure.
func TestCanceledLoadIsNotReported(t *testing.T) {
	l := cfg(allKinds, sub("source-a", "/a", target(addrA)))
	f := newFixture(t, l, map[string]string{"/a": vienna}, addrA)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	f.s.LoadInput = func(c context.Context, _ string, _ inputsrc.Options) (inputsrc.Loaded, error) {
		cancel()
		return inputsrc.Loaded{}, fmt.Errorf("unable to download input: %w", c.Err())
	}

	r, err := f.s.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if r.Failed != 0 || len(r.Targets) != 0 {
		t.Errorf("report = %+v", r)
	}
	for _, ev := range []string{"subscription_fetch_failed", "apply_failed"} {
		if n := len(f.logs.events(t, ev)); n != 0 {
			t.Errorf("%s events = %d", ev, n)
		}
	}
}
