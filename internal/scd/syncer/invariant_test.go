package syncer

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xtls/xray-core/app/router"
	"github.com/xtls/xray-core/core"

	"github.com/a1ekseev/scd/internal/scd/config"
	"github.com/a1ekseev/scd/internal/scd/monitor"
	"github.com/a1ekseev/scd/internal/scd/socks"
	"github.com/a1ekseev/scd/internal/scd/state"
	"github.com/a1ekseev/scd/internal/scd/xrayapi"
	"github.com/a1ekseev/scd/internal/scd/xrayapi/xrayfake"
)

// sourceOutbounds are the outbounds of source "wg" now in the fake.
func sourceOutbounds(srv *xrayfake.Server) []string {
	var out []string
	for _, tag := range srv.OutboundTags() {
		if strings.HasPrefix(tag, "x-observe-wg-") || strings.HasPrefix(tag, "wg-") {
			out = append(out, tag)
		}
	}
	return out
}

// watched passes calls to the fake and, after every mutation, fails the
// test when the source has more than 2 outbounds in it. With fail set, a
// mutation for which fail returns true fails: without being sent, or
// (executed) after being sent, like a deadline after Xray did it.
type watched struct {
	xrayapi.Client
	t        *testing.T
	srv      *xrayfake.Server
	fail     func(call string) (fail, executed bool)
	failures *atomic.Int32
}

func (w watched) step(call string, do func() error) error {
	fail, executed := false, false
	if w.fail != nil {
		fail, executed = w.fail(call)
	}
	var err error
	if !fail || executed {
		err = do()
	}
	if got := sourceOutbounds(w.srv); len(got) > 2 {
		w.t.Errorf("after %s: %d outbounds of the source, maxActive 2: %v", call, len(got), got)
	}
	if fail {
		if w.failures != nil {
			w.failures.Add(1)
		}
		return context.DeadlineExceeded
	}
	return err
}

func (w watched) AddOutbound(ctx context.Context, h *core.OutboundHandlerConfig) error {
	return w.step("AddOutbound:"+h.GetTag(), func() error { return w.Client.AddOutbound(ctx, h) })
}

func (w watched) RemoveOutbound(ctx context.Context, tag string) error {
	return w.step("RemoveOutbound:"+tag, func() error { return w.Client.RemoveOutbound(ctx, tag) })
}

func (w watched) AddInbound(ctx context.Context, h *core.InboundHandlerConfig) error {
	return w.step("AddInbound:"+h.GetTag(), func() error { return w.Client.AddInbound(ctx, h) })
}

func (w watched) RemoveInbound(ctx context.Context, tag string) error {
	return w.step("RemoveInbound:"+tag, func() error { return w.Client.RemoveInbound(ctx, tag) })
}

func (w watched) AddRule(ctx context.Context, r *router.RoutingRule) error {
	return w.step("AddRule:"+r.GetRuleTag(), func() error { return w.Client.AddRule(ctx, r) })
}

func (w watched) RemoveRule(ctx context.Context, tag string) error {
	return w.step("RemoveRule:"+tag, func() error { return w.Client.RemoveRule(ctx, tag) })
}

// watch makes every client of f (sync and monitor) a watched one.
func watch(t *testing.T, f *fixture, fail func(call string) (bool, bool)) {
	t.Helper()
	srv := f.x.servers[addrA]
	dial := srv.Dialer()
	f.s.Dial = func(addr string, timeout time.Duration) (xrayapi.Client, error) {
		c, err := dial(addr, timeout)
		if err != nil {
			return nil, err
		}
		return watched{Client: c, t: t, srv: srv, fail: fail}, nil
	}
}

// failOnce fails the first call that starts with prefix, executed or not.
func failOnce(prefix string, executed bool) func(string) (bool, bool) {
	var done atomic.Bool
	return func(call string) (bool, bool) {
		if strings.HasPrefix(call, prefix) && done.CompareAndSwap(false, true) {
			return true, executed
		}
		return false, false
	}
}

