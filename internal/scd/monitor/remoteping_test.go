package monitor

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/a1ekseev/scd/internal/scd/socks"
	"github.com/a1ekseev/scd/internal/scd/state"
)

// 03 §10.6 p.20: the push runs in the background; the tick does not wait.
func TestRemotePingAsync(t *testing.T) {
	e := newEnv(t, newSub(withBalancer(200, "https://push.example.test/api/push/token?keep=1", false)))
	var socksCalls atomic.Int32
	e.deps.Probe = func(ctx context.Context, r socks.Request) (socks.Response, error) {
		socksCalls.Add(1)
		return ok(200, 33)(ctx, r)
	}
	pushed := make(chan string, 1)
	release := make(chan struct{})
	e.deps.Direct = func(_ context.Context, u string, _ time.Duration) (int, error) {
		pushed <- u
		<-release
		return 200, nil
	}

	if err := BalancerTick(t.Context(), e.deps, e.sub); err != nil {
		t.Fatal(err)
	}
	b := e.balancer()
	if b.State != state.BalancerHealthy || b.RemotePingState != state.RemotePingPending ||
		b.RemotePingLastReportedStatus != "up" || b.RemotePingLastReportedMsg != "OK" ||
		deref(b.RemotePingLastReportedPingMs) != 33 || b.RemotePingLastRunAt == nil {
		t.Errorf("while pending = %+v", b)
	}
	if u := <-pushed; u != "https://push.example.test/api/push/token?keep=1&status=up&msg=OK&ping=33" {
		t.Errorf("push url = %s", u)
	}
	if socksCalls.Load() != 1 {
		t.Errorf("socks calls = %d", socksCalls.Load())
	}

	close(release)
	e.bg.Wait()
	b = e.balancer()
	if b.RemotePingState != state.RemotePingOK || deref(b.RemotePingLastStatusCode) != 200 ||
		b.RemotePingLastSuccessAt == nil || b.RemotePingLastError != "" {
		t.Errorf("after push = %+v", b)
	}
	logs := e.logs.events(t, "balancer_monitor_remote_ping_succeeded")
	if len(logs) != 1 || logs[0]["level"] != "INFO" || logs[0]["statusCode"] != 200.0 || logs[0]["viaSocks"] != false {
		t.Errorf("succeeded logs = %v", logs)
	}
	if _, has := logs[0]["proxyHost"]; has {
		t.Errorf("direct push log has proxyHost: %v", logs[0])
	}
}

// 03 §10.6 p.21.
func TestRemotePingFailureKeepsHealth(t *testing.T) {
	e := newEnv(t, newSub(withBalancer(204, "https://push.example.test/api/push/token", false)))
	e.deps.Probe = ok(200, 40)
	var pushes []directCall
	e.deps.Direct = recordDirect(&pushes, 500)

	if err := BalancerTick(t.Context(), e.deps, e.sub); err != nil {
		t.Fatal(err)
	}
	e.bg.Wait()
	b := e.balancer()
	if b.State != state.BalancerDegraded || b.RemotePingState != state.RemotePingFailed ||
		deref(b.RemotePingLastStatusCode) != 500 || b.RemotePingLastError != "Remote ping returned HTTP 500." ||
		b.RemotePingLastFailureAt == nil {
		t.Errorf("balancer = %+v", b)
	}
	if b.RemotePingLastReportedStatus != "down" || b.RemotePingLastReportedMsg != "Expected HTTP 204, got 200." {
		t.Errorf("reported = %q / %q", b.RemotePingLastReportedStatus, b.RemotePingLastReportedMsg)
	}
	if len(pushes) != 1 || pushes[0].url != "https://push.example.test/api/push/token?status=down&msg=Expected+HTTP+204%2C+got+200." ||
		pushes[0].timeout != 5*time.Second {
		t.Errorf("pushes = %+v", pushes)
	}
	logs := e.logs.events(t, "balancer_monitor_remote_ping_failed")
	if len(logs) != 1 || logs[0]["level"] != "WARN" || logs[0]["statusCode"] != 500.0 ||
		logs[0]["error"] != "Remote ping returned HTTP 500." || logs[0]["reportedStatus"] != "down" {
		t.Errorf("failed logs = %v", logs)
	}
}

