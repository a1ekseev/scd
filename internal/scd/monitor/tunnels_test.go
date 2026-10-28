package monitor

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/a1ekseev/scd/internal/scd/config"
	"github.com/a1ekseev/scd/internal/scd/socks"
	"github.com/a1ekseev/scd/internal/scd/state"
	"github.com/a1ekseev/scd/internal/scd/xrayapi"
)

// waitFor polls cond until it holds or 5 s pass. It may run on worker
// goroutines, so it reports a timeout with t.Errorf and returns false.
func waitFor(t *testing.T, what string, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Errorf("timed out waiting for %s", what)
			return false
		}
		time.Sleep(time.Millisecond)
	}
	return true
}

// storeMax raises m to n atomically.
func storeMax(m *atomic.Int32, n int32) {
	for {
		o := m.Load()
		if n <= o || m.CompareAndSwap(o, n) {
			return
		}
	}
}

// closeHook wraps a client and calls onClose when it is closed.
type closeHook struct {
	xrayapi.Client
	onClose func()
}

func (c closeHook) Close() error {
	c.onClose()
	return c.Client.Close()
}

// 03 §10.6 p.1.
func TestProbesParallelRepairsSerialized(t *testing.T) {
	e := newEnv(t, newSub(nil), linkAT, linkBE)
	var (
		started, inFlight, maxProbes atomic.Int32
		repairs, maxRepairs          atomic.Int32
	)
	e.deps.Probe = func(context.Context, socks.Request) (socks.Response, error) {
		started.Add(1)
		n := inFlight.Add(1)
		storeMax(&maxProbes, n)
		// Both probes must be in flight at once for the tick to finish.
		waitFor(t, "second probe", func() bool { return started.Load() == 2 })
		inFlight.Add(-1)
		return socks.Response{}, errors.New("probe failed")
	}
	// A repair spans Dial..Close under the target lock.
	e.onDial = func() {
		n := repairs.Add(1)
		storeMax(&maxRepairs, n)
		time.Sleep(10 * time.Millisecond)
	}
	e.wrap = func(c xrayapi.Client) xrayapi.Client {
		return closeHook{Client: c, onClose: func() { repairs.Add(-1) }}
	}

	if err := TunnelTick(t.Context(), e.deps, e.sub); err != nil {
		t.Fatal(err)
	}
	if started.Load() != 2 || maxProbes.Load() != 2 {
		t.Errorf("probes started=%d max concurrent=%d, want 2/2", started.Load(), maxProbes.Load())
	}
	if e.dials.Load() != 2 || maxRepairs.Load() != 1 {
		t.Errorf("repairs=%d max concurrent=%d, want 2/1", e.dials.Load(), maxRepairs.Load())
	}
}

// 03 §10.6 p.2.
func TestRespectsMaxParallel(t *testing.T) {
	sub := newSub(func(c *config.SubscriptionConfig) { c.Target.Monitor.MaxParallel = 2 })
	e := newEnv(t, sub, linkAT, linkBE, linkBG)
	var started, inFlight, maxInFlight atomic.Int32
	e.deps.Probe = func(_ context.Context, r socks.Request) (socks.Response, error) {
		started.Add(1)
		n := inFlight.Add(1)
		storeMax(&maxInFlight, n)
		// Hold the slot until two probes overlap (or the last one runs alone).
		waitFor(t, "overlap", func() bool { return inFlight.Load() >= 2 || started.Load() == 3 })
		time.Sleep(2 * time.Millisecond)
		inFlight.Add(-1)
		if r.ProxyPort == 20000 {
			return socks.Response{}, errors.New("probe failed")
		}
		return socks.Response{StatusCode: 200, LatencyMs: 25, BodyBytes: 100}, nil
	}

	if err := TunnelTick(t.Context(), e.deps, e.sub); err != nil {
		t.Fatal(err)
	}
	if maxInFlight.Load() != 2 {
		t.Errorf("max concurrent = %d, want 2", maxInFlight.Load())
	}
	if e.dials.Load() != 1 {
		t.Errorf("repairs = %d, want 1", e.dials.Load())
	}
	tns := e.tunnels()
	if tns[0].Monitor.State != state.TunnelDegraded {
		t.Errorf("first tunnel = %s, want degraded", tns[0].Monitor.State)
	}
	if tns[1].Monitor.State != state.TunnelHealthy || tns[2].Monitor.State != state.TunnelHealthy {
		t.Errorf("others = %s, %s, want healthy", tns[1].Monitor.State, tns[2].Monitor.State)
	}
}

