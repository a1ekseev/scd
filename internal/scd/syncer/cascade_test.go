package syncer

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xtls/xray-core/app/observatory"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/a1ekseev/scd/internal/scd/balancer"
	"github.com/a1ekseev/scd/internal/scd/config"
	"github.com/a1ekseev/scd/internal/scd/monitor"
	"github.com/a1ekseev/scd/internal/scd/socks"
	"github.com/a1ekseev/scd/internal/scd/state"
	"github.com/a1ekseev/scd/internal/scd/xrayapi"
	"github.com/a1ekseev/scd/internal/scd/xrayapi/xrayfake"
)

// cascadeClock is the monitor clock of the cascade test.
type cascadeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *cascadeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *cascadeClock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// addsOf returns the AddInbound calls of source id: in the monitor only a
// replacement adds an inbound.
func addsOf(calls []string, id string) []string {
	var out []string
	for _, c := range calls {
		m, tag, _ := strings.Cut(c, ":")
		if (m == "AddInbound") && strings.HasPrefix(tag, "in-"+id+"-") {
			out = append(out, c)
		}
	}
	return out
}

// A cascade vless → warp → proton through the monitor: while the upper
// balancer selects nothing the lower stages wait (no replacement, no
// quarantine; the quarantines of the observatory lag are released); after
// the recovery and the pause both waits end, and replacements resume.
// Remove before add and maxActive hold throughout.
func TestCascadeMonitorWait(t *testing.T) {
	all := wgFixtureLines(t)
	dir := t.TempDir()
	pw, pp := filepath.Join(dir, "warp"), filepath.Join(dir, "proton")
	writeCandidates(t, pw, all, 1, 2, 4)
	writeCandidates(t, pp, all, 5, 6, 1, 2)
	sub := wgSub(pw)
	warp := sub.Sources[0]
	warp.ID = "warp"
	warp.Balancer = "x-warp"
	warp.Selection.ReplaceAfterFailures = 2
	warp.DependsOn = []config.DependencyConfig{{Source: "vless", Health: config.HealthBalancer, RecoverGraceSeconds: 60}}
	proton := warp
	sel := *warp.Selection
	proton.Selection = &sel
	proton.ID, proton.Input, proton.Balancer = "proton", pp, ""
	proton.PortRange = &config.PortRange{Start: 20200, End: 20203}
	proton.DependsOn = []config.DependencyConfig{{Source: "warp", Health: config.HealthBalancer, RecoverGraceSeconds: 60}}
	v := source("vless", "/v")
	v.Balancer = "x-balancer"
	v.PortRange = &config.PortRange{Start: 20000, End: 20010}
	sub.Sources = []config.SourceConfig{v, warp, proton}
	sub.Target.Monitor = config.TargetMonitorConfig{
		Enabled: true, MaxParallel: 4, Retry: config.RetryConfig{Attempts: 1},
		Request: &config.TargetMonitorRequestConfig{URL: "https://example.test/", Method: "GET", ExpectedStatus: 200, TimeoutMs: 1000},
	}
	f := newSourcesFixture(t, sub, map[string]string{"/v": vienna},
		xrayfake.WithBalancer("x-balancer", []string{"none-"}, "random"),
		xrayfake.WithBalancer("x-warp", []string{"none-"}, "random"))
	srv := f.x.servers[addrA]
	srv.SetOverride("x-balancer", "x-observe-vless-1")
	srv.SetOverride("x-warp", "x-observe-warp-1")
	if r := f.run(t); r.Failed != 0 {
		t.Fatalf("report %+v", r.Targets)
	}
	key := state.Key("sub", addrA)
	var mu sync.Mutex
	failing := map[int]bool{}
	clock := &cascadeClock{now: time.Now()}
	d := monitor.Deps{
		Memory: f.s.Memory, Log: f.s.Log, Dial: f.s.Dial, Now: clock.Now,
		Probe: func(_ context.Context, r socks.Request) (socks.Response, error) {
			mu.Lock()
			defer mu.Unlock()
			if failing[r.ProxyPort] || failing[r.ProxyPort/100*100] {
				return socks.Response{}, errors.New("probe failed")
			}
			return socks.Response{StatusCode: 200}, nil
		},
		Sleep: func(context.Context, time.Duration) error { return nil },
	}
	setFail := func(bases ...int) {
		mu.Lock()
		defer mu.Unlock()
		clear(failing)
		for _, b := range bases {
			failing[b] = true
		}
	}
	tick := func() {
		t.Helper()
		if err := monitor.TunnelTick(t.Context(), d, sub); err != nil {
			t.Fatal(err)
		}
		clock.add(10 * time.Second)
	}
	waits := func() (w, p *state.SourceWait, qw, qp int) {
		f.s.Memory.View(key, func(tg *state.Target) {
			if x := tg.Waits["warp"]; x != nil {
				c := *x
				w = &c
			}
			if x := tg.Waits["proton"]; x != nil {
				c := *x
				p = &c
			}
			qw = len(tg.Sources["warp"].Selection.Quarantine)
			qp = len(tg.Sources["proton"].Selection.Quarantine)
		})
		return
	}
	tick()
	if w, p, _, _ := waits(); w.Since != nil || p.Since != nil {
		t.Fatal("waiting while healthy")
	}

	// x-balancer empties, x-warp still selects (observatory lag): warp waits,
	// proton goes on failing and is replaced (Н2 lag).
	n := len(srv.Calls())
	srv.SetOverride("x-balancer", "")
	setFail(20000, 20100, 20200)
	tick()
	tick()
	if _, _, _, qp := waits(); qp == 0 || len(addsOf(srv.Calls()[n:], "proton")) == 0 {
		t.Fatalf("no replacement in the lag: quarantine %d", qp)
	}
	srv.SetOverride("x-warp", "")
	tick()
	w, p, qw, qp := waits()
	if w.Since == nil || p.Since == nil || qp != 0 || qw != 0 {
		t.Fatalf("waits %+v %+v quarantine %d %d", w, p, qw, qp)
	}
	if ev := f.logs.events(t, "source_dependency_unhealthy"); len(ev) != 2 {
		t.Fatalf("source_dependency_unhealthy %v", ev)
	}
	n = len(srv.Calls())
	for range 6 {
		tick()
	}
	if a := append(addsOf(srv.Calls()[n:], "warp"), addsOf(srv.Calls()[n:], "proton")...); len(a) != 0 {
		t.Fatalf("replaced while waiting: %q", a)
	}
	if _, _, qw, qp := waits(); qw+qp != 0 {
		t.Fatal("quarantine while waiting")
	}

	// Recovery: x-balancer first; warp tunnels work; x-warp later.
	srv.SetOverride("x-balancer", "x-observe-vless-1")
	setFail(20200)
	tick()
	srv.SetOverride("x-warp", "x-observe-warp-1")
	tick()
	n = len(srv.Calls())
	for range 4 { // 40 s < 60 s pause of proton
		tick()
	}
	if a := addsOf(srv.Calls()[n:], "proton"); len(a) != 0 {
		t.Fatalf("replaced in the pause: %q", a)
	}
	n2 := len(srv.Calls())
	for range 3 {
		tick()
	}
	if rec := f.logs.events(t, "source_dependency_recovered"); len(rec) != 2 {
		t.Fatalf("source_dependency_recovered %v", rec)
	}
	if a := addsOf(srv.Calls()[n2:], "proton"); len(a) == 0 {
		t.Fatalf("no replacement after the pause: %q", srv.Calls()[n:])
	}
	for _, id := range []string{"warp", "proton"} {
		if pk := peakOutbounds(srv.Calls(), id); pk > 2 {
			t.Fatalf("%s peak %d", id, pk)
		}
	}
}

