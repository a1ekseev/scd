package syncer

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/xtls/xray-core/core"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/a1ekseev/scd/internal/scd/apply"
	"github.com/a1ekseev/scd/internal/scd/config"
	"github.com/a1ekseev/scd/internal/scd/manifest"
	"github.com/a1ekseev/scd/internal/scd/state"
	"github.com/a1ekseev/scd/internal/scd/xrayapi"
	"github.com/a1ekseev/scd/internal/scd/xrayapi/xrayfake"
)

// balancerSubFixture has subscription "sub" with one vless source "vless" (/s1) in
// balancer x-balancer on addrA.
func balancerSubFixture(t *testing.T, kinds []apply.Kind, opts ...xrayfake.Option) (*fixture, *xrayfake.Server) {
	t.Helper()
	s := sub("sub", "/s1", target(addrA))
	s.Sources[0].Balancer = "x-balancer"
	f := newFixture(t, cfg(kinds, s), map[string]string{"/s1": vienna})
	srv, _ := xrayfake.Start(t, opts...)
	f.x.servers[addrA] = srv
	return f, srv
}

// checkEvent asserts the only source_check_failed event: its code and level.
func checkEvent(t *testing.T, f *fixture, code, level string) {
	t.Helper()
	ev := f.logs.events(t, "source_check_failed")
	if len(ev) == 0 {
		t.Fatal("no source_check_failed")
	}
	if e := ev[len(ev)-1]; e["reasonCode"] != code || e["level"] != level {
		t.Fatalf("source_check_failed %v", ev)
	}
}

// A balancer unknown to Xray blocks the source (no mutation, failed in the
// report) until it exists; a source whose balancer disappears later is
// blocked too, with its resources kept.
func TestCheckBalancerNotFound(t *testing.T) {
	f, srv := balancerSubFixture(t, outboundOnly)
	r := f.run(t)
	if r.Failed == 0 || mutations(srv.Calls()) != 0 || len(srv.OutboundTags()) != 0 {
		t.Fatalf("report %+v, calls %q", r.Targets, srv.Calls())
	}
	checkEvent(t, f, CodeBalancerNotFound, "ERROR")
	if ev := f.logs.events(t, "source_check_failed"); ev[0]["error"] != `Xray balancer "x-balancer" not found.` {
		t.Fatalf("error %v", ev[0]["error"])
	}
	if !slices.Contains(srv.Calls(), "GetBalancerInfo:x-balancer") {
		t.Fatalf("calls %q", srv.Calls())
	}

	srv.AddBalancer("x-balancer", []string{"x-observe-"}, "leastPing")
	if r := f.run(t); r.Failed != 0 || len(srv.OutboundTags()) != 1 {
		t.Fatalf("report %+v, outbounds %v", r.Targets, srv.OutboundTags())
	}
	if e := f.logs.events(t, "source_check_recovered"); len(e) != 1 {
		t.Fatalf("source_check_recovered %v", e)
	}

	// Unchanged hashes, but the balancer is gone (here: renamed in the
	// config): blocked, nothing removed.
	s := f.s.Config.Config.Subscriptions[0]
	s.Sources[0].Balancer = "gone"
	f.s.Config = cfg(outboundOnly, s)
	n := len(srv.Calls())
	if r := f.run(t); r.Failed == 0 || len(srv.OutboundTags()) != 1 || mutations(srv.Calls()[n:]) != 0 {
		t.Fatalf("report %+v, outbounds %v, calls %q", r.Targets, srv.OutboundTags(), srv.Calls()[n:])
	}
	checkEvent(t, f, CodeBalancerNotFound, "ERROR")
}