func monitorDeps(f *fixture, probeOK bool) monitor.Deps {
	return monitor.Deps{
		Memory: f.s.Memory, Log: f.s.Log, Dial: f.s.Dial, Now: time.Now,
		Probe: func(context.Context, socks.Request) (socks.Response, error) {
			if probeOK {
				return socks.Response{StatusCode: 200}, nil
			}
			return socks.Response{}, errors.New("probe failed")
		},
		Sleep: func(context.Context, time.Duration) error { return nil },
	}
}

func assertCap(t *testing.T, srv *xrayfake.Server, step string) {
	t.Helper()
	if got := sourceOutbounds(srv); len(got) > 2 {
		t.Fatalf("%s: %d outbounds of the source, maxActive 2: %v", step, len(got), got)
	}
}

// A full apply whose inbounds fail leaves no stale tunnels in state, so a
// repair cannot add an outbound next to the new ones.
func TestStaleStateAfterFailedFullApply(t *testing.T) {
	f, srv, path, all := newWG(t, 1, 2)
	watch(t, f, nil)
	before, _ := activeTags(t, f)
	sub := wgSub(path)
	f.s.Memory.Update(state.Key("sub", addrA), func(tg *state.Target) { tg.ResetSourceHashes("wg") })
	writeCandidates(t, path, all, 4, 5)
	srv.FailNext("AddInbound", errors.New("boom"))
	if r := f.run(t); r.Failed == 0 {
		t.Fatalf("report %+v", r)
	}
	assertCap(t, srv, "failed full apply")
	if tags, _ := activeTags(t, f); len(tags) != 0 {
		t.Fatalf("stale tunnels %v", tags)
	}
	if err := monitor.Repair(t.Context(), monitorDeps(f, false), sub, before[0]); err != nil {
		t.Fatal(err)
	}
	assertCap(t, srv, "repair")
	if r := f.run(t); r.Failed != 0 || len(sourceOutbounds(srv)) != 2 {
		t.Fatalf("report %+v, outbounds %v", r, sourceOutbounds(srv))
	}
}

// Even with state behind Xray (stale tunnels after a failed full apply),
// the cap check refuses the add and resets the hashes.
func TestCapRefusesStaleRepair(t *testing.T) {
	f, srv, path, all := newWG(t, 1, 2)
	key := state.Key("sub", addrA)
	var stale []state.Tunnel
	f.s.Memory.View(key, func(tg *state.Target) {
		for _, tn := range tg.Tunnels {
			stale = append(stale, *tn)
		}
	})
	f.s.Memory.Update(key, func(tg *state.Target) { tg.ResetSourceHashes("wg") })
	writeCandidates(t, path, all, 4, 5)
	if r := f.run(t); r.Failed != 0 {
		t.Fatalf("report %+v", r)
	}
	// Put the old topology back: state now disagrees with Xray.
	f.s.Memory.Update(key, func(tg *state.Target) {
		for i := range tg.Tunnels {
			tg.Tunnels[i].Mapping = stale[i].Mapping
		}
	})
	n := len(srv.Calls())
	if err := monitor.Repair(t.Context(), monitorDeps(f, false), wgSub(path), stale[0].Mapping.BaseTunnelID); err != nil {
		t.Fatal(err)
	}
	assertCap(t, srv, "repair")
	for _, c := range srv.Calls()[n:] {
		if strings.HasPrefix(c, "AddOutbound:") {
			t.Fatalf("added past the cap: %q", srv.Calls()[n:])
		}
	}
	if h := wgHashes(f); len(h) != 0 {
		t.Fatalf("hashes not reset: %v", h)
	}
}