// 03 §10.6 p.3.
func TestClearsCurrentOnFailureKeepsLastSuccess(t *testing.T) {
	e := newEnv(t, newSub(nil), linkAT)
	e.deps.Probe = ok(200, 25)
	if err := TunnelTick(t.Context(), e.deps, e.sub); err != nil {
		t.Fatal(err)
	}
	m := e.tunnel(0).Monitor
	if m.State != state.TunnelHealthy || deref(m.LastStatusCode) != 200 || deref(m.LastLatencyMs) != 25 ||
		deref(m.LastSuccessStatusCode) != 200 || deref(m.LastSuccessLatencyMs) != 25 {
		t.Fatalf("after success = %+v", m)
	}
	if m.LastCheckedAt == nil || m.LastSuccessAt == nil || !m.LastCheckedAt.Equal(*m.LastSuccessAt) {
		t.Errorf("timestamps = %v / %v", m.LastCheckedAt, m.LastSuccessAt)
	}

	e.deps.Probe = func(context.Context, socks.Request) (socks.Response, error) {
		return socks.Response{}, errors.New("probe failed")
	}
	var atRepair state.TunnelMonitor
	e.onDial = func() { atRepair = e.tunnel(0).Monitor }
	if err := TunnelTick(t.Context(), e.deps, e.sub); err != nil {
		t.Fatal(err)
	}
	// The probe failure as recorded before the repair started.
	if atRepair.State != state.TunnelRepairing || atRepair.LastError != "probe failed" || atRepair.ConsecutiveFailures != 1 {
		t.Errorf("at repair = %+v", atRepair)
	}
	if atRepair.LastFailureAt == nil || atRepair.LastCheckedAt == nil || !atRepair.LastFailureAt.Equal(*atRepair.LastCheckedAt) {
		t.Errorf("failure timestamps = %v / %v", atRepair.LastCheckedAt, atRepair.LastFailureAt)
	}
	m = e.tunnel(0).Monitor
	if m.State != state.TunnelDegraded || m.LastStatusCode != nil || m.LastLatencyMs != nil {
		t.Errorf("after failure = %+v", m)
	}
	if deref(m.LastSuccessStatusCode) != 200 || deref(m.LastSuccessLatencyMs) != 25 || m.LastSuccessAt == nil {
		t.Errorf("last success lost: %+v", m)
	}
}

// 03 §10.6 p.4.
func TestRetriesBeforeDegraded(t *testing.T) {
	sub := newSub(func(c *config.SubscriptionConfig) {
		c.Target.Monitor.Retry = config.RetryConfig{Attempts: 2, DelayMs: 250}
	})
	e := newEnv(t, sub, linkAT)
	var calls atomic.Int32
	e.deps.Probe = func(context.Context, socks.Request) (socks.Response, error) {
		if calls.Add(1) == 1 {
			return socks.Response{}, errors.New("temporary probe failure")
		}
		return socks.Response{StatusCode: 200, LatencyMs: 25}, nil
	}
	var sleeps []time.Duration
	e.deps.Sleep = func(_ context.Context, d time.Duration) error {
		sleeps = append(sleeps, d)
		return nil
	}

	if err := TunnelTick(t.Context(), e.deps, e.sub); err != nil {
		t.Fatal(err)
	}
	m := e.tunnel(0).Monitor
	if calls.Load() != 2 || e.dials.Load() != 0 {
		t.Errorf("calls=%d repairs=%d, want 2/0", calls.Load(), e.dials.Load())
	}
	if m.State != state.TunnelHealthy || deref(m.LastLatencyMs) != 25 || m.ConsecutiveFailures != 0 {
		t.Errorf("monitor = %+v", m)
	}
	if !slices.Equal(sleeps, []time.Duration{250 * time.Millisecond}) {
		t.Errorf("sleeps = %v", sleeps)
	}
	logs := e.logs.events(t, "monitor_check_retry")
	if len(logs) != 1 {
		t.Fatalf("retry logs = %v", logs)
	}
	l := logs[0]
	if l["level"] != "DEBUG" || l["attempt"] != 1.0 || l["attempts"] != 2.0 || l["delayMs"] != 250.0 ||
		l["error"] != "temporary probe failure" || l["subscriptionId"] != "source-1" ||
		l["targetAddress"] != "127.0.0.1:8080" || l["tunnelId"] != e.tunnel(0).Mapping.BaseTunnelID {
		t.Errorf("retry log = %v", l)
	}
	if _, has := l["statusCode"]; has {
		t.Errorf("retry log has statusCode without a response: %v", l)
	}
}

