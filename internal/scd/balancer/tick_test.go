package balancer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/xtls/xray-core/app/observatory"

	"github.com/a1ekseev/scd/internal/scd/config"
	"github.com/a1ekseev/scd/internal/scd/state"
	"github.com/a1ekseev/scd/internal/scd/topology"
	"github.com/a1ekseev/scd/internal/scd/xrayapi"
	"github.com/a1ekseev/scd/internal/scd/xrayapi/xrayfake"
)

const prefix = "x-observe-"

// logSink is a goroutine-safe JSON log buffer.
type logSink struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logSink) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

// events returns the parsed log lines, optionally only those with event.
func (l *logSink) events(t *testing.T, event string) []map[string]any {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []map[string]any
	for line := range strings.Lines(l.b.String()) {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("log line %q: %v", line, err)
		}
		if event == "" || m["event"] == event {
			out = append(out, m)
		}
	}
	return out
}

func (l *logSink) logger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(l, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// stubClient serves GetBalancerInfo and GetOutboundStatus from its fields;
// every other RPC panics (nil embedded interface).
type stubClient struct {
	xrayapi.Client
	mu      sync.Mutex
	info    xrayapi.BalancerInfo
	infoErr error
	obs     []xrayapi.OutboundObservation
	obsErr  error
	tags    []string
	delay   time.Duration
}

func (s *stubClient) GetBalancerInfo(_ context.Context, tag string) (xrayapi.BalancerInfo, error) {
	s.mu.Lock()
	s.tags = append(s.tags, tag)
	d := s.delay
	s.mu.Unlock()
	time.Sleep(d)
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.info, s.infoErr
}

func (s *stubClient) GetOutboundStatus(context.Context) ([]xrayapi.OutboundObservation, error) {
	s.mu.Lock()
	d := s.delay
	s.mu.Unlock()
	time.Sleep(d)
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.obs), s.obsErr
}

func (s *stubClient) Close() error { return nil }

func (s *stubClient) set(fn func(s *stubClient)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(s)
}

func selSub(observatoryOn bool) config.SubscriptionConfig {
	return config.SubscriptionConfig{
		ID: "sub-1", Enabled: true,
		Target: config.SubscriptionTargetConfig{
			Address: "127.0.0.1:8080", TimeoutMs: 5000, ObservatorySubjectSelectorPrefix: prefix,
			BalancerSelection: config.BalancerSelectionConfig{
				Enabled: true, BalancerTag: "x-balancer", Schedule: "*/15 * * * * *", Observatory: observatoryOn,
			},
		},
	}
}

type env struct {
	mem  *state.Memory
	logs *logSink
	c    *stubClient
	d    Deps
	sub  config.SubscriptionConfig
	key  string
}

func newEnv(observatoryOn bool) *env {
	e := &env{mem: state.New(), logs: &logSink{}, c: &stubClient{}, sub: selSub(observatoryOn)}
	e.key = state.Key(e.sub.ID, e.sub.Target.Address)
	e.d = Deps{
		Memory: e.mem, Log: e.logs.logger(),
		Dial: func(string, time.Duration) (xrayapi.Client, error) { return e.c, nil },
	}
	return e
}

func (e *env) tick(t *testing.T) {
	t.Helper()
	if err := Tick(t.Context(), e.d, e.sub); err != nil {
		t.Fatal(err)
	}
}

func (e *env) sel(t *testing.T) state.SelectionState {
	t.Helper()
	var s state.SelectionState
	if !e.mem.View(e.key, func(tg *state.Target) { s = tg.BalancerSelection }) {
		t.Fatal("no target state")
	}
	return s
}

func single(tag string) xrayapi.BalancerInfo {
	return xrayapi.BalancerInfo{PrincipleTargets: []string{tag}, PrincipleKnown: true}
}

