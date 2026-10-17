package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/a1ekseev/scd/internal/common/logging"
	"github.com/a1ekseev/scd/internal/scd/config"
	"github.com/a1ekseev/scd/internal/scd/monitor"
	"github.com/a1ekseev/scd/internal/scd/state"
	"github.com/a1ekseev/scd/internal/scd/status"
	"github.com/a1ekseev/scd/internal/scd/xrayapi"
	"github.com/a1ekseev/scd/internal/scd/xrayapi/xrayfake"
)

const (
	vienna = "vless://7d1b6590-1069-4372-92be-8d0a0ae6eaf5@example.com:443?type=tcp&security=tls#🇦🇹 Вена, Австрия"
	berlin = "vless://7d1b6590-1069-4372-92be-8d0a0ae6eaf6@example.com:443?type=tcp&security=tls#🇩🇪 Берлин, Германия"

	// never is a schedule whose next slot is far away (D7: no waiting for it).
	never = "0 0 1 1 *"
)

// lockedBuffer is a goroutine-safe JSON log sink.
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

// events returns the parsed log records with the given event.
func (l *lockedBuffer) events(t *testing.T, event string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for line := range strings.Lines(l.String()) {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("log line %q: %v", line, err)
		}
		if m["event"] == event {
			out = append(out, m)
		}
	}
	return out
}

// waitEvent polls (real time) until event was logged n times.
func (l *lockedBuffer) waitEvent(t *testing.T, event string, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for len(l.events(t, event)) < n {
		if time.Now().After(deadline) {
			t.Fatalf("%d× %s not logged:\n%s", n, event, l.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func subscriptionFile(t *testing.T, lines ...string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "subscription.txt")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func target(addr string) config.SubscriptionTargetConfig {
	return config.SubscriptionTargetConfig{
		Address: addr, TimeoutMs: 5000,
		FixedOutbounds: []string{}, FixedInbounds: []string{}, FixedRouting: []string{},
		ObservatorySubjectSelectorPrefix: "x-observe-",
		InboundSocks:                     &config.InboundSocksConfig{Listen: "127.0.0.1", PortRange: config.PortRange{Start: 20000, End: 20010}},
		BalancerSelection:                config.BalancerSelectionConfig{Observatory: true},
	}
}

func sub(id, input string, t config.SubscriptionTargetConfig) config.SubscriptionConfig {
	return config.SubscriptionConfig{ID: id, Input: input, Enabled: true, Format: "auto", FetchTimeoutMs: 5000, AllowInsecure: "deny", Target: t}
}

func daemonConfig(subs ...config.SubscriptionConfig) config.Loaded {
	var r config.ResourceConfig
	r.Outbounds.Enabled, r.Inbounds.Enabled, r.Routing.Enabled = true, true, true
	return config.Loaded{Path: "/tmp/daemon.yml", Config: config.AppConfig{
		Subscriptions: subs,
		Runtime:       config.RuntimeConfig{Mode: "daemon", Schedule: never},
		Logging:       config.LoggingConfig{Level: "debug", Format: "json"},
		Resources:     r,
		StatusServer:  config.StatusServerConfig{InstanceID: "scd-test", RemoteInstances: []config.RemoteStatusInstanceConfig{}},
	}}
}

func newDaemon(t *testing.T, l config.Loaded, dial xrayapi.Dialer) (*Daemon, *lockedBuffer) {
	t.Helper()
	logs := &lockedBuffer{}
	log, err := logging.New(logs, "debug", "json")
	if err != nil {
		t.Fatal(err)
	}
	return &Daemon{Config: l, Log: log, Dial: dial}, logs
}

// dialRecorder is a Dialer that records the addresses and fails.
type dialRecorder struct {
	mu    sync.Mutex
	addrs []string
	at    []time.Time
}

func (d *dialRecorder) dial(addr string, _ time.Duration) (xrayapi.Client, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.addrs = append(d.addrs, addr)
	d.at = append(d.at, time.Now())
	return nil, errors.New("dial refused by test")
}

func (d *dialRecorder) snapshot() ([]string, []time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.addrs), slices.Clone(d.at)
}

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

func httpGet(t *testing.T, url string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(body)
}

func startRun(ctx context.Context, d *Daemon) <-chan error {
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	return done
}

func waitDone(t *testing.T, done <-chan error, limit time.Duration) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(limit):
		t.Fatalf("Run did not return within %v", limit)
		return nil
	}
}

func TestDaemonRejectsNonDaemonMode(t *testing.T) {
	l := daemonConfig()
	l.Config.Runtime.Mode = "run-once"
	d, logs := newDaemon(t, l, (&dialRecorder{}).dial)

	err := d.Run(t.Context())

	if want := "config at /tmp/daemon.yml is not in daemon mode"; err == nil || err.Error() != want {
		t.Fatalf("err = %v, want %q", err, want)
	}
	if s := logs.String(); s != "" {
		t.Errorf("logged before the mode check:\n%s", s)
	}
}

// TestDaemonFirstSyncImmediate: config_loaded, then the first daemon tick
// and its sync run at start-up without waiting for a cron slot (03 §3.2).
func TestDaemonFirstSyncImmediate(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		in := subscriptionFile(t, vienna)
		rec := &dialRecorder{}
		d, logs := newDaemon(t, daemonConfig(sub("source-1", in, target("127.0.0.1:8080"))), rec.dial)
		ctx, cancel := context.WithCancel(t.Context())
		t0 := time.Now()
		done := startRun(ctx, d)
		synctest.Wait()

		if n := len(logs.events(t, "config_loaded")); n != 1 {
			t.Errorf("config_loaded logged %d times", n)
		}
		if n := len(logs.events(t, "daemon_tick")); n != 1 {
			t.Errorf("daemon_tick logged %d times, want 1", n)
		}
		if n := len(logs.events(t, "sync_started")); n != 1 {
			t.Errorf("sync_started logged %d times, want 1", n)
		}
		addrs, at := rec.snapshot()
		if len(addrs) == 0 || addrs[0] != "127.0.0.1:8080" || !at[0].Equal(t0) {
			t.Errorf("dials = %v at %v, want 127.0.0.1:8080 at start", addrs, at)
		}
		cancel()
		if err := <-done; err != nil {
			t.Fatalf("Run: %v", err)
		}
	})
}

