package monitor

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/a1ekseev/scd/internal/scd/balancer"
	"github.com/a1ekseev/scd/internal/scd/config"
	"github.com/a1ekseev/scd/internal/scd/socks"
	"github.com/a1ekseev/scd/internal/scd/state"
)

// realShapeSub is the first subscription of the owner's real config in Go
// form (D30): balancerMonitor.socks5 blanc-xray-en:1080, remotePing via SOCKS.
func realShapeSub(t *testing.T) config.SubscriptionConfig {
	t.Helper()
	l, _, err := config.Load(realShapeConfig)
	if err != nil {
		t.Fatal(err)
	}
	return l.Config.Subscriptions[0]
}

// directCall is one Deps.Direct call.
type directCall struct {
	url     string
	timeout time.Duration
}

// recordDirect returns a Direct that records its calls and answers code.
func recordDirect(calls *[]directCall, code int) func(context.Context, string, time.Duration) (int, error) {
	return func(_ context.Context, u string, timeout time.Duration) (int, error) {
		*calls = append(*calls, directCall{u, timeout})
		return code, nil
	}
}

// 03 §10.6 p.16, on the real config shape (D30).
func TestBalancerTickViaExternalSocks(t *testing.T) {
	sub := realShapeSub(t)
	sub.Target.BalancerMonitor.RemotePing.Enabled = false
	e := newEnv(t, sub)
	var got []socks.Request
	e.deps.Probe = func(_ context.Context, r socks.Request) (socks.Response, error) {
		got = append(got, r)
		return socks.Response{StatusCode: 200, LatencyMs: 33, BodyBytes: 128}, nil
	}

	if err := BalancerTick(t.Context(), e.deps, e.sub); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ProxyHost != "blanc-xray-en" || got[0].ProxyPort != 1080 ||
		got[0].URL != "https://probe.example.com/generate_204" || got[0].Method != http.MethodGet || got[0].Timeout != 5*time.Second {
		t.Errorf("requests = %+v", got)
	}
	b := e.balancer()
	if b.State != state.BalancerHealthy || deref(b.LastStatusCode) != 200 || deref(b.LastLatencyMs) != 33 ||
		deref(b.LastSuccessStatusCode) != 200 || deref(b.LastSuccessLatencyMs) != 33 || b.ConsecutiveFailures != 0 || b.LastError != "" {
		t.Errorf("balancer = %+v", b)
	}
	if b.LastCheckedAt == nil || b.LastSuccessAt == nil || !b.LastCheckedAt.Equal(*b.LastSuccessAt) {
		t.Errorf("timestamps = %v / %v", b.LastCheckedAt, b.LastSuccessAt)
	}
	if b.RemotePingState != state.RemotePingIdle {
		t.Errorf("remote ping = %s, want idle", b.RemotePingState)
	}
}

// 03 §10.6 p.17.
func TestBalancerDegradedKeepsLastSuccess(t *testing.T) {
	e := newEnv(t, newSub(withBalancer(204, "", false)))
	e.mem.Update(e.key, func(tg *state.Target) {
		tg.Balancer = state.BalancerMonitor{
			State: state.BalancerHealthy, RemotePingState: state.RemotePingIdle,
			LastStatusCode: new(204), LastLatencyMs: new(12), LastSuccessStatusCode: new(204), LastSuccessLatencyMs: new(12),
			ConsecutiveFailures: 2,
		}
	})
	var urls []string
	e.deps.Probe = func(_ context.Context, r socks.Request) (socks.Response, error) {
		urls = append(urls, r.URL)
		return socks.Response{StatusCode: 200, LatencyMs: 40}, nil
	}

	if err := BalancerTick(t.Context(), e.deps, e.sub); err != nil {
		t.Fatal(err)
	}
	if len(urls) != 1 || urls[0] != "https://example.test/health" {
		t.Errorf("urls = %v", urls)
	}
	b := e.balancer()
	if b.State != state.BalancerDegraded || b.LastStatusCode != nil || b.LastLatencyMs != nil ||
		deref(b.LastSuccessStatusCode) != 204 || deref(b.LastSuccessLatencyMs) != 12 ||
		b.LastError != "Expected HTTP 204, got 200." || b.ConsecutiveFailures != 3 {
		t.Errorf("balancer = %+v", b)
	}
	if b.LastCheckedAt == nil || b.LastFailureAt == nil || !b.LastCheckedAt.Equal(*b.LastFailureAt) {
		t.Errorf("timestamps = %v / %v", b.LastCheckedAt, b.LastFailureAt)
	}
	// Success and failure of the check itself are not logged.
	if s := e.logs.String(); s != "" {
		t.Errorf("logs = %s", s)
	}
}