// The paper scenario: warp and proton reach the upper Xray through via-en
// and wait for probe balancer pool-en (selector via-en, leastPing). While
// via-en is dead pool-en selects nothing: both wait, no replacement and
// one GetBalancerInfo of pool-en per tick; after its recovery and the
// pause both waits end, the failure counts are reset and replacements
// resume. maxActive holds throughout.
func TestCascadeBalancerDependencyWait(t *testing.T) {
	all := wgFixtureLines(t)
	dir := t.TempDir()
	pw, pp := filepath.Join(dir, "warp"), filepath.Join(dir, "proton")
	writeCandidates(t, pw, all, 1, 2, 4)
	writeCandidates(t, pp, all, 5, 6, 1, 2)
	sub := wgSub(pw)
	warp := sub.Sources[0]
	warp.ID = "warp"
	warp.Balancer = "pool-vpn"
	warp.DialerProxy = "via-en"
	warp.Selection.ReplaceAfterFailures = 2
	warp.DependsOn = []config.DependencyConfig{{Balancer: "pool-en", Health: config.HealthBalancer, RecoverGraceSeconds: 60}}
	proton := warp
	sel := *warp.Selection
	proton.Selection = &sel
	proton.ID, proton.Input = "proton", pp
	proton.PortRange = &config.PortRange{Start: 20200, End: 20203}
	sub.Sources = []config.SourceConfig{warp, proton}
	sub.Target.FixedOutbounds = []string{"via-en"}
	sub.Target.Monitor = config.TargetMonitorConfig{
		Enabled: true, MaxParallel: 4, Retry: config.RetryConfig{Attempts: 1},
		Request: &config.TargetMonitorRequestConfig{URL: "https://example.test/", Method: "GET", ExpectedStatus: 200, TimeoutMs: 1000},
	}
	f := newSourcesFixture(t, sub, nil,
		xrayfake.WithStaticOutbounds("via-en"),
		xrayfake.WithBalancer("pool-vpn", []string{"x-observe-warp-", "x-observe-proton-"}, "random"),
		xrayfake.WithBalancer("pool-en", []string{"via-en"}, "leastPing"))
	srv := f.x.servers[addrA]
	enAlive := func(alive bool) {
		srv.SetObservation(&observatory.OutboundStatus{OutboundTag: "via-en", Alive: alive, Delay: 50})
	}
	enAlive(true)
	if r := f.run(t); r.Failed != 0 {
		t.Fatalf("report %+v", r.Targets)
	}
	key := state.Key("sub", addrA)
	var mu sync.Mutex
	failing := map[int]bool{}
	clock := &cascadeClock{now: time.Now()}
	d := monitor.Deps{
		Memory: f.s.Memory, Log: f.s.Log, Dial: f.s.Dial, Now: clock.Now,
		Probe: func(_ context.Context, r socks.Request) (socks.Response, error) {
			mu.Lock()
			defer mu.Unlock()
			if failing[r.ProxyPort/100*100] {
				return socks.Response{}, errors.New("probe failed")
			}
			return socks.Response{StatusCode: 200}, nil
		},
		Sleep: func(context.Context, time.Duration) error { return nil },
	}
	setFail := func(bases ...int) {
		mu.Lock()
		defer mu.Unlock()
		clear(failing)
		for _, b := range bases {
			failing[b] = true
		}
	}
	tick := func() {
		t.Helper()
		if err := monitor.TunnelTick(t.Context(), d, sub); err != nil {
			t.Fatal(err)
		}
		clock.add(10 * time.Second)
	}
	waits := func() (w, p *state.SourceWait, q int) {
		f.s.Memory.View(key, func(tg *state.Target) {
			if x := tg.Waits["warp"]; x != nil {
				c := *x
				w = &c
			}
			if x := tg.Waits["proton"]; x != nil {
				c := *x
				p = &c
			}
			q = len(tg.Sources["warp"].Selection.Quarantine) + len(tg.Sources["proton"].Selection.Quarantine)
		})
		return
	}
	tick()
	if w, p, _ := waits(); w.Since != nil || p.Since != nil {
		t.Fatal("waiting while healthy")
	}

	// en goes down: pool-en selects nothing, every tunnel fails.
	enAlive(false)
	setFail(20100, 20200)
	tick()
	tick()
	w, p, q := waits()
	if w.Since == nil || p.Since == nil || q != 0 {
		t.Fatalf("waits %+v %+v quarantine %d", w, p, q)
	}
	ev := f.logs.events(t, "source_dependency_unhealthy")
	if len(ev) != 2 {
		t.Fatalf("source_dependency_unhealthy %v", ev)
	}
	for _, e := range ev {
		if dep, _ := e["dependsOn"].([]any); len(dep) != 1 || dep[0] != "balancer:pool-en" {
			t.Fatalf("dependsOn %v", e["dependsOn"])
		}
	}
	n := len(srv.Calls())
	for range 6 {
		tick()
	}
	if a := append(addsOf(srv.Calls()[n:], "warp"), addsOf(srv.Calls()[n:], "proton")...); len(a) != 0 {
		t.Fatalf("replaced while waiting: %q", a)
	}
	if c := count(srv.Calls()[n:], "GetBalancerInfo:pool-en"); c != 6 {
		t.Fatalf("GetBalancerInfo:pool-en %d in 6 ticks", c)
	}
	if _, _, q := waits(); q != 0 {
		t.Fatal("quarantine while waiting")
	}

	// en is back; proton's tunnels still fail: no replacement in the pause.
	enAlive(true)
	setFail(20200)
	tick()
	n = len(srv.Calls())
	for range 4 { // 40 s < 60 s pause
		tick()
	}
	if a := addsOf(srv.Calls()[n:], "proton"); len(a) != 0 {
		t.Fatalf("replaced in the pause: %q", a)
	}
	n2 := len(srv.Calls())
	for range 3 {
		tick()
	}
	rec := f.logs.events(t, "source_dependency_recovered")
	if len(rec) != 2 {
		t.Fatalf("source_dependency_recovered %v", rec)
	}
	for _, e := range rec {
		if r, _ := e["failuresReset"].(float64); e["sourceId"] == "proton" && r == 0 {
			t.Fatalf("proton recovered without a reset: %v", e)
		}
	}
	if w, p, _ := waits(); w.ResetAt == nil || p.ResetAt == nil {
		t.Fatalf("ResetAt %+v %+v", w, p)
	}
	if a := addsOf(srv.Calls()[n2:], "proton"); len(a) == 0 {
		t.Fatalf("no replacement after the pause: %q", srv.Calls()[n:])
	}
	for _, id := range []string{"warp", "proton"} {
		if pk := peakOutbounds(srv.Calls(), id); pk > 2 {
			t.Fatalf("%s peak %d", id, pk)
		}
	}
}