// A failure at any step of a repair or rejoin (sent or not, including an
// add that failed after Xray did it), then a replacement by the monitor and
// a sync that fills or replaces: never more than maxActive outbounds of the
// source in Xray, checked after every RPC.
func TestRepairRejoinFailuresThenReplace(t *testing.T) {
	for _, op := range []string{"repair", "rejoin"} {
		for _, method := range []string{"RemoveRule", "RemoveOutbound", "AddOutbound", "AddRule"} {
			for _, executed := range []bool{false, true} {
				name := op + "/" + method
				if executed {
					name += "/executed"
				}
				t.Run(name, func(t *testing.T) {
					f, srv, path, all := newWG(t, 1, 2, 4, 5)
					sub := wgSub(path)
					before, _ := activeTags(t, f)
					if op == "rejoin" {
						watch(t, f, nil)
						if err := monitor.Repair(t.Context(), monitorDeps(f, false), sub, before[0]); err != nil {
							t.Fatal(err)
						}
					}
					watch(t, f, failOnce(method, executed))
					run := monitor.Repair
					if op == "rejoin" {
						run = monitor.Rejoin
					}
					if err := run(t.Context(), monitorDeps(f, op == "rejoin"), sub, before[0]); err != nil {
						t.Fatal(err)
					}
					assertCap(t, srv, op)
					watch(t, f, nil)
					if _, err := monitor.ReplaceFailed(t.Context(), monitorDeps(f, false), sub, sub.Sources[0], before[0]); err != nil {
						t.Fatal(err)
					}
					assertCap(t, srv, "replace")
					// Line 2 (slot 0's old candidate) is gone: the sync fills or
					// replaces, or applies in full.
					writeCandidates(t, path, all, 1, 3, 4, 5)
					f.run(t)
					assertCap(t, srv, "sync")
					f.run(t)
					assertCap(t, srv, "second sync")
					if got := sourceOutbounds(srv); len(got) != 2 {
						t.Fatalf("outbounds %v", got)
					}
				})
			}
		}
	}
}

// The ghost of a repair whose AddRule failed: state records the
// unprefixed outbound, so a replacement removes it.
func TestRepairGhostThenReplace(t *testing.T) {
	f, srv, path, all := newWG(t, 1, 2, 4, 5)
	watch(t, f, nil)
	before, _ := activeTags(t, f)
	sub := wgSub(path)
	srv.FailNext("AddRule", errors.New("boom"))
	if err := monitor.Repair(t.Context(), monitorDeps(f, false), sub, before[0]); err != nil {
		t.Fatal(err)
	}
	if done, err := monitor.ReplaceFailed(t.Context(), monitorDeps(f, false), sub, sub.Sources[0], before[0]); err != nil || !done {
		t.Fatalf("done %v, err %v", done, err)
	}
	assertCap(t, srv, "replace")
	writeCandidates(t, path, all, 1, 4, 5)
	f.run(t)
	assertCap(t, srv, "sync")
	if got := sourceOutbounds(srv); len(got) != 2 {
		t.Fatalf("outbounds %v", got)
	}
}

// Sync and monitor at once with a failure on every 7th mutation (every
// other one after Xray did it): the cap holds after every RPC. Run with
// -race.
func TestWireGuardConcurrentWithFailures(t *testing.T) {
	all := wgFixtureLines(t)
	path := filepath.Join(t.TempDir(), "wg")
	writeCandidates(t, path, all, 1, 2, 3, 4, 5)
	sub := wgSub(path)
	sub.Target.Monitor = config.TargetMonitorConfig{
		Enabled: true, MaxParallel: 2, Retry: config.RetryConfig{Attempts: 1},
		Request: &config.TargetMonitorRequestConfig{URL: "https://example.test/", Method: "GET", ExpectedStatus: 200, TimeoutMs: 1000},
	}
	f := newSourcesFixture(t, sub, nil)
	srv := f.x.servers[addrA]
	var n atomic.Int64
	watch(t, f, func(string) (bool, bool) {
		i := n.Add(1)
		return i%7 == 0, i%14 == 0
	})
	var probes atomic.Int64
	d := monitorDeps(f, true)
	d.Probe = func(context.Context, socks.Request) (socks.Response, error) {
		if probes.Add(1)%3 == 0 {
			return socks.Response{}, errors.New("timeout")
		}
		return socks.Response{StatusCode: 200}, nil
	}
	sets := [][]int{{1, 2, 3, 4, 5}, {1, 3, 4, 5}, {2, 4, 5}, {1, 2, 5}, {1}}
	var wg sync.WaitGroup
	wg.Go(func() {
		for i := range 25 {
			writeCandidates(t, path, all, sets[i%len(sets)]...)
			if _, err := f.s.Run(t.Context()); err != nil {
				t.Error(err)
			}
		}
	})
	wg.Go(func() {
		for range 50 {
			_ = monitor.TunnelTick(t.Context(), d, sub)
		}
	})
	wg.Wait()
	assertCap(t, srv, "end")
}