func TestRemotePingErrorClearsStatusCode(t *testing.T) {
	e := newEnv(t, newSub(withBalancer(200, "https://push.example.test/api/push/token", false)))
	e.mem.Update(e.key, func(tg *state.Target) { tg.Balancer.RemotePingLastStatusCode = new(200) })
	e.deps.Probe = ok(200, 33)
	e.deps.Direct = func(context.Context, string, time.Duration) (int, error) {
		return 0, textError("Remote ping timed out after 5000ms.")
	}
	if err := BalancerTick(t.Context(), e.deps, e.sub); err != nil {
		t.Fatal(err)
	}
	e.bg.Wait()
	b := e.balancer()
	if b.State != state.BalancerHealthy || b.RemotePingState != state.RemotePingFailed || b.RemotePingLastStatusCode != nil ||
		b.RemotePingLastError != "Remote ping timed out after 5000ms." {
		t.Errorf("balancer = %+v", b)
	}
	logs := e.logs.events(t, "balancer_monitor_remote_ping_failed")
	if len(logs) != 1 {
		t.Fatalf("failed logs = %v", logs)
	}
	if _, has := logs[0]["statusCode"]; has {
		t.Errorf("failed log without response has statusCode: %v", logs[0])
	}
}

// 03 §10.6 p.22.
func TestRemotePingOverlapSkipped(t *testing.T) {
	e := newEnv(t, newSub(withBalancer(200, "https://push.example.test/api/push/token", false)))
	e.mem.Update(e.key, func(tg *state.Target) { tg.Balancer.RemotePingState = state.RemotePingPending })
	e.deps.Probe = ok(200, 33)
	var pushes []directCall
	e.deps.Direct = recordDirect(&pushes, 200)

	if err := BalancerTick(t.Context(), e.deps, e.sub); err != nil {
		t.Fatal(err)
	}
	e.bg.Wait()
	if len(pushes) != 0 {
		t.Errorf("pushes = %+v", pushes)
	}
	logs := e.logs.events(t, "balancer_monitor_remote_ping_skipped_overrun")
	if len(logs) != 1 || logs[0]["level"] != "WARN" || logs[0]["subscriptionId"] != "source-1" ||
		logs[0]["msg"] != "Balancer monitor remote ping skipped because previous push is still in progress." {
		t.Errorf("skipped logs = %v", logs)
	}
	if b := e.balancer(); b.RemotePingState != state.RemotePingPending || b.State != state.BalancerHealthy {
		t.Errorf("balancer = %+v", b)
	}
}

// Concurrent ticks start at most one push (check-and-set under the state
// lock).
func TestRemotePingSingleFlight(t *testing.T) {
	e := newEnv(t, newSub(withBalancer(200, "https://push.example.test/api/push/token", false)))
	e.deps.Probe = ok(200, 33)
	release := make(chan struct{})
	var pushes atomic.Int32
	e.deps.Direct = func(context.Context, string, time.Duration) (int, error) {
		pushes.Add(1)
		<-release
		return 200, nil
	}
	done := make(chan error, 8)
	for range 8 {
		go func() { done <- BalancerTick(t.Context(), e.deps, e.sub) }()
	}
	for range 8 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	close(release)
	e.bg.Wait()
	if pushes.Load() != 1 {
		t.Errorf("pushes = %d, want 1", pushes.Load())
	}
	if n := len(e.logs.events(t, "balancer_monitor_remote_ping_skipped_overrun")); n != 7 {
		t.Errorf("skipped = %d, want 7", n)
	}
}