// 10 §6.9 p.4: idle → ok; the tick creates the state before the first sync.
func TestTickIdleToOK(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(true)
		e.c.info = single(prefix + "de-1")
		e.c.obs = []xrayapi.OutboundObservation{{Tag: prefix + "de-1", Alive: true, DelayMs: new(int64(42))}}
		start := time.Now()
		e.tick(t)

		s := e.sel(t)
		if s.State != state.SelectionOK || s.Mode != state.ModeSingle || s.SelectedTag != prefix+"de-1" ||
			s.BalancerTag != "x-balancer" || !slices.Equal(s.PrincipleTargets, []string{prefix + "de-1"}) ||
			s.SelectedSince == nil || !s.SelectedSince.Equal(start) || s.LastCheckedAt == nil || s.LastSuccessAt == nil ||
			s.LastFailureAt != nil || s.ConsecutiveFailures != 0 || s.ObservatoryCheckedAt == nil ||
			len(s.Observatory) != 1 || *s.Observatory[prefix+"de-1"].DelayMs != 42 {
			t.Errorf("state = %+v", s)
		}
		if !slices.Equal(e.c.tags, []string{"x-balancer"}) {
			t.Errorf("balancer tags = %v", e.c.tags)
		}
		e.mem.View(e.key, func(tg *state.Target) {
			if tg.Topology != nil || len(tg.Tunnels) != 0 || tg.Balancer.State != state.BalancerIdle {
				t.Errorf("tick changed more than the selection: %+v", tg)
			}
		})
		ch := e.logs.events(t, "balancer_selection_changed")
		if len(ch) != 1 || ch[0]["from"] != "" || ch[0]["to"] != prefix+"de-1" || ch[0]["mode"] != "single" ||
			ch[0]["balancerTag"] != "x-balancer" || ch[0]["subscriptionId"] != "sub-1" || ch[0]["targetAddress"] != "127.0.0.1:8080" ||
			ch[0]["level"] != "INFO" {
			t.Errorf("changed logs = %v", ch)
		}
		if all := e.logs.events(t, ""); len(all) != 1 {
			t.Errorf("logs = %v", all)
		}

		// The same selection again: nothing logged, selectedSince kept.
		time.Sleep(15 * time.Second)
		e.tick(t)
		if s2 := e.sel(t); !s2.SelectedSince.Equal(start) || !s2.LastCheckedAt.Equal(start.Add(15*time.Second)) {
			t.Errorf("second state = %+v", s2)
		}
		if all := e.logs.events(t, ""); len(all) != 1 {
			t.Errorf("logs after unchanged tick = %v", all)
		}
	})
}

