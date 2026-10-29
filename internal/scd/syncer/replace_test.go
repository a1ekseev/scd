package syncer

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/a1ekseev/scd/internal/scd/config"
	"github.com/a1ekseev/scd/internal/scd/monitor"
	"github.com/a1ekseev/scd/internal/scd/socks"
	"github.com/a1ekseev/scd/internal/scd/state"
	"github.com/a1ekseev/scd/internal/scd/xrayapi/xrayfake"
)

// mutationCalls are the Add* and Remove* calls of calls.
func mutationCalls(calls []string) []string {
	var out []string
	for _, c := range calls {
		if strings.HasPrefix(c, "Add") || strings.HasPrefix(c, "Remove") {
			out = append(out, c)
		}
	}
	return out
}

// firstCall is the index of the first call of method at or after from.
func firstCall(calls []string, from int, method string) int {
	return from + slices.IndexFunc(calls[from:], func(c string) bool { return c == method || strings.HasPrefix(c, method+":") })
}

func wgHashes(f *fixture) map[string]string {
	out := map[string]string{}
	f.s.Memory.View(state.Key("sub", addrA), func(tg *state.Target) {
		for k, v := range tg.Sources["wg"].Hashes {
			out[string(k)] = v
		}
	})
	return out
}

func wgWarnings(f *fixture) []string {
	var out []string
	f.s.Memory.View(state.Key("sub", addrA), func(tg *state.Target) {
		for _, w := range tg.Sources["wg"].AllWarnings() {
			out = append(out, w.Code)
		}
	})
	return out
}

// newWG syncs the lines of the fixture in full: slots [line 2, line 1].
func newWG(t *testing.T, lines ...int) (*fixture, *xrayfake.Server, string, []string) {
	t.Helper()
	all := wgFixtureLines(t)
	path := filepath.Join(t.TempDir(), "wg")
	writeCandidates(t, path, all, lines...)
	f := newSourcesFixture(t, wgSub(path), nil)
	if r := f.run(t); r.Failed != 0 {
		t.Fatalf("report %+v", r)
	}
	return f, f.x.servers[addrA], path, all
}

// A candidate gone from the file is replaced in its slot, remove before
// add, without a full apply.
func TestWireGuardReplaceSequence(t *testing.T) {
	f, srv, path, all := newWG(t, 1, 2, 4, 5)
	before, _ := activeTags(t, f)
	n := len(srv.Calls())
	writeCandidates(t, path, all, 1, 4, 5)
	r := f.run(t)
	if r.Failed != 0 || r.Removed != 3 || r.Added != 3 {
		t.Fatalf("report %+v", r)
	}
	tags, ports := activeTags(t, f)
	if tags[1] != before[1] || !slices.Equal(ports, []int{20100, 20101}) {
		t.Fatalf("active %v ports %v, before %v", tags, ports, before)
	}
	old, next := before[0], tags[0]
	want := []string{
		"RemoveRule:route-" + old, "RemoveOutbound:x-observe-" + old, "RemoveOutbound:" + old, "RemoveInbound:in-" + old,
		"AddOutbound:x-observe-" + next, "AddInbound:in-" + next, "AddRule:route-" + next + "->x-observe-" + next,
	}
	if got := mutationCalls(srv.Calls()[n:]); !slices.Equal(got, want) {
		t.Fatalf("calls %q, want %q", got, want)
	}
	if e := f.logs.events(t, "candidate_replaced"); len(e) != 1 || e[0]["fromTag"] != old || e[0]["toTag"] != next || e[0]["reasonCode"] != "gone" {
		t.Fatalf("candidate_replaced %v", e)
	}
	if e := f.logs.events(t, "resource_apply_started"); len(e) != 3 {
		t.Fatalf("a full apply ran: %d resource_apply_started", len(e))
	}

	// Unchanged: nothing is touched.
	n = len(srv.Calls())
	if r := f.run(t); r.Unchanged != 3 || len(mutationCalls(srv.Calls()[n:])) != 0 {
		t.Fatalf("report %+v, calls %q", r, srv.Calls()[n:])
	}
}