// Without RoutingService the balancer cannot be checked: a vless source
// with a balancer and routing off gets routing_service_disabled. With
// routing managed, the orphan removal already fails (ListRule) and no
// source is checked.
func TestCheckRoutingServiceDisabled(t *testing.T) {
	f, srv := balancerSubFixture(t, outboundOnly, xrayfake.WithoutRoutingService())
	if r := f.run(t); r.Failed == 0 || mutations(srv.Calls()) != 0 {
		t.Fatalf("report %+v", r.Targets)
	}
	checkEvent(t, f, CodeRoutingServiceDisabled, "ERROR")

	path := filepath.Join(t.TempDir(), "wg")
	writeCandidates(t, path, wgFixtureLines(t), 1, 2)
	w := wgSub(path)
	w.Sources[0].Balancer = "x-balancer"
	g := newSourcesFixture(t, w, nil, xrayfake.WithoutRoutingService())
	if r := g.run(t); r.Failed == 0 {
		t.Fatalf("report %+v", r.Targets)
	}
	if e := g.logs.events(t, "orphan_remove_failed"); len(e) != 1 {
		t.Fatalf("orphan_remove_failed %v", e)
	}
	if e := g.logs.events(t, "source_check_failed"); len(e) != 0 {
		t.Fatalf("source_check_failed %v", e)
	}
}

// A dialerProxy that is not an outbound of the target blocks the source
// until a client adds it.
func TestCheckDialerProxyMissing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wg")
	writeCandidates(t, path, wgFixtureLines(t), 1, 2)
	w := wgSub(path)
	w.Sources[0].DialerProxy = "chain-up"
	w.Target.FixedOutbounds = []string{"chain-up"}
	f := newSourcesFixture(t, w, nil)
	srv := f.x.servers[addrA]
	if r := f.run(t); r.Failed == 0 || mutations(srv.Calls()) != 0 {
		t.Fatalf("report %+v, calls %q", r.Targets, srv.Calls())
	}
	checkEvent(t, f, CodeDialerProxyMissing, "ERROR")
	if ev := f.logs.events(t, "source_check_failed"); ev[0]["error"] != `dialerProxy "chain-up" is not an outbound of the target` {
		t.Fatalf("error %v", ev[0]["error"])
	}
	f.run(t)
	checkEvent(t, f, CodeDialerProxyMissing, "DEBUG")

	c, err := srv.Dialer()("bufconn", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.AddOutbound(context.Background(), &core.OutboundHandlerConfig{Tag: "chain-up"}); err != nil {
		t.Fatal(err)
	}
	_ = c.Close()
	if r := f.run(t); r.Failed != 0 || len(srv.OutboundTags()) != 3 {
		t.Fatalf("report %+v, outbounds %v", r.Targets, srv.OutboundTags())
	}
	if e := f.logs.events(t, "source_check_recovered"); len(e) != 1 {
		t.Fatalf("source_check_recovered %v", e)
	}
}

// Another GetBalancerInfo error is no check verdict: the source fails like
// an apply (apply_failed), an unavailable API starts xray_unavailable, and
// the next verdict of the API (here balancer_not_found) ends it.
func TestCheckBalancerXrayError(t *testing.T) {
	f, srv := balancerSubFixture(t, outboundOnly)
	srv.FailNext("GetBalancerInfo", status.Error(codes.Unavailable, "connection refused"))
	if r := f.run(t); r.Failed == 0 || mutations(srv.Calls()) != 0 {
		t.Fatalf("report %+v", r.Targets)
	}
	if e := f.logs.events(t, "source_check_failed"); len(e) != 0 {
		t.Fatalf("source_check_failed %v", e)
	}
	if e := f.logs.events(t, "apply_failed"); len(e) != 1 {
		t.Fatalf("apply_failed %v", e)
	}
	if e := f.logs.events(t, "xray_unavailable"); len(e) != 1 {
		t.Fatalf("xray_unavailable %v", e)
	}
	// The orphan removal answers first and ends the episode; the
	// verdict changes nothing more.
	f.run(t)
	if e := f.logs.events(t, "xray_recovered"); len(e) != 1 {
		t.Fatalf("xray_recovered %v", e)
	}
	checkEvent(t, f, CodeBalancerNotFound, "ERROR")
}