// 10 §6.9 p.4: ok → error keeps the selection and logs once; the same error
// again is not logged; error → ok logs recovered.
func TestTickErrorTransitions(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(false)
		e.c.info = single(prefix + "de-1")
		start := time.Now()
		e.tick(t)

		fail := &xrayapi.BalancerNotFoundError{Tag: "x-balancer"}
		e.c.set(func(s *stubClient) { s.infoErr = fail })
		time.Sleep(15 * time.Second)
		failedAt := time.Now()
		e.tick(t)
		s := e.sel(t)
		if s.State != state.SelectionError || s.SelectedTag != prefix+"de-1" || s.Mode != state.ModeSingle ||
			!slices.Equal(s.PrincipleTargets, []string{prefix + "de-1"}) || s.LastError != `Xray balancer "x-balancer" not found.` ||
			!s.LastFailureAt.Equal(failedAt) || !s.LastSuccessAt.Equal(start) || !s.LastCheckedAt.Equal(failedAt) ||
			s.ConsecutiveFailures != 1 || !s.SelectedSince.Equal(start) {
			t.Errorf("error state = %+v", s)
		}
		failed := e.logs.events(t, "balancer_selection_failed")
		if len(failed) != 1 || failed[0]["error"] != `Xray balancer "x-balancer" not found.` || failed[0]["level"] != "WARN" ||
			failed[0]["balancerTag"] != "x-balancer" {
			t.Errorf("failed logs = %v", failed)
		}

		// Same error: no log, failures counted.
		time.Sleep(15 * time.Second)
		e.tick(t)
		if s := e.sel(t); s.ConsecutiveFailures != 2 || s.State != state.SelectionError {
			t.Errorf("repeat state = %+v", s)
		}
		if n := len(e.logs.events(t, "balancer_selection_failed")); n != 1 {
			t.Errorf("failed logs after repeat = %d", n)
		}

		// A different error text is logged again.
		e.c.set(func(s *stubClient) {
			s.infoErr = &xrayapi.Error{Message: "Request to Xray API at 127.0.0.1:8080 timed out after 5000ms."}
		})
		e.tick(t)
		if n := len(e.logs.events(t, "balancer_selection_failed")); n != 2 {
			t.Errorf("failed logs after new text = %d", n)
		}

		// Recovery.
		e.c.set(func(s *stubClient) { s.infoErr = nil })
		e.tick(t)
		s = e.sel(t)
		if s.State != state.SelectionOK || s.LastError != "" || s.ConsecutiveFailures != 0 || s.LastFailureAt == nil {
			t.Errorf("recovered state = %+v", s)
		}
		rec := e.logs.events(t, "balancer_selection_recovered")
		if len(rec) != 1 || rec[0]["level"] != "INFO" || rec[0]["balancerTag"] != "x-balancer" {
			t.Errorf("recovered logs = %v", rec)
		}
		if n := len(e.logs.events(t, "balancer_selection_changed")); n != 1 {
			t.Errorf("changed logs = %d (recovery to the same selection is no change)", n)
		}
	})
}

// 10 §6.9 p.4: a new selection logs changed and moves selectedSince.
func TestTickSelectionChanged(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(false)
		e.c.info = single(prefix + "de-1")
		e.tick(t)
		time.Sleep(time.Minute)
		changedAt := time.Now()
		e.c.set(func(s *stubClient) { s.info = single(prefix + "nl-1") })
		e.tick(t)
		s := e.sel(t)
		if s.SelectedTag != prefix+"nl-1" || s.PreviousSelectedTag != prefix+"de-1" || !s.SelectedSince.Equal(changedAt) {
			t.Errorf("state = %+v", s)
		}

		// Override of the same tag: the mode changes (logged), the tag and
		// selectedSince do not.
		time.Sleep(time.Minute)
		e.c.set(func(s *stubClient) {
			s.info = xrayapi.BalancerInfo{OverrideTarget: prefix + "nl-1", PrincipleTargets: []string{prefix + "de-1"}, PrincipleKnown: true}
		})
		e.tick(t)
		s = e.sel(t)
		if s.Mode != state.ModeOverride || s.SelectedTag != prefix+"nl-1" || s.OverrideTarget != prefix+"nl-1" ||
			!s.SelectedSince.Equal(changedAt) || !slices.Equal(s.PrincipleTargets, []string{prefix + "de-1"}) {
			t.Errorf("override state = %+v", s)
		}

		// No alive candidate.
		e.c.set(func(s *stubClient) { s.info = xrayapi.BalancerInfo{PrincipleTargets: []string{}, PrincipleKnown: true} })
		e.tick(t)
		s = e.sel(t)
		if s.Mode != state.ModeNone || s.SelectedTag != "" || s.OverrideTarget != "" || s.PreviousSelectedTag != prefix+"nl-1" {
			t.Errorf("none state = %+v", s)
		}

		ch := e.logs.events(t, "balancer_selection_changed")
		want := [][3]string{
			{"", prefix + "de-1", "single"},
			{prefix + "de-1", prefix + "nl-1", "single"},
			{prefix + "nl-1", prefix + "nl-1", "override"},
			{prefix + "nl-1", "", "none"},
		}
		if len(ch) != len(want) {
			t.Fatalf("changed logs = %v", ch)
		}
		for i, w := range want {
			if ch[i]["from"] != w[0] || ch[i]["to"] != w[1] || ch[i]["mode"] != w[2] {
				t.Errorf("changed[%d] = %v, want %v", i, ch[i], w)
			}
		}
	})
}