// A failure at any step of a replacement: no add after it, the slot stays
// empty, the hashes are reset, and the next sync applies the source in
// full; never more than maxActive outbounds.
func TestWireGuardReplaceFailure(t *testing.T) {
	for _, method := range []string{"RemoveOutbound", "RemoveInbound", "AddOutbound", "AddInbound", "AddRule"} {
		t.Run(method, func(t *testing.T) {
			f, srv, path, all := newWG(t, 1, 2, 4, 5)
			n := len(srv.Calls())
			srv.FailNext(method, errors.New("boom"))
			writeCandidates(t, path, all, 1, 4, 5)
			if r := f.run(t); r.Failed != 1 {
				t.Fatalf("report %+v", r)
			}
			failed := firstCall(srv.Calls(), n, method)
			if tags, _ := activeTags(t, f); len(tags) != 1 {
				t.Fatalf("active %v", tags)
			}
			if h := wgHashes(f); len(h) != 0 {
				t.Fatalf("hashes %v", h)
			}
			if e := f.logs.events(t, "candidate_replace_failed"); len(e) != 1 || e[0]["reasonCode"] != "gone" {
				t.Fatalf("candidate_replace_failed %v", e)
			}
			if method == "RemoveOutbound" && slices.ContainsFunc(srv.Calls()[n:], func(c string) bool { return strings.HasPrefix(c, "AddOutbound:") }) {
				t.Fatalf("added after a failed removal: %q", srv.Calls()[n:])
			}

			if r := f.run(t); r.Failed != 0 {
				t.Fatalf("next report %+v", r)
			}
			if e := f.logs.events(t, "resource_apply_started"); len(e) != 6 {
				t.Fatalf("no full apply: %d resource_apply_started", len(e))
			}
			if tags, _ := activeTags(t, f); len(tags) != 2 || len(srv.OutboundTags()) != 2 || len(srv.InboundTags()) != 2 {
				t.Fatalf("active %v, outbounds %v, inbounds %v", tags, srv.OutboundTags(), srv.InboundTags())
			}
			if p := peakOutbounds(srv.Calls(), "wg", failed); p > 2 {
				t.Fatalf("peak %d > maxActive 2: %q", p, srv.Calls())
			}
		})
	}
}

// A candidate gone without a replacement is removed and its slot left
// empty with a no_replacement warning; a new candidate fills it again.
func TestWireGuardRemoveWithoutReplacement(t *testing.T) {
	f, srv, path, all := newWG(t, 1, 2)
	before, _ := activeTags(t, f)
	n := len(srv.Calls())
	writeCandidates(t, path, all, 1)
	if r := f.run(t); r.Failed != 0 || r.Removed != 3 || r.Added != 0 {
		t.Fatalf("report %+v", r)
	}
	old := before[0]
	want := []string{"RemoveRule:route-" + old, "RemoveOutbound:x-observe-" + old, "RemoveOutbound:" + old, "RemoveInbound:in-" + old}
	if got := mutationCalls(srv.Calls()[n:]); !slices.Equal(got, want) {
		t.Fatalf("calls %q, want %q", got, want)
	}
	if tags, _ := activeTags(t, f); !slices.Equal(tags, before[1:]) {
		t.Fatalf("active %v", tags)
	}
	if w := wgWarnings(f); !slices.Equal(w, []string{"no_replacement"}) {
		t.Fatalf("warnings %v", w)
	}
	if e := f.logs.events(t, "candidate_removed"); len(e) != 1 || e[0]["fromTag"] != old || e[0]["level"] != "WARN" {
		t.Fatalf("candidate_removed %v", e)
	}

	// Line 3 (the key of the removed line 2) takes the empty slot: added in
	// place, the other tunnel untouched, no full apply.
	n = len(srv.Calls())
	writeCandidates(t, path, all, 1, 3)
	if r := f.run(t); r.Failed != 0 || r.Added != 3 || r.Removed != 0 {
		t.Fatalf("report %+v", r)
	}
	tags, ports := activeTags(t, f)
	if len(tags) != 2 || tags[1] != before[1] || !slices.Equal(ports, []int{20100, 20101}) {
		t.Fatalf("active %v ports %v", tags, ports)
	}
	next := tags[0]
	want = []string{"AddOutbound:x-observe-" + next, "AddInbound:in-" + next, "AddRule:route-" + next + "->x-observe-" + next}
	if got := mutationCalls(srv.Calls()[n:]); !slices.Equal(got, want) {
		t.Fatalf("calls %q, want %q", got, want)
	}
	if e := f.logs.events(t, "resource_apply_started"); len(e) != 3 {
		t.Fatalf("a full apply ran: %d resource_apply_started", len(e))
	}
	if e := f.logs.events(t, "candidate_replaced"); len(e) != 1 || e[0]["reasonCode"] != "vacant" || e[0]["toTag"] != next || e[0]["fromTag"] != nil {
		t.Fatalf("candidate_replaced %v", e)
	}
	if w := wgWarnings(f); len(w) != 0 {
		t.Fatalf("warnings %v", w)
	}
}