// 03 §10.6 p.23 and D30: the real config shape pushes through the balancer
// SOCKS5 (viaSocks: true) or directly (viaSocks: false).
func TestRemotePingViaSocks(t *testing.T) {
	t.Run("viaSocks=true", func(t *testing.T) {
		e := newEnv(t, realShapeSub(t))
		var reqs []socks.Request
		e.deps.Probe = func(_ context.Context, r socks.Request) (socks.Response, error) {
			reqs = append(reqs, r)
			if len(reqs) == 1 {
				return socks.Response{StatusCode: 200, LatencyMs: 33, BodyBytes: 128}, nil
			}
			return socks.Response{StatusCode: 502, LatencyMs: 12}, nil
		}
		if err := BalancerTick(t.Context(), e.deps, e.sub); err != nil {
			t.Fatal(err)
		}
		e.bg.Wait()
		if len(reqs) != 2 {
			t.Fatalf("socks requests = %+v", reqs)
		}
		r := reqs[1]
		if r.ProxyHost != "blanc-xray-en" || r.ProxyPort != 1080 || r.Method != http.MethodGet || r.Timeout != 5*time.Second ||
			r.URL != "https://ping.example.com/api/push?status=up&msg=OK&ping=33" {
			t.Errorf("push request = %+v", r)
		}
		b := e.balancer()
		if b.State != state.BalancerHealthy || b.RemotePingState != state.RemotePingFailed || deref(b.RemotePingLastStatusCode) != 502 {
			t.Errorf("balancer = %+v", b)
		}
		started := e.logs.events(t, "balancer_monitor_remote_ping_started")
		failed := e.logs.events(t, "balancer_monitor_remote_ping_failed")
		if len(started) != 1 || len(failed) != 1 {
			t.Fatalf("started=%v failed=%v", started, failed)
		}
		for _, l := range []map[string]any{started[0], failed[0]} {
			if l["reportedStatus"] != "up" || l["viaSocks"] != true || l["remoteHost"] != "ping.example.com" ||
				l["proxyHost"] != "blanc-xray-en" || l["proxyPort"] != 1080.0 ||
				l["subscriptionId"] != "blanc-en" || l["targetAddress"] != "blanc-xray-en:8080" {
				t.Errorf("log = %v", l)
			}
		}
		if started[0]["status"] != "up" || started[0]["level"] != "INFO" || started[0]["msg"] != "Balancer monitor remote ping started." {
			t.Errorf("started = %v", started[0])
		}
		if failed[0]["statusCode"] != 502.0 || failed[0]["msg"] != "Balancer monitor remote ping failed." {
			t.Errorf("failed = %v", failed[0])
		}
	})
	t.Run("viaSocks=false", func(t *testing.T) {
		sub := realShapeSub(t)
		sub.Target.BalancerMonitor.RemotePing.ViaSocks = false
		e := newEnv(t, sub)
		var probes atomic.Int32
		e.deps.Probe = func(ctx context.Context, r socks.Request) (socks.Response, error) {
			probes.Add(1)
			return ok(200, 33)(ctx, r)
		}
		var pushes []directCall
		e.deps.Direct = recordDirect(&pushes, 200)
		if err := BalancerTick(t.Context(), e.deps, e.sub); err != nil {
			t.Fatal(err)
		}
		e.bg.Wait()
		if probes.Load() != 1 {
			t.Errorf("socks requests = %d, want 1", probes.Load())
		}
		if len(pushes) != 1 || pushes[0].url != "https://ping.example.com/api/push?status=up&msg=OK&ping=33" || pushes[0].timeout != 5*time.Second {
			t.Errorf("pushes = %+v", pushes)
		}
		if b := e.balancer(); b.RemotePingState != state.RemotePingOK {
			t.Errorf("balancer = %+v", b)
		}
		for _, ev := range []string{"balancer_monitor_remote_ping_started", "balancer_monitor_remote_ping_succeeded"} {
			logs := e.logs.events(t, ev)
			if len(logs) != 1 || logs[0]["viaSocks"] != false || logs[0]["remoteHost"] != "ping.example.com" {
				t.Errorf("%s = %v", ev, logs)
				continue
			}
			if _, has := logs[0]["proxyHost"]; has {
				t.Errorf("%s has proxyHost: %v", ev, logs[0])
			}
		}
	})
}