// 10 §6.4, §6.9 p.4: observatory unavailable while routing works.
func TestTickObservatoryUnavailable(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(true)
		e.c.info = single(prefix + "de-1")
		e.c.obs = []xrayapi.OutboundObservation{{Tag: prefix + "de-1", Alive: true, DelayMs: new(int64(42))}}
		e.tick(t)
		okAt := e.sel(t).ObservatoryCheckedAt

		disabled := &xrayapi.ServiceDisabledError{Service: "ObservatoryService"}
		e.c.set(func(s *stubClient) { s.obsErr = disabled })
		time.Sleep(15 * time.Second)
		e.tick(t)
		e.tick(t)
		s := e.sel(t)
		if s.State != state.SelectionOK || s.SelectedTag != prefix+"de-1" || s.Observatory != nil ||
			s.ObservatoryError != disabled.Error() || !s.ObservatoryCheckedAt.Equal(*okAt) {
			t.Errorf("state = %+v", s)
		}
		un := e.logs.events(t, "balancer_observatory_unavailable")
		if len(un) != 1 || un[0]["error"] != disabled.Error() || un[0]["level"] != "WARN" {
			t.Errorf("unavailable logs = %v", un)
		}
		if n := len(e.logs.events(t, "balancer_selection_failed")); n != 0 {
			t.Errorf("selection failed logs = %d", n)
		}

		// Back: the error clears, the map is refilled.
		e.c.set(func(s *stubClient) { s.obsErr = nil })
		e.tick(t)
		if s := e.sel(t); s.ObservatoryError != "" || len(s.Observatory) != 1 {
			t.Errorf("recovered observatory = %+v", s)
		}
	})
}

// observatory: false never calls ObservatoryService.
func TestTickObservatoryDisabled(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(false)
		e.c.info = single(prefix + "de-1")
		e.c.obsErr = errors.New("must not be called")
		e.tick(t)
		if s := e.sel(t); s.Observatory != nil || s.ObservatoryError != "" || s.ObservatoryCheckedAt != nil {
			t.Errorf("state = %+v", s)
		}
	})
}

// GetBalancerInfo and GetOutboundStatus run in parallel (10 §6.3).
func TestTickCallsInParallel(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(true)
		e.c.info = single(prefix + "de-1")
		e.c.delay = time.Second
		start := time.Now()
		e.tick(t)
		if took := time.Since(start); took != time.Second {
			t.Errorf("tick took %v, want 1s (parallel calls)", took)
		}
	})
}

// A dial error is a selection failure; the observatory counts as not
// checked: no observatory log, its previous data are kept.
func TestTickDialError(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(true)
		e.c.info = single(prefix + "de-1")
		e.c.obs = []xrayapi.OutboundObservation{{Tag: prefix + "de-1", Alive: true, DelayMs: new(int64(42))}}
		e.tick(t)
		okAt := e.sel(t).ObservatoryCheckedAt

		e.d.Dial = func(string, time.Duration) (xrayapi.Client, error) { return nil, errors.New("dial failed") }
		time.Sleep(15 * time.Second)
		e.tick(t)
		s := e.sel(t)
		if s.State != state.SelectionError || s.LastError != "dial failed" || s.ConsecutiveFailures != 1 {
			t.Errorf("state = %+v", s)
		}
		if s.ObservatoryError != "" || len(s.Observatory) != 1 || !s.ObservatoryCheckedAt.Equal(*okAt) {
			t.Errorf("observatory changed on a dial error: %+v", s)
		}
		if n := len(e.logs.events(t, "balancer_observatory_unavailable")); n != 0 {
			t.Errorf("observatory_unavailable logs = %d", n)
		}
		if n := len(e.logs.events(t, "balancer_selection_failed")); n != 1 {
			t.Errorf("selection_failed logs = %d", n)
		}

		// Before any success: still no observatory error.
		e2 := newEnv(true)
		e2.d.Dial = e.d.Dial
		e2.tick(t)
		if s := e2.sel(t); s.ObservatoryError != "" || s.Observatory != nil || s.State != state.SelectionError {
			t.Errorf("first-tick dial error state = %+v", s)
		}
	})
}