// 03 §10.6 p.5.
func TestRetriesUnexpectedStatusThenRepairs(t *testing.T) {
	sub := newSub(func(c *config.SubscriptionConfig) {
		c.Target.Monitor.Retry = config.RetryConfig{Attempts: 2}
		c.Target.Monitor.Request.ExpectedStatus = 204
	})
	e := newEnv(t, sub, linkAT)
	var calls atomic.Int32
	e.deps.Probe = func(ctx context.Context, r socks.Request) (socks.Response, error) {
		calls.Add(1)
		return ok(200, 25)(ctx, r)
	}
	var atRepair state.TunnelMonitor
	e.onDial = func() { atRepair = e.tunnel(0).Monitor }

	if err := TunnelTick(t.Context(), e.deps, e.sub); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 || e.dials.Load() != 1 {
		t.Errorf("calls=%d repairs=%d, want 2/1", calls.Load(), e.dials.Load())
	}
	if atRepair.LastError != "Expected HTTP 204, got 200." {
		t.Errorf("lastError at repair = %q", atRepair.LastError)
	}
	m := e.tunnel(0).Monitor
	if m.State != state.TunnelDegraded || m.LastLatencyMs != nil {
		t.Errorf("monitor = %+v", m)
	}
	logs := e.logs.events(t, "monitor_check_retry")
	if len(logs) != 1 || logs[0]["attempt"] != 1.0 || logs[0]["attempts"] != 2.0 ||
		logs[0]["statusCode"] != 200.0 || logs[0]["error"] != "Expected HTTP 204, got 200." {
		t.Errorf("retry logs = %v", logs)
	}
}

// 03 §10.6 p.6: retries stay inside the worker slots (synctest: probes take
// 5 ms of fake time).
func TestRetryInsideWorkerSlots(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sub := newSub(func(c *config.SubscriptionConfig) {
			c.Target.Monitor.MaxParallel = 2
			c.Target.Monitor.Retry = config.RetryConfig{Attempts: 2}
		})
		mem := state.New()
		key := state.Key(sub.ID, sub.Target.Address)
		mem.ReplaceSourceTopology(key, "vless", topoOf(t, sub, linkAT, linkBE, linkBG), nil)
		var (
			mu                  sync.Mutex
			inFlight, maxFlight int
			byPort              = map[int]int{}
		)
		d := Deps{
			Memory: mem,
			Log:    discardLog(),
			Dial: func(string, time.Duration) (xrayapi.Client, error) {
				t.Error("unexpected dial")
				return nil, errors.New("no dial")
			},
			Probe: func(_ context.Context, r socks.Request) (socks.Response, error) {
				mu.Lock()
				inFlight++
				maxFlight = max(maxFlight, inFlight)
				mu.Unlock()
				time.Sleep(5 * time.Millisecond)
				mu.Lock()
				defer mu.Unlock()
				inFlight--
				byPort[r.ProxyPort]++
				if byPort[r.ProxyPort] == 1 {
					return socks.Response{}, errors.New("temporary probe failure")
				}
				return socks.Response{StatusCode: 200, LatencyMs: 25}, nil
			},
		}
		if err := TunnelTick(t.Context(), d, sub); err != nil {
			t.Fatal(err)
		}
		if maxFlight != 2 {
			t.Errorf("max concurrent = %d, want 2", maxFlight)
		}
		if byPort[20000] != 2 || byPort[20001] != 2 || byPort[20002] != 2 || len(byPort) != 3 {
			t.Errorf("calls by port = %v", byPort)
		}
		mem.View(key, func(tg *state.Target) {
			for _, tn := range tg.Tunnels {
				if tn.Monitor.State != state.TunnelHealthy {
					t.Errorf("%s = %s", tn.Mapping.BaseTunnelID, tn.Monitor.State)
				}
			}
		})
	})
}

// 03 §10.6 p.14.
func TestUnexpectedStatusNoStaleSuccess(t *testing.T) {
	e := newEnv(t, newSub(nil), linkAT)
	e.deps.Probe = ok(204, 31)
	var atRepair state.TunnelMonitor
	e.onDial = func() { atRepair = e.tunnel(0).Monitor }
	if err := TunnelTick(t.Context(), e.deps, e.sub); err != nil {
		t.Fatal(err)
	}
	if atRepair.LastError != "Expected HTTP 200, got 204." {
		t.Errorf("lastError at repair = %q", atRepair.LastError)
	}
	m := e.tunnel(0).Monitor
	if m.State != state.TunnelDegraded || m.LastStatusCode != nil || m.LastLatencyMs != nil ||
		m.LastSuccessStatusCode != nil || m.LastSuccessAt != nil {
		t.Errorf("monitor = %+v", m)
	}
}

// 03 §10.6 p.15: an error from repair (here: the mutation client cannot be
// created) fails the tick only after every probe settled.
func TestRepairErrorPropagatesAfterProbes(t *testing.T) {
	e := newEnv(t, newSub(nil), linkAT, linkBE)
	var probes atomic.Int32
	e.deps.Probe = func(context.Context, socks.Request) (socks.Response, error) {
		probes.Add(1)
		return socks.Response{}, errors.New("probe failed")
	}
	dialErr := errors.New("unexpected repair exception")
	e.deps.Dial = func(string, time.Duration) (xrayapi.Client, error) { return nil, dialErr }

	err := TunnelTick(t.Context(), e.deps, e.sub)
	if err == nil || !strings.Contains(err.Error(), "concurrent task execution failed") {
		t.Fatalf("err = %v", err)
	}
	if !errors.Is(err, dialErr) {
		t.Errorf("err does not wrap the repair error: %v", err)
	}
	var multi interface{ Unwrap() []error }
	if !errors.As(err, &multi) || len(multi.Unwrap()) != 2 {
		t.Errorf("causes = %v", err)
	}
	if probes.Load() != 2 {
		t.Errorf("probes = %d, want 2", probes.Load())
	}
}