// A failure at any step of a fill: the added parts are removed, the other
// tunnel is untouched, the slot stays empty and the next sync applies the
// source in full; never more than maxActive outbounds.
func TestWireGuardFillFailure(t *testing.T) {
	for _, method := range []string{"AddOutbound", "AddInbound", "AddRule"} {
		t.Run(method, func(t *testing.T) {
			f, srv, path, all := newWG(t, 1, 2)
			writeCandidates(t, path, all, 1)
			if r := f.run(t); r.Failed != 0 {
				t.Fatalf("report %+v", r)
			}
			kept, _ := activeTags(t, f)
			n := len(srv.Calls())
			srv.FailNext(method, errors.New("boom"))
			writeCandidates(t, path, all, 1, 3)
			if r := f.run(t); r.Failed != 1 {
				t.Fatalf("report %+v", r)
			}
			failed := firstCall(srv.Calls(), n, method)
			for _, c := range mutationCalls(srv.Calls()[n:]) {
				if strings.Contains(c, kept[0]) {
					t.Fatalf("the other tunnel touched: %q", c)
				}
			}
			if tags, _ := activeTags(t, f); !slices.Equal(tags, kept) || len(wgHashes(f)) != 0 {
				t.Fatalf("active %v, hashes %v", tags, wgHashes(f))
			}
			if got := srv.OutboundTags(); len(got) != 1 {
				t.Fatalf("outbounds %v", got)
			}
			if e := f.logs.events(t, "candidate_replace_failed"); len(e) != 1 || e[0]["reasonCode"] != "vacant" {
				t.Fatalf("candidate_replace_failed %v", e)
			}
			if r := f.run(t); r.Failed != 0 || len(f.logs.events(t, "resource_apply_started")) != 6 {
				t.Fatalf("no full apply: %+v", r)
			}
			if tags, _ := activeTags(t, f); len(tags) != 2 || len(srv.OutboundTags()) != 2 {
				t.Fatalf("active %v", tags)
			}
			if p := peakOutbounds(srv.Calls(), "wg", failed); p > 2 {
				t.Fatalf("peak %d > maxActive 2: %q", p, srv.Calls())
			}
		})
	}
}

// A changed maxActive applies the source in full.
func TestWireGuardConfigHashChange(t *testing.T) {
	f, srv, _, _ := newWG(t, 1, 2, 4, 5)
	sub := wgSub(f.s.Config.Config.Subscriptions[0].Sources[0].Input)
	sub.Sources[0].MaxActive = 3
	f.s.Config = cfg(allKinds, sub)
	if r := f.run(t); r.Failed != 0 || len(srv.OutboundTags()) != 3 {
		t.Fatalf("report %+v, outbounds %v", r, srv.OutboundTags())
	}
}