// The cross-Xray scenario: warp and proton of blanc-ru wait for balancer
// pool-blanc of blanc-en, another Xray, read from the balancer selection
// state of blanc-en (written by balancer.Tick only; blanc-en is disabled
// for the syncer). Neither the syncer nor the monitor of blanc-ru dials
// the Xray of blanc-en. While the pool selects nothing or its Xray is
// unavailable both wait; after its recovery and the pause both waits end
// and replacements resume. A pool-blanc missing in that Xray blocks the
// sync of both until it is back.
func TestCascadeCrossBalancerWait(t *testing.T) {
	const addrRU, addrEN = addrA, addrB
	all := wgFixtureLines(t)
	dir := t.TempDir()
	pw, pp := filepath.Join(dir, "warp"), filepath.Join(dir, "proton")
	writeCandidates(t, pw, all, 1, 2, 4)
	writeCandidates(t, pp, all, 5, 6, 1, 2)
	ru := wgSub(pw)
	ru.ID = "blanc-ru"
	warp := ru.Sources[0]
	warp.ID = "warp"
	warp.Balancer = "pool-vpn"
	warp.DialerProxy = "via-en"
	warp.Selection.ReplaceAfterFailures = 2
	warp.DependsOn = []config.DependencyConfig{{Subscription: "blanc-en", Balancer: "pool-blanc", Health: config.HealthBalancer, RecoverGraceSeconds: 60}}
	proton := warp
	sel := *warp.Selection
	proton.Selection = &sel
	proton.ID, proton.Input = "proton", pp
	proton.PortRange = &config.PortRange{Start: 20200, End: 20203}
	ru.Sources = []config.SourceConfig{warp, proton}
	ru.Target.FixedOutbounds = []string{"via-en"}
	ru.Target.Monitor = config.TargetMonitorConfig{
		Enabled: true, MaxParallel: 4, Retry: config.RetryConfig{Attempts: 1},
		Request: &config.TargetMonitorRequestConfig{URL: "https://example.test/", Method: "GET", ExpectedStatus: 200, TimeoutMs: 1000},
	}
	en := sub("blanc-en", "/en", target(addrEN))
	en.Enabled = false
	en.Target.Balancers = []config.BalancerConfig{{Tag: "pool-blanc", Selection: &config.BalancerSelectionConfig{Enabled: true, Schedule: "*/15 * * * * *"}}}

	f := newFixture(t, cfg(allKinds, ru, en), nil)
	srv, _ := xrayfake.Start(t,
		xrayfake.WithStaticOutbounds("via-en"),
		xrayfake.WithBalancer("pool-vpn", []string{"x-observe-warp-", "x-observe-proton-"}, "random"))
	f.x.servers[addrRU] = srv
	enSrv, _ := xrayfake.Start(t,
		xrayfake.WithStaticOutbounds("x-observe-blanc-1"),
		xrayfake.WithBalancer("pool-blanc", []string{"x-observe-blanc-"}, "leastPing"))
	enAlive := func(alive bool) {
		enSrv.SetObservation(&observatory.OutboundStatus{OutboundTag: "x-observe-blanc-1", Alive: alive, Delay: 50})
	}
	enAlive(true)
	clock := &cascadeClock{now: time.Now()}
	f.s.Now = clock.Now
	if r := f.run(t); r.Failed != 0 {
		t.Fatalf("report %+v", r.Targets)
	}
	key := state.Key("blanc-ru", addrRU)
	var mu sync.Mutex
	failing := map[int]bool{}
	d := monitor.Deps{
		Memory: f.s.Memory, Log: f.s.Log, Now: clock.Now,
		Subscriptions: f.s.Config.Config.Subscriptions,
		Dial: func(addr string, timeout time.Duration) (xrayapi.Client, error) {
			if addr == addrEN {
				t.Fatalf("the monitor of blanc-ru dialed %s", addr)
			}
			return f.s.Dial(addr, timeout)
		},
		Probe: func(_ context.Context, r socks.Request) (socks.Response, error) {
			mu.Lock()
			defer mu.Unlock()
			if failing[r.ProxyPort/100*100] {
				return socks.Response{}, errors.New("probe failed")
			}
			return socks.Response{StatusCode: 200}, nil
		},
		Sleep: func(context.Context, time.Duration) error { return nil },
	}
	bd := balancer.Deps{Memory: f.s.Memory, Log: f.s.Log, Dial: enSrv.Dialer(), Now: clock.Now}
	setFail := func(bases ...int) {
		mu.Lock()
		defer mu.Unlock()
		clear(failing)
		for _, b := range bases {
			failing[b] = true
		}
	}
	selTick := func() {
		t.Helper()
		if err := balancer.Tick(t.Context(), bd, en, en.Target.Balancers[0]); err != nil {
			t.Fatal(err)
		}
	}
	tick := func() {
		t.Helper()
		selTick()
		if err := monitor.TunnelTick(t.Context(), d, ru); err != nil {
			t.Fatal(err)
		}
		clock.add(10 * time.Second)
	}
	waits := func() (w, p *state.SourceWait, q int) {
		f.s.Memory.View(key, func(tg *state.Target) {
			if x := tg.Waits["warp"]; x != nil {
				c := *x
				w = &c
			}
			if x := tg.Waits["proton"]; x != nil {
				c := *x
				p = &c
			}
			q = len(tg.Sources["warp"].Selection.Quarantine) + len(tg.Sources["proton"].Selection.Quarantine)
		})
		return
	}
	const dep = "balancer:blanc-en/pool-blanc"
	// noCandidateEvents fails on a candidate_replaced or candidate_removed
	// of warp or proton logged after the first skip lines.
	noCandidateEvents := func(skip map[string]int) {
		t.Helper()
		for _, name := range []string{"candidate_replaced", "candidate_removed"} {
			if ev := f.logs.events(t, name); len(ev) > skip[name] {
				t.Fatalf("%s while waiting: %v", name, ev[skip[name]:])
			}
		}
	}
	candidateCounts := func() map[string]int {
		return map[string]int{
			"candidate_replaced": len(f.logs.events(t, "candidate_replaced")),
			"candidate_removed":  len(f.logs.events(t, "candidate_removed")),
		}
	}
	tick()
	if w, p, _ := waits(); w.Since != nil || p.Since != nil {
		t.Fatal("waiting while healthy")
	}

	// The pool of en selects nothing, every tunnel of ru fails.
	enAlive(false)
	setFail(20100, 20200)
	skip := candidateCounts()
	tick()
	tick()
	w, p, q := waits()
	if w.Since == nil || p.Since == nil || q != 0 {
		t.Fatalf("waits %+v %+v quarantine %d", w, p, q)
	}
	ev := f.logs.events(t, "source_dependency_unhealthy")
	if len(ev) != 2 {
		t.Fatalf("source_dependency_unhealthy %v", ev)
	}
	for _, e := range ev {
		if deps, _ := e["dependsOn"].([]any); len(deps) != 1 || deps[0] != dep {
			t.Fatalf("dependsOn %v", e["dependsOn"])
		}
	}
	n := len(srv.Calls())
	for range 6 {
		tick()
	}
	if a := append(addsOf(srv.Calls()[n:], "warp"), addsOf(srv.Calls()[n:], "proton")...); len(a) != 0 {
		t.Fatalf("replaced while waiting: %q", a)
	}
	noCandidateEvents(skip)

	// The Xray of en is unavailable: unknown, the waits hold.
	for range 3 {
		enSrv.FailNext("GetBalancerInfo", status.Error(codes.Unavailable, "connection refused"))
		tick()
	}
	w, p, _ = waits()
	if w.Since == nil || p.Since == nil || w.Deps[dep].State != state.DepUnknown {
		t.Fatalf("waits %+v %+v", w, p)
	}
	for _, e := range f.logs.events(t, "xray_unavailable") {
		if e["subscriptionId"] == "blanc-ru" {
			t.Fatalf("xray_unavailable of blanc-ru: %v", e)
		}
	}
	noCandidateEvents(skip)

	// en is back with a live pool; proton's tunnels still fail: no
	// replacement in the pause.
	enAlive(true)
	setFail(20200)
	tick()
	n = len(srv.Calls())
	for range 4 { // 40 s < 60 s pause
		tick()
	}
	if a := addsOf(srv.Calls()[n:], "proton"); len(a) != 0 {
		t.Fatalf("replaced in the pause: %q", a)
	}
	noCandidateEvents(skip)
	n2 := len(srv.Calls())
	for range 3 {
		tick()
	}
	if rec := f.logs.events(t, "source_dependency_recovered"); len(rec) != 2 {
		t.Fatalf("source_dependency_recovered %v", rec)
	}
	if w, p, _ := waits(); w.ResetAt == nil || p.ResetAt == nil {
		t.Fatalf("ResetAt %+v %+v", w, p)
	}
	if a := addsOf(srv.Calls()[n2:], "proton"); len(a) == 0 {
		t.Fatalf("no replacement after the pause: %q", srv.Calls()[n:])
	}
	for _, id := range []string{"warp", "proton"} {
		if pk := peakOutbounds(srv.Calls(), id); pk > 2 {
			t.Fatalf("%s peak %d", id, pk)
		}
	}

	// pool-blanc missing in the Xray of en blocks the sync of both sources.
	enSrv.RemoveBalancer("pool-blanc")
	selTick()
	n = len(srv.Calls())
	if r := f.run(t); r.Failed == 0 {
		t.Fatalf("report %+v", r.Targets)
	}
	for _, c := range srv.Calls()[n:] {
		if touchesSource(c, "warp") || touchesSource(c, "proton") {
			t.Fatalf("RPC on a blocked source: %q", srv.Calls()[n:])
		}
	}
	failed := map[string]bool{}
	for _, e := range f.logs.events(t, "source_check_failed") {
		if e["reasonCode"] == CodeBalancerNotFound {
			failed[e["sourceId"].(string)] = true
		}
	}
	if !failed["warp"] || !failed["proton"] {
		t.Fatalf("source_check_failed %v", f.logs.events(t, "source_check_failed"))
	}
	enSrv.AddBalancer("pool-blanc", []string{"x-observe-blanc-"}, "leastPing")
	selTick()
	if r := f.run(t); r.Failed != 0 {
		t.Fatalf("report %+v", r.Targets)
	}
	if rec := f.logs.events(t, "source_check_recovered"); len(rec) != 2 {
		t.Fatalf("source_check_recovered %v", rec)
	}

	if dl := f.x.dials(); slices.Contains(dl, addrEN) {
		t.Fatalf("dials %v", dl)
	}
	for _, c := range enSrv.Calls() {
		if c != "GetBalancerInfo:pool-blanc" {
			t.Fatalf("calls of en %q", enSrv.Calls())
		}
	}
}