// A sync cancelled during the balancer check logs and reports nothing for
// the source (like a cancelled orphan removal) and applies nothing.
func TestCheckCanceled(t *testing.T) {
	f, srv := balancerSubFixture(t, outboundOnly)
	srv.Delay("GetBalancerInfo", 5*time.Second)
	ctx, cancel := context.WithCancel(t.Context())
	time.AfterFunc(50*time.Millisecond, cancel)
	r, err := f.s.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if r.Canceled != PhaseTargets || len(r.Targets) != 0 || mutations(srv.Calls()) != 0 {
		t.Fatalf("report %+v, calls %q", r, srv.Calls())
	}
	for _, ev := range []string{"apply_failed", "source_check_failed", "xray_unavailable"} {
		if e := f.logs.events(t, ev); len(e) != 0 {
			t.Errorf("%s %v", ev, e)
		}
	}
}

// A balancer_not_found verdict is an answer of the API: it ends an
// xray_unavailable episode of the target by itself.
func TestCheckVerdictEndsXrayUnavailable(t *testing.T) {
	f, _ := balancerSubFixture(t, outboundOnly)
	f.run(t) // initialises the syncer; the source is blocked
	c := f.s.Config.Config.Subscriptions[0]
	state.NoteXray(t.Context(), f.s.Memory, f.s.Log, c.ID, c.Target.Address, &xrayapi.Error{Message: "down", Unavailable: true}, time.Now())
	if e := f.logs.events(t, "xray_unavailable"); len(e) != 1 {
		t.Fatalf("xray_unavailable %v", e)
	}
	if _, ok := f.s.checkSource(t.Context(), manifest.LoadedSource{Subscription: c, Config: c.Sources[0]}); ok {
		t.Fatal("check passed without the balancer")
	}
	if e := f.logs.events(t, "xray_recovered"); len(e) != 1 {
		t.Fatalf("xray_recovered %v", e)
	}
}

// A verdict records the reason code in state (the monitor treats the source
// as waiting); a passed check clears it.
func TestCheckRecordsFailedCheck(t *testing.T) {
	f, srv := balancerSubFixture(t, outboundOnly)
	srv.AddBalancer("x-balancer", []string{"x-observe-"}, "leastPing")
	f.run(t)
	key := state.Key("sub", addrA)
	s := f.s.Config.Config.Subscriptions[0]
	s.Sources[0].Balancer = "gone"
	f.s.Config = cfg(outboundOnly, s)
	f.run(t)
	code := ""
	f.s.Memory.View(key, func(tg *state.Target) { code = tg.CheckFailed["vless"] })
	if code != CodeBalancerNotFound {
		t.Fatalf("CheckFailed %q", code)
	}
	s.Sources[0].Balancer = "x-balancer"
	f.s.Config = cfg(outboundOnly, s)
	f.run(t)
	f.s.Memory.View(key, func(tg *state.Target) {
		if len(tg.CheckFailed) != 0 {
			t.Fatalf("CheckFailed %v", tg.CheckFailed)
		}
	})
}

// warpBalancerDep is the wgSub source renamed warp, depending only on
// balancer pool-en.
func warpBalancerDep(t *testing.T) config.SubscriptionConfig {
	t.Helper()
	path := filepath.Join(t.TempDir(), "warp")
	writeCandidates(t, path, wgFixtureLines(t), 1, 2)
	w := wgSub(path)
	w.Sources[0].ID = "warp"
	w.Sources[0].DependsOn = []config.DependencyConfig{{Balancer: "pool-en", Health: config.HealthBalancer, RecoverGraceSeconds: 60}}
	return w
}