func TestTickNoopWithoutRequestOrTarget(t *testing.T) {
	cases := map[string]func(c *config.SubscriptionConfig){
		"disabled":   func(c *config.SubscriptionConfig) { c.Target.Monitor.Enabled = false },
		"no request": func(c *config.SubscriptionConfig) { c.Target.Monitor.Request = nil },
		"no target":  func(c *config.SubscriptionConfig) { c.ID = "other" },
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, newSub(nil), linkAT)
			if err := TunnelTick(t.Context(), e.deps, newSub(mut)); err != nil {
				t.Fatal(err)
			}
			if e.tunnel(0).Monitor.State != state.TunnelIdle {
				t.Errorf("state = %s", e.tunnel(0).Monitor.State)
			}
		})
	}
}

// A cancelled tick (daemon shutdown) neither marks tunnels degraded nor
// starts repairs (D7).
func TestTickCancelledSkipsRepair(t *testing.T) {
	e := newEnv(t, newSub(nil), linkAT)
	ctx, cancel := context.WithCancel(t.Context())
	e.deps.Probe = func(ctx context.Context, _ socks.Request) (socks.Response, error) {
		cancel()
		<-ctx.Done()
		return socks.Response{}, textError("SOCKS HTTP request timed out after 5000ms.")
	}
	err := TunnelTick(ctx, e.deps, e.sub)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if e.dials.Load() != 0 || e.tunnel(0).Monitor.State != state.TunnelIdle {
		t.Errorf("dials=%d state=%s", e.dials.Load(), e.tunnel(0).Monitor.State)
	}
}

func TestProbeRequestUsesTunnelSocks(t *testing.T) {
	sub := newSub(func(c *config.SubscriptionConfig) {
		c.Target.Address = "xray.internal:8080"
		c.Target.InboundSocks.Listen = "0.0.0.0"
		c.Target.Monitor.Request = &config.TargetMonitorRequestConfig{
			URL: "https://probe.example.test/generate_204", Method: "HEAD", ExpectedStatus: 204, TimeoutMs: 1500,
		}
	})
	e := newEnv(t, sub, linkAT)
	var got []socks.Request
	e.deps.Probe = func(_ context.Context, r socks.Request) (socks.Response, error) {
		got = append(got, r)
		return socks.Response{StatusCode: 204, LatencyMs: 7}, nil
	}
	if err := TunnelTick(t.Context(), e.deps, e.sub); err != nil {
		t.Fatal(err)
	}
	want := socks.Request{
		ProxyHost: "xray.internal", ProxyPort: 20000, URL: "https://probe.example.test/generate_204",
		Method: "HEAD", Timeout: 1500 * time.Millisecond,
	}
	if len(got) != 1 || got[0].ProxyHost != want.ProxyHost || got[0].ProxyPort != want.ProxyPort ||
		got[0].URL != want.URL || got[0].Method != want.Method || got[0].Timeout != want.Timeout {
		t.Errorf("requests = %+v, want %+v", got, want)
	}
}

func TestResolveProxyHost(t *testing.T) {
	cases := []struct{ listen, target, want string }{
		{"127.0.0.1", "xray:8080", "127.0.0.1"},
		{"0.0.0.0", "xray:8080", "xray"},
		{"::", "10.0.0.5:8080", "10.0.0.5"},
		{"0.0.0.0", "xray", "xray"},
	}
	for _, c := range cases {
		if got := ResolveProxyHost(c.listen, c.target); got != c.want {
			t.Errorf("ResolveProxyHost(%q, %q) = %q, want %q", c.listen, c.target, got, c.want)
		}
	}
}

// The TS split(':')[0] broke on IPv6 target addresses; net.SplitHostPort
// does not.
func TestResolveProxyHostIPv6Target(t *testing.T) {
	for _, listen := range []string{"0.0.0.0", "::"} {
		if got := ResolveProxyHost(listen, "[fd00::5]:8080"); got != "fd00::5" {
			t.Errorf("ResolveProxyHost(%q, [fd00::5]:8080) = %q, want fd00::5", listen, got)
		}
	}
	if got := ResolveProxyHost("::1", "[fd00::5]:8080"); got != "::1" {
		t.Errorf("explicit listen = %q", got)
	}
}