// 03 §10.6 p.18.
func TestBalancerRetryThenUpPush(t *testing.T) {
	sub := newSub(withBalancer(200, "https://push.example.test/api/push/token", false))
	sub.Target.BalancerMonitor.Retry = config.RetryConfig{Attempts: 2}
	e := newEnv(t, sub)
	var calls atomic.Int32
	e.deps.Probe = func(context.Context, socks.Request) (socks.Response, error) {
		if calls.Add(1) == 1 {
			return socks.Response{}, errors.New("temporary balancer failure")
		}
		return socks.Response{StatusCode: 200, LatencyMs: 33}, nil
	}
	var pushes []directCall
	e.deps.Direct = recordDirect(&pushes, 200)

	if err := BalancerTick(t.Context(), e.deps, e.sub); err != nil {
		t.Fatal(err)
	}
	e.bg.Wait()
	b := e.balancer()
	if calls.Load() != 2 || b.State != state.BalancerHealthy || deref(b.LastLatencyMs) != 33 || b.RemotePingState != state.RemotePingOK {
		t.Errorf("calls=%d balancer=%+v", calls.Load(), b)
	}
	if len(pushes) != 1 || pushes[0].url != "https://push.example.test/api/push/token?status=up&msg=OK&ping=33" {
		t.Errorf("pushes = %+v", pushes)
	}
	logs := e.logs.events(t, "balancer_monitor_check_retry")
	if len(logs) != 1 || logs[0]["level"] != "DEBUG" || logs[0]["attempt"] != 1.0 || logs[0]["attempts"] != 2.0 ||
		logs[0]["error"] != "temporary balancer failure" || logs[0]["subscriptionId"] != "source-1" ||
		logs[0]["msg"] != "Balancer monitor check attempt failed; retrying." {
		t.Errorf("retry logs = %v", logs)
	}
	if _, has := logs[0]["tunnelId"]; has {
		t.Errorf("balancer retry log has tunnelId: %v", logs[0])
	}
}

// 03 §10.6 p.19.
func TestBalancerDownPushAfterRetries(t *testing.T) {
	sub := newSub(withBalancer(204, "https://push.example.test/api/push/token", false))
	sub.Target.BalancerMonitor.Retry = config.RetryConfig{Attempts: 2}
	e := newEnv(t, sub)
	var calls atomic.Int32
	e.deps.Probe = func(context.Context, socks.Request) (socks.Response, error) {
		calls.Add(1)
		return socks.Response{StatusCode: 200, LatencyMs: 40}, nil
	}
	var pushes []directCall
	e.deps.Direct = recordDirect(&pushes, 200)

	if err := BalancerTick(t.Context(), e.deps, e.sub); err != nil {
		t.Fatal(err)
	}
	e.bg.Wait()
	b := e.balancer()
	if calls.Load() != 2 || b.State != state.BalancerDegraded || b.LastError != "Expected HTTP 204, got 200." ||
		b.RemotePingState != state.RemotePingOK {
		t.Errorf("calls=%d balancer=%+v", calls.Load(), b)
	}
	if b.RemotePingLastReportedStatus != "down" || b.RemotePingLastReportedPingMs != nil {
		t.Errorf("reported = %q / %v", b.RemotePingLastReportedStatus, deref(b.RemotePingLastReportedPingMs))
	}
	if len(pushes) != 1 || pushes[0].url != "https://push.example.test/api/push/token?status=down&msg=Expected+HTTP+204%2C+got+200." {
		t.Errorf("pushes = %+v", pushes)
	}
	logs := e.logs.events(t, "balancer_monitor_check_retry")
	if len(logs) != 1 || logs[0]["statusCode"] != 200.0 || logs[0]["error"] != "Expected HTTP 204, got 200." {
		t.Errorf("retry logs = %v", logs)
	}
}