// outboundsOf returns the outbounds of source id in srv.
func outboundsOf(srv *xrayfake.Server, id string) []string {
	var out []string
	for _, tag := range srv.OutboundTags() {
		if touchesSource("x:"+tag, id) {
			out = append(out, tag)
		}
	}
	return out
}

// A balancer dependency needs no state of another source: with the balancer
// in Xray (here without a target, unhealthy) the source applies in the
// first sync; health is the monitor's business.
func TestCheckDependencyBalancerExists(t *testing.T) {
	f := newSourcesFixture(t, warpBalancerDep(t), nil, xrayfake.WithBalancer("pool-en", []string{"via-en"}, "leastPing"))
	srv := f.x.servers[addrA]
	if r := f.run(t); r.Failed != 0 || len(outboundsOf(srv, "warp")) != 2 {
		t.Fatalf("report %+v, outbounds %v", r.Targets, srv.OutboundTags())
	}
	if e := f.logs.events(t, "source_check_failed"); len(e) != 0 {
		t.Fatalf("source_check_failed %v", e)
	}
	if !slices.Contains(srv.Calls(), "GetBalancerInfo:pool-en") {
		t.Fatalf("calls %q", srv.Calls())
	}
}

// A dependency balancer missing in Xray blocks the source (no mutation,
// CheckFailed, a source error) until it exists. Removed later, it blocks
// the applied source with its tunnels kept; once Xray loses them, no full
// apply brings them back: zero tunnels until the balancer returns.
func TestCheckDependencyBalancerNotFound(t *testing.T) {
	f := newSourcesFixture(t, warpBalancerDep(t), nil)
	srv := f.x.servers[addrA]
	key := state.Key("sub", addrA)
	if r := f.run(t); r.Failed == 0 || mutations(srv.Calls()) != 0 {
		t.Fatalf("report %+v, calls %q", r.Targets, srv.Calls())
	}
	checkEvent(t, f, CodeBalancerNotFound, "ERROR")
	const msg = `dependency balancer "pool-en": Xray balancer "pool-en" not found.`
	if ev := f.logs.events(t, "source_check_failed"); ev[0]["error"] != msg {
		t.Fatalf("error %v", ev[0]["error"])
	}
	if e, ok := sourceError(f, "warp"); !ok || e.Code != CodeBalancerNotFound || e.Message != msg {
		t.Fatalf("source error %+v %v", e, ok)
	}
	f.run(t) // the target state exists from the first sync's source error on
	checkEvent(t, f, CodeBalancerNotFound, "DEBUG")
	f.s.Memory.View(key, func(tg *state.Target) {
		if tg.CheckFailed["warp"] != CodeBalancerNotFound {
			t.Fatalf("CheckFailed %v", tg.CheckFailed)
		}
	})

	srv.AddBalancer("pool-en", []string{"via-en"}, "leastPing")
	if r := f.run(t); r.Failed != 0 || len(outboundsOf(srv, "warp")) != 2 {
		t.Fatalf("report %+v, outbounds %v", r.Targets, srv.OutboundTags())
	}
	if e := f.logs.events(t, "source_check_recovered"); len(e) != 1 {
		t.Fatalf("source_check_recovered %v", e)
	}

	srv.RemoveBalancer("pool-en")
	n := len(srv.Calls())
	if r := f.run(t); r.Failed == 0 || mutations(srv.Calls()[n:]) != 0 || len(outboundsOf(srv, "warp")) != 2 {
		t.Fatalf("report %+v, calls %q", r.Targets, srv.Calls()[n:])
	}
	checkEvent(t, f, CodeBalancerNotFound, "ERROR")
	f.s.Memory.View(key, func(tg *state.Target) {
		if !tg.SourceWaiting("warp") {
			t.Fatal("not waiting with a failed check")
		}
	})

	// Xray loses the tunnels (no restart in the fake: a client removes them).
	c, err := srv.Dialer()(addrA, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	for _, tag := range srv.InboundTags() {
		if err := c.RemoveInbound(t.Context(), tag); err != nil {
			t.Fatal(err)
		}
	}
	for _, tag := range outboundsOf(srv, "warp") {
		if err := c.RemoveOutbound(t.Context(), tag); err != nil {
			t.Fatal(err)
		}
	}
	n = len(srv.Calls())
	if r := f.run(t); r.Failed == 0 || count(srv.Calls()[n:], "Add") != 0 || len(outboundsOf(srv, "warp")) != 0 {
		t.Fatalf("report %+v, calls %q", r.Targets, srv.Calls()[n:])
	}
	// The check comes before the drift check: the loss is not seen yet.
	if e := f.logs.events(t, "xray_state_drift"); len(e) != 0 {
		t.Fatalf("xray_state_drift %v", e)
	}
}

// Without RoutingService a dependency balancer cannot be checked:
// routing_service_disabled with the dependency named.
func TestCheckDependencyBalancerRoutingDisabled(t *testing.T) {
	f, srv := balancerSubFixture(t, outboundOnly, xrayfake.WithoutRoutingService())
	s := f.s.Config.Config.Subscriptions[0]
	s.Sources[0].Balancer = ""
	s.Sources[0].DependsOn = []config.DependencyConfig{{Balancer: "pool-en", Health: config.HealthBalancer, RecoverGraceSeconds: 60}}
	f.s.Config = cfg(outboundOnly, s)
	if r := f.run(t); r.Failed == 0 || mutations(srv.Calls()) != 0 {
		t.Fatalf("report %+v", r.Targets)
	}
	checkEvent(t, f, CodeRoutingServiceDisabled, "ERROR")
	if ev := f.logs.events(t, "source_check_failed"); !strings.HasPrefix(ev[0]["error"].(string), `dependency balancer "pool-en": `) {
		t.Fatalf("error %v", ev[0]["error"])
	}
}

// crossFixture has subscription "sub" (addrA) whose wireguard source "warp"
// depends on balancer pool-blanc of subscription blanc-en (addrB, disabled
// here: sync never dials it); the clock is fixed. en writes the selection
// state of pool-blanc the way balancer.Tick does.
func crossFixture(t *testing.T) (*fixture, *xrayfake.Server, time.Time) {
	t.Helper()
	w := warpBalancerDep(t)
	w.Sources[0].DependsOn = []config.DependencyConfig{{Subscription: "blanc-en", Balancer: "pool-blanc", Health: config.HealthBalancer, RecoverGraceSeconds: 60}}
	en := sub("blanc-en", "/en", target(addrB))
	en.Enabled = false
	en.Target.Balancers = []config.BalancerConfig{{Tag: "pool-blanc", Selection: &config.BalancerSelectionConfig{Enabled: true, Schedule: "*/15 * * * * *"}}}
	f := newFixture(t, cfg(allKinds, w, en), nil)
	srv, _ := xrayfake.Start(t)
	f.x.servers[addrA] = srv
	now := time.Unix(1_800_000_000, 0)
	f.s.Now = func() time.Time { return now }
	return f, srv, now
}

func enState(f *fixture, fn func(tg *state.Target, s *state.SelectionState)) {
	key := state.Key("blanc-en", addrB)
	f.s.Memory.Ensure(key)
	f.s.Memory.Update(key, func(tg *state.Target) { fn(tg, &tg.EnsureBalancer("pool-blanc").Selection) })
}

const enNotFound = `Xray balancer "pool-blanc" not found.`

func notFoundAt(at time.Time) func(*state.Target, *state.SelectionState) {
	return func(_ *state.Target, s *state.SelectionState) {
		s.State, s.NotFound, s.LastCheckedAt, s.LastError = state.SelectionError, true, new(at), enNotFound
	}
}

// A dependency on another subscription's balancer costs no RPC: only a
// fresh BalancerNotFound in its selection state blocks the source (with
// that target's Xray down too), until the state is ok again.
func TestCheckCrossBalancer(t *testing.T) {
	f, srv, now := crossFixture(t)
	if r := f.run(t); r.Failed != 0 || len(outboundsOf(srv, "warp")) != 2 {
		t.Fatalf("report %+v, outbounds %v", r.Targets, srv.OutboundTags())
	}
	if slices.Contains(srv.Calls(), "GetBalancerInfo:pool-blanc") {
		t.Fatalf("calls %q", srv.Calls())
	}

	enState(f, notFoundAt(now))
	n := len(srv.Calls())
	if r := f.run(t); r.Failed == 0 || mutations(srv.Calls()[n:]) != 0 {
		t.Fatalf("report %+v, calls %q", r.Targets, srv.Calls()[n:])
	}
	checkEvent(t, f, CodeBalancerNotFound, "ERROR")
	if ev := f.logs.events(t, "source_check_failed"); ev[0]["error"] != `dependency balancer "blanc-en/pool-blanc": `+enNotFound {
		t.Fatalf("error %v", ev[0]["error"])
	}
	f.s.Memory.View(state.Key("sub", addrA), func(tg *state.Target) {
		if tg.CheckFailed["warp"] != CodeBalancerNotFound {
			t.Fatalf("CheckFailed %v", tg.CheckFailed)
		}
	})

	enState(f, func(tg *state.Target, _ *state.SelectionState) { tg.SetXrayDown("down", now) })
	n = len(srv.Calls())
	if r := f.run(t); r.Failed == 0 || mutations(srv.Calls()[n:]) != 0 {
		t.Fatalf("XrayDown: report %+v", r.Targets)
	}

	enState(f, func(_ *state.Target, s *state.SelectionState) {
		s.State, s.NotFound, s.LastSuccessAt, s.LastCheckedAt, s.LastError = state.SelectionOK, false, new(now), new(now), ""
	})
	if r := f.run(t); r.Failed != 0 {
		t.Fatalf("report %+v", r.Targets)
	}
	if e := f.logs.events(t, "source_check_recovered"); len(e) != 1 {
		t.Fatalf("source_check_recovered %v", e)
	}
	if d := f.x.dials(); slices.Contains(d, addrB) {
		t.Fatalf("dials %v", d)
	}
}

// Any other state of another subscription's balancer does not block.
func TestCheckCrossBalancerPasses(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(now time.Time) func(*state.Target, *state.SelectionState)
	}{
		{"stale not found", func(now time.Time) func(*state.Target, *state.SelectionState) {
			return notFoundAt(now.Add(-46 * time.Second))
		}},
		{"unavailable", func(now time.Time) func(*state.Target, *state.SelectionState) {
			return func(_ *state.Target, s *state.SelectionState) {
				s.State, s.LastCheckedAt, s.LastError = state.SelectionError, new(now), "unavailable"
			}
		}},
		{"service disabled", func(now time.Time) func(*state.Target, *state.SelectionState) {
			return func(_ *state.Target, s *state.SelectionState) {
				s.State, s.LastCheckedAt, s.LastError = state.SelectionError, new(now), (&xrayapi.ServiceDisabledError{Service: "RoutingService"}).Error()
			}
		}},
		{"never polled", func(time.Time) func(*state.Target, *state.SelectionState) {
			return func(*state.Target, *state.SelectionState) {}
		}},
		{"unhealthy", func(now time.Time) func(*state.Target, *state.SelectionState) {
			return func(_ *state.Target, s *state.SelectionState) {
				s.State, s.LastSuccessAt, s.LastCheckedAt = state.SelectionOK, new(now), new(now)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, srv, now := crossFixture(t)
			enState(f, tc.set(now))
			if r := f.run(t); r.Failed != 0 || len(outboundsOf(srv, "warp")) != 2 {
				t.Fatalf("report %+v", r.Targets)
			}
		})
	}
}
