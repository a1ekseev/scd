package syncer

import (
	"context"
	"path/filepath"
	"slices"
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

// A monitor tick that starts a dependency wait while a sync of the source
// holds the target lock: the quarantine release waits for the lock, so the
// selection state the sync writes back does not bring the released pair
// back. Never more than maxActive outbounds of the source. Run with -race.
func TestWaitReleaseWithConcurrentSync(t *testing.T) {
	all := wgFixtureLines(t)
	path := filepath.Join(t.TempDir(), "wg")
	writeCandidates(t, path, all, 1, 2, 4)
	sub := wgSub(path)
	up := source("up", "/up")
	up.Balancer = "x-up"
	up.PortRange = &config.PortRange{Start: 20000, End: 20010}
	sub.Sources[0].DependsOn = []config.DependencyConfig{{Source: "up", Health: config.HealthBalancer, RecoverGraceSeconds: 180}}
	sub.Sources = append(sub.Sources, up)
	sub.Target.Monitor = config.TargetMonitorConfig{
		Enabled: true, MaxParallel: 2, Retry: config.RetryConfig{Attempts: 1},
		Request: &config.TargetMonitorRequestConfig{URL: "https://example.test/", Method: "GET", ExpectedStatus: 200, TimeoutMs: 1000},
	}
	f := newSourcesFixture(t, sub, map[string]string{"/up": vienna}, xrayfake.WithBalancer("x-up", []string{"none-"}, "random"))
	srv := f.x.servers[addrA]
	if r := f.run(t); r.Failed != 0 {
		t.Fatalf("report %+v", r.Targets)
	}
	key := state.Key("sub", addrA)
	f.s.Memory.Update(key, func(tg *state.Target) {
		st := tg.Sources["wg"]
		st.Selection.QuarantinePair(st.Candidates[len(st.Candidates)-1].SelectionCandidate(), time.Now(), 4)
	})

	// The next sync replaces a candidate and holds the lock in AddInbound,
	// after it took its copy of the selection state.
	writeCandidates(t, path, all, 1, 4, 5)
	srv.Delay("AddInbound", 300*time.Millisecond)
	d := monitor.Deps{
		Memory: f.s.Memory, Log: f.s.Log, Dial: f.s.Dial,
		Probe: func(context.Context, socks.Request) (socks.Response, error) {
			return socks.Response{StatusCode: 200}, nil
		},
		Sleep: func(context.Context, time.Duration) error { return nil },
	}
	n := len(srv.Calls())
	var wg sync.WaitGroup
	wg.Go(func() {
		if _, err := f.s.Run(t.Context()); err != nil {
			t.Error(err)
		}
	})
	for deadline := time.Now().Add(5 * time.Second); !slices.ContainsFunc(srv.Calls()[n:], func(c string) bool {
		return strings.HasPrefix(c, "AddInbound:")
	}); {
		if time.Now().After(deadline) {
			t.Fatal("the sync did not reach AddInbound")
		}
		time.Sleep(time.Millisecond)
	}
	if err := monitor.TunnelTick(t.Context(), d, sub); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	srv.Delay("AddInbound", 0)

	f.s.Memory.View(key, func(tg *state.Target) {
		if q := tg.Sources["wg"].Selection.Quarantine; len(q) != 0 {
			t.Errorf("quarantine %v after the release", q)
		}
	})
	if ev := f.logs.events(t, "source_dependency_unhealthy"); len(ev) != 1 || ev[0]["quarantineReleased"] != 1.0 {
		t.Fatalf("source_dependency_unhealthy %v", ev)
	}
	if p := peakOutbounds(srv.Calls(), "wg"); p > 2 {
		t.Fatalf("peak %d > maxActive 2: %q", p, srv.Calls())
	}
}