func TestBalancerTickNoop(t *testing.T) {
	cases := map[string]func(c *config.SubscriptionConfig){
		"disabled":   func(c *config.SubscriptionConfig) { c.Target.BalancerMonitor.Enabled = false },
		"no socks5":  func(c *config.SubscriptionConfig) { c.Target.BalancerMonitor.Socks5 = nil },
		"no request": func(c *config.SubscriptionConfig) { c.Target.BalancerMonitor.Request = nil },
		"no target":  func(c *config.SubscriptionConfig) { c.ID = "other" },
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, newSub(withBalancer(200, "", false)))
			sub := newSub(withBalancer(200, "", false))
			mut(&sub)
			if err := BalancerTick(t.Context(), e.deps, sub); err != nil {
				t.Fatal(err)
			}
			if e.balancer().State != state.BalancerIdle {
				t.Errorf("state = %s", e.balancer().State)
			}
		})
	}
}

// A cancelled tick records nothing and pushes nothing (D7).
func TestBalancerTickCancelled(t *testing.T) {
	e := newEnv(t, newSub(withBalancer(200, "https://push.example.test/api/push/token", false)))
	ctx, cancel := context.WithCancel(t.Context())
	e.deps.Probe = func(ctx context.Context, _ socks.Request) (socks.Response, error) {
		cancel()
		<-ctx.Done()
		return socks.Response{}, textError("SOCKS HTTP request timed out after 5000ms.")
	}
	if err := BalancerTick(ctx, e.deps, e.sub); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	e.bg.Wait()
	if b := e.balancer(); b.State != state.BalancerIdle || b.RemotePingState != state.RemotePingIdle {
		t.Errorf("balancer = %+v", b)
	}
}

// Review 6.6: the balancer selection tick creates the target before the
// first sync (10 §6.3); the balancer monitor must still wait until a sync
// reached the target (TS: the target exists only after
// syncSubscriptionTarget) — no probe, no remote push.
func TestBalancerTickWaitsForSyncAfterSelectionTick(t *testing.T) {
	sub := newSub(withBalancer(200, "https://push.example.test/api/push/token", false))
	sub.Target.BalancerSelection = config.BalancerSelectionConfig{Enabled: true, BalancerTag: "x-balancer", Schedule: "*/15 * * * * *"}
	e := newEnv(t, sub)
	e.mem = state.New()
	e.deps.Memory = e.mem
	if err := balancer.Tick(t.Context(), balancer.Deps{Memory: e.mem, Log: discardLog(), Dial: e.xray.Dialer()}, sub); err != nil {
		t.Fatal(err)
	}
	if !e.mem.View(e.key, func(*state.Target) {}) {
		t.Fatal("selection tick did not create the target")
	}

	var probes atomic.Int32
	var pushes []directCall
	e.deps.Probe = func(context.Context, socks.Request) (socks.Response, error) {
		probes.Add(1)
		return socks.Response{StatusCode: 200, LatencyMs: 10}, nil
	}
	e.deps.Direct = recordDirect(&pushes, 200)
	if err := BalancerTick(t.Context(), e.deps, sub); err != nil {
		t.Fatal(err)
	}
	if err := TunnelTick(t.Context(), e.deps, sub); err != nil {
		t.Fatal(err)
	}
	e.bg.Wait()
	if probes.Load() != 0 || len(pushes) != 0 || e.balancer().State != state.BalancerIdle {
		t.Fatalf("before sync: probes %d, pushes %v, balancer %+v", probes.Load(), pushes, e.balancer())
	}

	// A sync pass reached the target (even if its apply failed).
	e.mem.MarkSynced(e.key)
	if err := BalancerTick(t.Context(), e.deps, sub); err != nil {
		t.Fatal(err)
	}
	e.bg.Wait()
	if probes.Load() != 1 || len(pushes) != 1 || e.balancer().State != state.BalancerHealthy {
		t.Fatalf("after sync: probes %d, pushes %v, balancer %+v", probes.Load(), pushes, e.balancer())
	}
}