// Disabled selection: no state, no dial.
func TestTickDisabled(t *testing.T) {
	e := newEnv(true)
	e.sub.Target.BalancerSelection.Enabled = false
	e.d.Dial = func(string, time.Duration) (xrayapi.Client, error) {
		t.Error("unexpected dial")
		return nil, errors.New("no")
	}
	e.tick(t)
	if len(e.mem.Keys()) != 0 {
		t.Error("state created")
	}
}

// A cancelled tick records nothing.
func TestTickCancelled(t *testing.T) {
	e := newEnv(true)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := Tick(ctx, e.d, e.sub); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	e.mem.View(e.key, func(tg *state.Target) {
		if tg.BalancerSelection.State != "" {
			t.Errorf("state recorded: %+v", tg.BalancerSelection)
		}
	})
}

// 10 §6.3: the tick takes no target lock and keeps working while sync
// holds it. Real time: a goroutine blocked on a sync.Mutex is not durably
// blocked for synctest. Uses the bufconn fake Xray end to end.
func TestTickNotBlockedByTargetLock(t *testing.T) {
	srv, _ := xrayfake.Start(t,
		xrayfake.WithStaticOutbounds(prefix+"de-1", prefix+"nl-1", "direct"),
		xrayfake.WithBalancer("x-balancer", []string{prefix}, "leastPing"))
	srv.SetObservation(
		&observatory.OutboundStatus{OutboundTag: prefix + "nl-1", Alive: true, Delay: 120, LastSeenTime: 1792229310, LastTryTime: 1792229310},
		&observatory.OutboundStatus{OutboundTag: prefix + "de-1", Alive: true, Delay: 21, LastSeenTime: 1792229310, LastTryTime: 1792229310},
	)
	mem := state.New()
	sub := selSub(true)
	key := state.Key(sub.ID, sub.Target.Address)
	unlock := mem.LockTarget(key)
	defer unlock()

	done := make(chan error, 1)
	go func() {
		done <- Tick(t.Context(), Deps{Memory: mem, Log: slog.New(slog.DiscardHandler), Dial: srv.Dialer()}, sub)
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("tick blocked by the target lock")
	}
	mem.View(key, func(tg *state.Target) {
		s := tg.BalancerSelection
		if s.State != state.SelectionOK || s.SelectedTag != prefix+"de-1" || len(s.Observatory) != 2 ||
			*s.Observatory[prefix+"nl-1"].DelayMs != 120 {
			t.Errorf("state = %+v", s)
		}
	})
	if calls := srv.Calls(); !slices.Contains(calls, "GetBalancerInfo:x-balancer") || !slices.Contains(calls, "GetOutboundStatus") {
		t.Errorf("calls = %v", calls)
	}
}

// The selection survives a topology replacement (10 §6.4).
func TestSelectionSurvivesReplaceTopology(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(false)
		e.c.info = single(prefix + "de-1")
		e.tick(t)
		e.mem.ReplaceTopology(e.key, topologyOf(), nil)
		if s := e.sel(t); s.SelectedTag != prefix+"de-1" || s.State != state.SelectionOK {
			t.Errorf("state after ReplaceTopology = %+v", s)
		}
	})
}

// topologyOf is a one-tunnel topology (de-1, prefixed).
func topologyOf() topology.Topology {
	return topology.Topology{Tunnels: []topology.Tunnel{tunnelOf("de-1", prefix, prefix+"de-1").Mapping}}
}