// TestDaemonStartsWhenXrayDown (Review Focus 2): with Xray unreachable the
// sync fails per kind, the status server still answers and the next tick
// tries the apply again.
func TestDaemonStartsWhenXrayDown(t *testing.T) {
	closed := freeAddr(t)
	var (
		mu    sync.Mutex
		dials int
	)
	dial := func(_ string, timeout time.Duration) (xrayapi.Client, error) {
		mu.Lock()
		dials++
		mu.Unlock()
		return xrayapi.Dial(closed, timeout)
	}
	l := daemonConfig(sub("source-1", subscriptionFile(t, vienna), target("127.0.0.1:8080")))
	l.Config.Runtime.Schedule = "* * * * * *"
	listen := freeAddr(t)
	l.Config.StatusServer.Enabled, l.Config.StatusServer.Listen = true, listen
	d, logs := newDaemon(t, l, dial)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := startRun(ctx, d)

	logs.waitEvent(t, "status_server_started", 1)
	logs.waitEvent(t, "apply_failed", 3)
	code, body := httpGet(t, "http://"+listen+"/api/instance-status")
	if code != http.StatusOK || !strings.Contains(body, `"version": 1`) {
		t.Errorf("/api/instance-status = %d %s", code, body)
	}
	logs.waitEvent(t, "daemon_tick", 2)
	logs.waitEvent(t, "apply_failed", 6)
	mu.Lock()
	n := dials
	mu.Unlock()
	if n < 2 {
		t.Errorf("dials = %d, want a retry on the next tick", n)
	}
	cancel()
	if err := waitDone(t, done, 5*time.Second); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

// TestDaemonShutdownWaitsForInFlightApply (Review Focus 1, D7): a cancel in
// the middle of an apply lets the apply finish (all RPCs reach Xray) and
// the status server keep answering until then; Run returns after it.
func TestDaemonShutdownWaitsForInFlightApply(t *testing.T) {
	const delay = 200 * time.Millisecond
	srv, _ := xrayfake.Start(t)
	srv.Delay("AddOutbound", delay)
	l := daemonConfig(sub("source-1", subscriptionFile(t, vienna, berlin), target("127.0.0.1:8080")))
	listen := freeAddr(t)
	l.Config.StatusServer.Enabled, l.Config.StatusServer.Listen = true, listen
	d, logs := newDaemon(t, l, srv.Dialer())
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := startRun(ctx, d)

	deadline := time.Now().Add(5 * time.Second)
	for !slices.ContainsFunc(srv.Calls(), func(c string) bool { return strings.HasPrefix(c, "AddOutbound") }) {
		if time.Now().After(deadline) {
			t.Fatal("AddOutbound was never called")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancelled := time.Now()
	cancel()
	if code, _ := httpGet(t, "http://"+listen+"/api/instance-status"); code != http.StatusOK {
		t.Errorf("status server during drain: %d", code)
	}
	if err := waitDone(t, done, 5*time.Second); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if took := time.Since(cancelled); took < delay {
		t.Errorf("Run returned %v after cancel, before the in-flight apply (≥ %v)", took, delay)
	}

	if n := len(logs.events(t, "apply_finished")); n != 3 {
		t.Errorf("apply_finished logged %d times, want 3:\n%s", n, logs.String())
	}
	if n := len(logs.events(t, "apply_failed")); n != 0 {
		t.Errorf("apply_failed logged %d times:\n%s", n, logs.String())
	}
	if got := srv.OutboundTags(); len(got) != 2 {
		t.Errorf("outbounds = %v, want 2", got)
	}
	if got := srv.InboundTags(); len(got) != 2 {
		t.Errorf("inbounds = %v, want 2", got)
	}
	if got := srv.Rules(); len(got) != 2 {
		t.Errorf("rules = %v, want 2", got)
	}
	if n := len(logs.events(t, "daemon_stopping")); n != 1 {
		t.Errorf("daemon_stopping logged %d times", n)
	}
	if _, err := (&net.Dialer{Timeout: time.Second}).DialContext(t.Context(), "tcp", listen); err == nil {
		t.Error("status server still listening after Run returned")
	}
}

// failingTransport fails every request without touching the network.
type failingTransport struct{}

func (failingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("no network in tests")
}

// TestDaemonStopsImmediatelyBetweenTicks (D7): a cancel between ticks ends
// every loop (sync, selection, remote) at once, without waiting for the next
// slot of a yearly schedule.
func TestDaemonStopsImmediatelyBetweenTicks(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tg := target("127.0.0.1:8080")
		tg.BalancerSelection = config.BalancerSelectionConfig{Enabled: true, BalancerTag: "x-balancer", Schedule: never, Observatory: true}
		tg.Monitor = config.TargetMonitorConfig{Enabled: true, Schedule: never, MaxParallel: 1}
		l := daemonConfig(sub("source-1", subscriptionFile(t, vienna), tg))
		l.Config.StatusServer.RemoteInstances = []config.RemoteStatusInstanceConfig{
			{ID: "remote-1", APIURL: "http://remote.invalid/api/instance-status", Schedule: never, TimeoutMs: 1000},
		}
		d, logs := newDaemon(t, l, (&dialRecorder{}).dial)
		d.HTTPClient = &http.Client{Transport: failingTransport{}}
		ctx, cancel := context.WithCancel(t.Context())
		t0 := time.Now()
		done := startRun(ctx, d)
		synctest.Wait()
		if n := len(logs.events(t, "remote_status_collection_failed")); n != 1 {
			t.Errorf("remote collection ran %d times, want 1", n)
		}

		cancel()
		if err := <-done; err != nil {
			t.Fatalf("Run: %v", err)
		}
		if waited := time.Since(t0); waited != 0 {
			t.Errorf("Run waited %v for a cron slot", waited)
		}
		if s := logs.String(); strings.Contains(s, "tick_failed") || strings.Contains(s, "Daemon sync failed.") {
			t.Errorf("shutdown logged a tick failure:\n%s", s)
		}
	})
}

// TestDaemonStartsLoopsForDisabledSubscriptionMonitors: like TS, the
// monitor, balancer monitor and balancer selection loops start for every
// configured subscription, including disabled ones (03 §3.1).
func TestDaemonStartsLoopsForDisabledSubscriptionMonitors(t *testing.T) {
	tg := target("10.0.0.2:8080")
	tg.Monitor = config.TargetMonitorConfig{Enabled: true, Schedule: never, MaxParallel: 1}
	tg.BalancerMonitor = config.TargetBalancerMonitorConfig{Enabled: true, Schedule: never}
	tg.BalancerSelection = config.BalancerSelectionConfig{Enabled: true, BalancerTag: "x-balancer", Schedule: never, Observatory: true}
	disabled := sub("disabled-1", "/nonexistent", tg)
	disabled.Enabled = false
	off := sub("off-1", "/nonexistent", target("10.0.0.3:8080"))
	off.Enabled = false
	off.Target.Monitor = config.TargetMonitorConfig{Enabled: true} // no schedule: no loop
	l := daemonConfig(disabled, off)
	l.Config.StatusServer.RemoteInstances = []config.RemoteStatusInstanceConfig{
		{ID: "remote-1", APIURL: "http://remote.invalid/api/instance-status", Schedule: never, TimeoutMs: 1000},
	}
	d, _ := newDaemon(t, l, (&dialRecorder{}).dial)

	var got []string
	for _, lp := range d.loops(state.New(), status.NewAggregator(nil), &sync.WaitGroup{}) {
		got = append(got, fmt.Sprintf("%s %v", lp.events.FailureEvent, lp.attrs))
	}
	want := []string{
		"sync_failed []",
		"monitor_tick_failed [subscriptionId disabled-1 targetAddress 10.0.0.2:8080]",
		"balancer_monitor_tick_failed [subscriptionId disabled-1 targetAddress 10.0.0.2:8080]",
		"balancer_selection_tick_failed [subscriptionId disabled-1 targetAddress 10.0.0.2:8080]",
		"remote_status_tick_failed [remoteInstanceId remote-1 apiUrl http://remote.invalid/api/instance-status]",
	}
	if !slices.Equal(got, want) {
		t.Errorf("loops =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	// The selection loop of the disabled subscription really runs.
	synctest.Test(t, func(t *testing.T) {
		l := daemonConfig(disabled)
		rec := &dialRecorder{}
		d, _ := newDaemon(t, l, rec.dial)
		ctx, cancel := context.WithCancel(t.Context())
		done := startRun(ctx, d)
		synctest.Wait()
		cancel()
		if err := <-done; err != nil {
			t.Fatalf("Run: %v", err)
		}
		if addrs, _ := rec.snapshot(); !slices.Equal(addrs, []string{"10.0.0.2:8080"}) {
			t.Errorf("dials = %v, want the selection tick of the disabled subscription", addrs)
		}
	})
}

func TestDaemonBindErrorFails(t *testing.T) {
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	l := daemonConfig(sub("source-1", subscriptionFile(t, vienna), target("127.0.0.1:8080")))
	l.Config.StatusServer.Enabled, l.Config.StatusServer.Listen = true, ln.Addr().String()
	rec := &dialRecorder{}
	d, logs := newDaemon(t, l, rec.dial)

	err = d.Run(t.Context())

	if err == nil || !strings.Contains(err.Error(), "listen on "+ln.Addr().String()) {
		t.Fatalf("err = %v, want a listen error", err)
	}
	if n := len(logs.events(t, "daemon_tick")); n != 0 {
		t.Errorf("loops started despite the bind error:\n%s", logs.String())
	}
	if addrs, _ := rec.snapshot(); len(addrs) != 0 {
		t.Errorf("dials = %v, want none", addrs)
	}
}

// TestDaemonCollectsRemoteInstances: the remote loop collects at start-up and
// /api/instances shows the remote snapshot (03 §7).
func TestDaemonCollectsRemoteInstances(t *testing.T) {
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"version":1,"instanceId":"remote-a","capturedAt":"2026-10-17T12:00:00.000Z","subscriptions":[],"tunnels":[]}`)
	}))
	defer remote.Close()
	l := daemonConfig()
	listen := freeAddr(t)
	l.Config.StatusServer.Enabled, l.Config.StatusServer.Listen = true, listen
	l.Config.StatusServer.RemoteInstances = []config.RemoteStatusInstanceConfig{
		{ID: "remote-a", APIURL: remote.URL + "/api/instance-status", Schedule: never, TimeoutMs: 2000},
	}
	d, logs := newDaemon(t, l, (&dialRecorder{}).dial)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := startRun(ctx, d)

	logs.waitEvent(t, "remote_status_collected", 1)
	code, body := httpGet(t, "http://"+listen+"/api/instances")
	if code != http.StatusOK || !strings.Contains(body, `"instanceId": "remote-a"`) {
		t.Errorf("/api/instances = %d %s", code, body)
	}
	cancel()
	if err := waitDone(t, done, 5*time.Second); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestOnlyCancelled(t *testing.T) {
	boom := errors.New("Unable to connect to Xray API")
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"canceled", context.Canceled, true},
		{"wrapped canceled", fmt.Errorf("probe: %w", context.Canceled), true},
		{"task error of cancellations", &monitor.TaskError{Errs: []error{context.Canceled, context.Canceled}}, true},
		{"task error with a real failure", &monitor.TaskError{Errs: []error{context.Canceled, boom}}, false},
		{"empty task error", &monitor.TaskError{}, false},
		{"other error", boom, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := onlyCancelled(tc.err); got != tc.want {
				t.Errorf("onlyCancelled(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestQuietOnCancelKeepsErrorsWhileRunning(t *testing.T) {
	task := quietOnCancel(func(context.Context) error { return context.Canceled })
	if err := task(t.Context()); !errors.Is(err, context.Canceled) {
		t.Errorf("live ctx: err = %v, want it reported", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := task(ctx); err != nil {
		t.Errorf("cancelled ctx: err = %v, want nil", err)
	}
}