// The push URL usually carries a token: logs and state carry only its host
// (TS getRemoteHost; review-2 F15).
func TestRemotePingLogsOnlyHost(t *testing.T) {
	const pushURL = "https://push.example.test/api/push/SECRET-PATH?token=SECRET-QUERY"
	t.Run("direct error with URL", func(t *testing.T) {
		e := newEnv(t, newSub(withBalancer(200, pushURL, false)))
		e.deps.Probe = ok(200, 33)
		e.deps.Direct = func(_ context.Context, u string, _ time.Duration) (int, error) {
			return 0, &url.Error{Op: "Get", URL: u, Err: errors.New("dial tcp: connection refused")}
		}
		if err := BalancerTick(t.Context(), e.deps, e.sub); err != nil {
			t.Fatal(err)
		}
		e.bg.Wait()
		checkNoSecret(t, e)
		if b := e.balancer(); b.RemotePingLastError != "dial tcp: connection refused" {
			t.Errorf("lastError = %q", b.RemotePingLastError)
		}
		for _, ev := range []string{"balancer_monitor_remote_ping_started", "balancer_monitor_remote_ping_failed"} {
			if logs := e.logs.events(t, ev); len(logs) != 1 || logs[0]["remoteHost"] != "push.example.test" {
				t.Errorf("%s = %v", ev, logs)
			}
		}
	})
	t.Run("via socks error with URL", func(t *testing.T) {
		e := newEnv(t, newSub(withBalancer(200, pushURL, true)))
		var n atomic.Int32
		e.deps.Probe = func(_ context.Context, r socks.Request) (socks.Response, error) {
			if n.Add(1) == 1 {
				return socks.Response{StatusCode: 200, LatencyMs: 33}, nil
			}
			return socks.Response{}, errors.New("invalid URL: " + r.URL)
		}
		if err := BalancerTick(t.Context(), e.deps, e.sub); err != nil {
			t.Fatal(err)
		}
		e.bg.Wait()
		checkNoSecret(t, e)
	})
	t.Run("success", func(t *testing.T) {
		e := newEnv(t, newSub(withBalancer(200, pushURL, false)))
		e.deps.Probe = ok(200, 33)
		var pushes []directCall
		e.deps.Direct = recordDirect(&pushes, 204)
		if err := BalancerTick(t.Context(), e.deps, e.sub); err != nil {
			t.Fatal(err)
		}
		e.bg.Wait()
		checkNoSecret(t, e)
		if len(pushes) != 1 || !strings.Contains(pushes[0].url, "token=SECRET-QUERY") {
			t.Errorf("pushes = %+v", pushes)
		}
	})
	t.Run("invalid URL", func(t *testing.T) {
		e := newEnv(t, newSub(withBalancer(200, "SECRET-PATH://%zz", false)))
		e.deps.Probe = ok(200, 33)
		if err := BalancerTick(t.Context(), e.deps, e.sub); err != nil {
			t.Fatal(err)
		}
		e.bg.Wait()
		checkNoSecret(t, e)
		if logs := e.logs.events(t, "balancer_monitor_remote_ping_started"); len(logs) != 1 || logs[0]["remoteHost"] != "<invalid>" {
			t.Errorf("started = %v", logs)
		}
		if b := e.balancer(); b.RemotePingState != state.RemotePingFailed || b.RemotePingLastError == "" {
			t.Errorf("balancer = %+v", b)
		}
	})
}

func checkNoSecret(t *testing.T, e *env) {
	t.Helper()
	if s := e.logs.String(); strings.Contains(s, "SECRET") {
		t.Errorf("logs leak the push URL: %s", s)
	}
	b := e.balancer()
	if strings.Contains(b.RemotePingLastError, "SECRET") || strings.Contains(b.RemotePingLastReportedMsg, "SECRET") {
		t.Errorf("state leaks the push URL: %+v", b)
	}
}

// 03 §10.6 p.24: GET on the exact URL, the status is taken from the headers
// and the body is never downloaded.
func TestDirectPushDoesNotReadBody(t *testing.T) {
	type seen struct{ method, uri string }
	got := make(chan seen, 1)
	closed := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- seen{r.Method, r.RequestURI}
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write([]byte(strings.Repeat("x", 1024)))
		w.(http.Flusher).Flush()
		// The rest of the body never comes; the client must not wait for it.
		<-r.Context().Done()
		close(closed)
	}))
	defer srv.Close()

	start := time.Now()
	code, err := PushDirect(t.Context(), srv.URL+"/api/push/token?status=up&msg=OK", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if code != http.StatusPartialContent {
		t.Errorf("status = %d", code)
	}
	if s := <-got; s.method != http.MethodGet || s.uri != "/api/push/token?status=up&msg=OK" {
		t.Errorf("request = %+v", s)
	}
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("the client kept the response body open")
	}
	if time.Since(start) > 4*time.Second {
		t.Errorf("push took %v", time.Since(start))
	}
}

func TestDirectPushTimeout(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)

	_, err := PushDirect(t.Context(), srv.URL+"/api/push/SECRET?token=SECRET", 50*time.Millisecond)
	if err == nil || err.Error() != "Remote ping timed out after 50ms." {
		t.Fatalf("err = %v", err)
	}
}

func TestDirectPushErrorHasNoURL(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	u := srv.URL
	srv.Close() // connection refused
	_, err := PushDirect(t.Context(), u+"/api/push/SECRET?token=SECRET", time.Second)
	if err == nil || strings.Contains(err.Error(), "SECRET") || strings.Contains(err.Error(), u) {
		t.Fatalf("err = %v", err)
	}
}
