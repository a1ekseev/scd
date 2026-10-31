package syncer

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a1ekseev/scd/internal/scd/config"
	"github.com/a1ekseev/scd/internal/scd/monitor"
	"github.com/a1ekseev/scd/internal/scd/socks"
	"github.com/a1ekseev/scd/internal/scd/state"
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