// Sync and monitor (repair, rejoin, replacement after a failure) on one
// target at the same time: never more than maxActive outbounds of the
// source, and Xray matches the state at the end. Run with -race.
func TestWireGuardSyncAndMonitorConcurrently(t *testing.T) {
	all := wgFixtureLines(t)
	path := filepath.Join(t.TempDir(), "wg")
	writeCandidates(t, path, all, 1, 2, 3, 4, 5)
	sub := wgSub(path)
	sub.Target.Monitor = config.TargetMonitorConfig{
		Enabled: true, MaxParallel: 2, Retry: config.RetryConfig{Attempts: 1},
		Request: &config.TargetMonitorRequestConfig{URL: "https://example.test/", Method: "GET", ExpectedStatus: 200, TimeoutMs: 1000},
	}
	sub.Sources[0].Selection.DeadPairCooldownMinutes = 4
	f := newSourcesFixture(t, sub, nil)
	srv := f.x.servers[addrA]
	if r := f.run(t); r.Failed != 0 {
		t.Fatalf("report %+v", r)
	}

	var probes atomic.Int64
	clock := time.Unix(1_800_000_000, 0)
	var clockMu sync.Mutex
	now := func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		clock = clock.Add(time.Minute)
		return clock
	}
	f.s.Now = now
	d := monitor.Deps{
		Memory: f.s.Memory, Log: f.s.Log, Dial: f.s.Dial, Now: now,
		Probe: func(context.Context, socks.Request) (socks.Response, error) {
			if probes.Add(1)%3 == 0 {
				return socks.Response{}, errors.New("timeout")
			}
			return socks.Response{StatusCode: 200}, nil
		},
		Sleep: func(context.Context, time.Duration) error { return nil },
	}
	sets := [][]int{{1, 2, 3, 4, 5}, {1, 3, 4, 5}, {2, 4, 5}, {1, 2, 5}}
	var wg sync.WaitGroup
	wg.Go(func() {
		for i := range 20 {
			writeCandidates(t, path, all, sets[i%len(sets)]...)
			if _, err := f.s.Run(t.Context()); err != nil {
				t.Error(err)
			}
		}
	})
	wg.Go(func() {
		for range 40 {
			_ = monitor.TunnelTick(t.Context(), d, sub)
		}
	})
	wg.Wait()

	if p := peakOutbounds(srv.Calls(), "wg"); p > 2 {
		t.Fatalf("peak %d > maxActive 2", p)
	}
	if len(f.logs.events(t, "candidate_replaced")) == 0 {
		t.Fatal("no replacement ran")
	}
	var tunnels []string
	f.s.Memory.View(state.Key("sub", addrA), func(tg *state.Target) {
		for _, tn := range tg.Tunnels {
			tunnels = append(tunnels, tn.Mapping.OutboundTagCurrent)
		}
	})
	slices.Sort(tunnels)
	if got := srv.OutboundTags(); !slices.Equal(got, tunnels) {
		t.Fatalf("xray outbounds %v, state %v", got, tunnels)
	}
}

// An empty candidate file (wgp has no live candidate) is not a source
// failure: every tunnel is removed, the slots stay empty with the
// no_replacement warning; candidates coming back fill them again.
func TestWireGuardEmptyFile(t *testing.T) {
	f, srv, path, all := newWG(t, 1, 2)
	writeCandidates(t, path, all)
	if r := f.run(t); r.Failed != 0 || r.Removed != 6 || r.Added != 0 {
		t.Fatalf("report %+v", r)
	}
	if tags, _ := activeTags(t, f); len(tags) != 0 || len(srv.OutboundTags()) != 0 {
		t.Fatalf("active %v outbounds %v", tags, srv.OutboundTags())
	}
	if w := wgWarnings(f); !slices.Equal(w, []string{"no_replacement"}) {
		t.Fatalf("warnings %v", w)
	}
	if e := f.logs.events(t, "manifest_validation_failed"); len(e) != 0 {
		t.Fatalf("manifest_validation_failed %v", e)
	}
	writeCandidates(t, path, all, 1, 2)
	if r := f.run(t); r.Failed != 0 || r.Added != 6 {
		t.Fatalf("report %+v", r)
	}
	if w := wgWarnings(f); len(w) != 0 {
		t.Fatalf("warnings %v", w)
	}
}
