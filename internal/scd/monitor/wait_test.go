package monitor

import (
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/a1ekseev/scd/internal/scd/config"
	"github.com/a1ekseev/scd/internal/scd/state"
)

// waitEnv is wgEnv (lines 1, 2, 4; replaceAfterFailures 2) whose source
// "wg" depends on source "up" (balancer x-up, health balancer, pause grace
// seconds). x-up selects nothing until the test sets its override
// (healthy). The clock is e.clock, moved by e.advance.
type waitClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *waitClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *waitClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func waitEnv(t *testing.T, grace int) (*env, *waitClock) {
	t.Helper()
	e := wgEnv(t, 2, 1, 2, 4)
	e.sub.Sources = append(e.sub.Sources, config.SourceConfig{ID: "up", Type: config.SourceVLESS, Enabled: true, Balancer: "x-up"})
	e.sub.Sources[0].DependsOn = []config.DependencyConfig{{Source: "up", Health: config.HealthBalancer, RecoverGraceSeconds: grace}}
	e.xray.AddBalancer("x-up", []string{"none-"}, "random")
	clock := &waitClock{now: time.Unix(1_800_000_000, 0)}
	e.deps.Now = clock.Now
	return e, clock
}

func (e *env) tick() {
	e.t.Helper()
	if err := TunnelTick(e.t.Context(), e.deps, e.sub); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) wait() state.SourceWait {
	var w state.SourceWait
	e.mem.View(e.key, func(tg *state.Target) {
		if p := tg.Waits["wg"]; p != nil {
			w = *p
		}
	})
	return w
}

// hasReplacement reports whether calls hold an AddInbound: only a
// replacement adds an inbound in the monitor.
func hasReplacement(calls []string) bool {
	return slices.ContainsFunc(calls, func(c string) bool { return strings.HasPrefix(c, "AddInbound:") })
}

// While the dependency selects nothing the failing tunnel is repaired, not
// replaced, its pair is not quarantined and the repair lines are debug;
// after the dependency recovers the pause holds the wait, its end resets
// FailuresInRow once, and the repair of the series that went on is debug
// without a flap. Every AddOutbound is Capped and remove comes before add.
func TestWaitRepairsOnly(t *testing.T) {
	e, clock := waitEnv(t, 180)
	tn := e.tunnel(0).Mapping
	e.deps.Probe = failPort(tn.Port)
	n := len(e.xray.Calls())
	for range 4 {
		e.tick()
		clock.advance(time.Minute)
	}
	calls := e.callsSince(n)
	if hasReplacement(calls) || len(e.source().Selection.Quarantine) != 0 || len(e.logs.events(t, "candidate_replaced")) != 0 {
		t.Fatalf("replaced while waiting: %q", calls)
	}
	if m := e.tunnel(0).Monitor; m.FailuresInRow != 4 || e.tunnel(0).Mapping.BaseTunnelID != tn.BaseTunnelID {
		t.Fatalf("tunnel %+v", e.tunnel(0))
	}
	repair := []string{
		"GetBalancerInfo:x-up", "ListInbounds", "RemoveRule:" + tn.RouteTag, "RemoveOutbound:" + tn.OutboundTagInitial,
		"ListOutbounds", "AddOutbound:" + tn.OutboundWithoutPrefix.Tag, "AddRule:" + tn.RouteTag + "->" + tn.OutboundWithoutPrefix.Tag,
	}
	if got := calls[:len(repair)]; !slices.Equal(got, repair) {
		t.Fatalf("calls %q, want %q", got, repair)
	}
	for _, ev := range e.logs.events(t, "tunnel_repaired") {
		if ev["level"] != "DEBUG" {
			t.Errorf("tunnel_repaired %v", ev)
		}
	}
	if ev := e.logs.events(t, "source_dependency_unhealthy"); len(ev) != 1 || ev[0]["level"] != "WARN" || ev[0]["sourceId"] != "wg" ||
		!slices.Equal(ev[0]["dependsOn"].([]any), []any{"up"}) || ev[0]["health"] != "balancer" || ev[0]["quarantineReleased"] != 0.0 {
		t.Fatalf("source_dependency_unhealthy %v", ev)
	}

	// The dependency selects again: the pause (180 s) holds the wait.
	e.xray.SetOverride("x-up", "up-1")
	e.tick()
	clock.advance(time.Minute)
	e.tick()
	if w := e.wait(); w.Since == nil || hasReplacement(e.callsSince(n)) || len(e.logs.events(t, "source_dependency_recovered")) != 0 {
		t.Fatalf("wait ended in the pause: %+v", w)
	}
	clock.advance(2 * time.Minute)
	e.deps.Probe = ok(200, 10)
	e.tick()
	ev := e.logs.events(t, "source_dependency_recovered")
	if len(ev) != 1 || ev[0]["level"] != "INFO" || ev[0]["failuresReset"] != 1.0 || ev[0]["waitedForMs"] != float64((7*time.Minute).Milliseconds()) {
		t.Fatalf("source_dependency_recovered %v", ev)
	}
	if w := e.wait(); w.Since != nil || w.ResetAt == nil {
		t.Fatalf("wait %+v", w)
	}

	// A failure right after the reset is a new series: FailuresInRow 1 and
	// a flap counted; it is not replaced (replaceAfterFailures 2).
	e.deps.Probe = failPort(tn.Port)
	clock.advance(time.Minute)
	e.tick()
	if m := e.tunnel(0).Monitor; m.FailuresInRow != 1 {
		t.Fatalf("monitor %+v", m)
	}
}

// A series that started before the end of the wait goes on after it: its
// repair is debug and counts no flap.
func TestWaitSeriesBeforeResetIsNotNew(t *testing.T) {
	e, clock := waitEnv(t, 0)
	tn := e.tunnel(0).Mapping
	e.deps.Probe = failPort(tn.Port)
	e.tick() // starts the wait, the series and a repair
	e.xray.SetOverride("x-up", "up-1")
	clock.advance(time.Minute)
	e.tick() // pause 0: the wait ends, FailuresInRow reset, then the probe fails again
	if ev := e.logs.events(t, "source_dependency_recovered"); len(ev) != 1 {
		t.Fatalf("source_dependency_recovered %v", ev)
	}
	m := e.tunnel(0).Monitor
	if m.FailuresInRow != 1 || len(m.Flap.Times) != 0 {
		t.Fatalf("monitor %+v", m)
	}
	for _, ev := range e.logs.events(t, "tunnel_repaired") {
		if ev["level"] != "DEBUG" {
			t.Errorf("tunnel_repaired %v", ev)
		}
	}
}

// ReplaceFailed asks the dependency again after the inbound check: the tick
// found it healthy, the failure comes after it went down; nothing is
// replaced, the wait starts.
func TestWaitFreshCheckInReplaceFailed(t *testing.T) {
	e, _ := waitEnv(t, 180)
	e.xray.SetOverride("x-up", "up-1")
	tn := e.tunnel(0).Mapping
	e.deps.Probe = failPort(tn.Port)
	e.setTunnel(0, func(st *state.Tunnel) { st.Monitor.FailuresInRow = 1 })
	dials := 0
	e.onDial = func() {
		dials++
		if dials == 2 { // after the tick's dependency check
			e.xray.SetOverride("x-up", "")
		}
	}
	n := len(e.xray.Calls())
	e.tick()
	calls := e.callsSince(n)
	if hasReplacement(calls) || len(e.source().Selection.Quarantine) != 0 {
		t.Fatalf("replaced: %q", calls)
	}
	i := slices.Index(calls, "ListInbounds")
	if i < 0 || i+1 >= len(calls) || calls[i+1] != "GetBalancerInfo:x-up" {
		t.Fatalf("calls %q", calls)
	}
	if w := e.wait(); w.Since == nil {
		t.Fatal("no wait")
	}
	if ev := e.logs.events(t, "source_dependency_unhealthy"); len(ev) != 1 {
		t.Fatalf("source_dependency_unhealthy %v", ev)
	}
}

// A wait releases the quarantines the source started within the pause
// before it; older ones stay.
func TestWaitReleasesRecentQuarantine(t *testing.T) {
	e, clock := waitEnv(t, 180)
	e.xray.SetOverride("x-up", "up-1")
	tn := e.tunnel(0).Mapping
	e.deps.Probe = failPort(tn.Port)
	e.tick()
	e.tick() // replaceAfterFailures 2: replaced, its pair in quarantine
	if len(e.logs.events(t, "candidate_replaced")) != 1 || len(e.source().Selection.Quarantine) != 1 {
		t.Fatal("no replacement")
	}
	e.deps.Probe = ok(200, 10)
	clock.advance(time.Minute)
	e.xray.SetOverride("x-up", "")
	e.tick()
	if q := e.source().Selection.Quarantine; len(q) != 0 {
		t.Fatalf("quarantine %v", q)
	}
	if ev := e.logs.events(t, "source_dependency_unhealthy"); len(ev) != 1 || ev[0]["quarantineReleased"] != 1.0 {
		t.Fatalf("source_dependency_unhealthy %v", ev)
	}
}

// While the Xray API is unavailable no dependency line is written and the
// source counts as waiting (no replacement); back without a wait before and the dependency
// healthy: no line and no pause. A wait that went on before ends normally.
func TestWaitXrayUnavailable(t *testing.T) {
	e, clock := waitEnv(t, 60)
	e.xray.SetOverride("x-up", "up-1")
	tn := e.tunnel(0).Mapping
	e.deps.Probe = failPort(tn.Port)
	e.xray.FailNext("GetBalancerInfo", status.Error(codes.Unavailable, "connection refused"))
	e.setTunnel(0, func(st *state.Tunnel) { st.Monitor.FailuresInRow = 5 })
	n := len(e.xray.Calls())
	e.tick()
	if hasReplacement(e.callsSince(n)) {
		t.Fatalf("replaced while the API was unavailable: %q", e.callsSince(n))
	}
	// The repair's ListInbounds answered and ended the episode again.
	if len(e.logs.events(t, "xray_unavailable")) != 1 || len(e.logs.events(t, "xray_recovered")) != 1 {
		t.Fatal("no xray_unavailable episode")
	}
	e.deps.Probe = ok(200, 10)
	clock.advance(time.Minute)
	e.tick()
	if ev := e.logs.events(t, "source_dependency_unhealthy"); len(ev) != 0 {
		t.Fatalf("source_dependency_unhealthy %v", ev)
	}
	if ev := e.logs.events(t, "source_dependency_recovered"); len(ev) != 0 {
		t.Fatalf("source_dependency_recovered %v", ev)
	}
	if w := e.wait(); w.Since != nil || w.Deps["up"].State != state.DepHealthy {
		t.Fatalf("wait %+v", w)
	}

	// A wait in progress outlives the unavailability and ends normally.
	e.xray.SetOverride("x-up", "")
	e.tick()
	e.xray.FailNext("GetBalancerInfo", status.Error(codes.Unavailable, "connection refused"))
	e.tick()
	e.xray.SetOverride("x-up", "up-1")
	e.tick() // healthy again: pause 60 s
	clock.advance(2 * time.Minute)
	e.tick()
	if ev := e.logs.events(t, "source_dependency_recovered"); len(ev) != 1 {
		t.Fatalf("source_dependency_recovered %v", ev)
	}
	if ev := e.logs.events(t, "source_dependency_unhealthy"); len(ev) != 1 {
		t.Fatalf("source_dependency_unhealthy %v", ev)
	}
}

// A source whose last check before an apply failed is treated as waiting.
func TestWaitAfterFailedCheck(t *testing.T) {
	e := wgEnv(t, 1, 1, 2, 4)
	tn := e.tunnel(0).Mapping
	e.deps.Probe = failPort(tn.Port)
	e.mem.Update(e.key, func(tg *state.Target) { tg.CheckFailed = map[string]string{"wg": "dialer_proxy_missing"} })
	n := len(e.xray.Calls())
	e.tick()
	if calls := e.callsSince(n); hasReplacement(calls) || !slices.Contains(calls, "AddRule:"+tn.RouteTag+"->"+tn.OutboundWithoutPrefix.Tag) {
		t.Fatalf("calls %q", calls)
	}
}

// A series that started in the wait goes on after its end on a tunnel that
// stayed prefixed (its repair failed): the repair after the reset is not
// skipped, and it is debug without a flap (FailingSince before ResetAt).
func TestWaitSeriesBeforeResetPrefixed(t *testing.T) {
	e, clock := waitEnv(t, 0)
	e.deps.Probe = failPort(e.tunnel(0).Mapping.Port)
	e.xray.FailNext("AddOutbound", status.Error(codes.Internal, "boom")) // the repair fails: still prefixed
	e.tick()
	e.xray.SetOverride("x-up", "up-1")
	clock.advance(time.Minute)
	e.tick()
	m := e.tunnel(0).Monitor
	if m.FailuresInRow != 1 || len(m.Flap.Times) != 0 {
		t.Fatalf("monitor %+v", m)
	}
	ev := e.logs.events(t, "tunnel_repaired")
	if len(ev) != 1 || ev[0]["level"] != "DEBUG" {
		t.Fatalf("tunnel_repaired %v", ev)
	}
}

// While waiting, a failed repair and a flapping tunnel that stays down are
// debug.
func TestWaitRepairFailedAndDownDebug(t *testing.T) {
	e, _ := waitEnv(t, 0)
	e.deps.Probe = failPort(e.tunnel(0).Mapping.Port)
	e.setTunnel(0, func(st *state.Tunnel) {
		st.Monitor.FailuresInRow = downAfter - 1
		st.Monitor.Flap = state.FlapState{Since: time.Unix(1_799_999_000, 0), Count: 3}
	})
	e.xray.FailNext("AddOutbound", status.Error(codes.Internal, "boom"))
	e.tick()
	if ev := e.logs.events(t, "tunnel_repair_failed"); len(ev) != 1 || ev[0]["level"] != "DEBUG" {
		t.Fatalf("tunnel_repair_failed %v", ev)
	}
	if ev := e.logs.events(t, "tunnel_down"); len(ev) != 1 || ev[0]["level"] != "DEBUG" {
		t.Fatalf("tunnel_down %v", ev)
	}
}

// A dependency balancer that Xray does not know is unknown: the source
// waits and nothing is replaced.
func TestWaitBalancerNotFound(t *testing.T) {
	e, _ := waitEnv(t, 0)
	e.sub.Sources[1].Balancer = "x-missing"
	e.deps.Probe = failPort(e.tunnel(0).Mapping.Port)
	e.setTunnel(0, func(st *state.Tunnel) { st.Monitor.FailuresInRow = 5 })
	n := len(e.xray.Calls())
	e.tick()
	if hasReplacement(e.callsSince(n)) {
		t.Fatalf("replaced: %q", e.callsSince(n))
	}
	if w := e.wait(); w.Since == nil || w.Deps["up"].State != state.DepUnknown {
		t.Fatalf("wait %+v", w)
	}
	if ev := e.logs.events(t, "source_dependency_unhealthy"); len(ev) != 1 || ev[0]["level"] != "WARN" {
		t.Fatalf("source_dependency_unhealthy %v", ev)
	}
}

// A quarantine older than the pause before the wait stays.
func TestWaitKeepsOldQuarantine(t *testing.T) {
	e, clock := waitEnv(t, 180)
	e.deps.Probe = ok(200, 10)
	e.xray.SetOverride("x-up", "up-1")
	e.tick()
	cands := e.source().Candidates
	e.mem.Update(e.key, func(tg *state.Target) {
		tg.Sources["wg"].Selection.QuarantinePair(cands[len(cands)-1].SelectionCandidate(), clock.Now().Add(-3*time.Minute-time.Second), 4)
	})
	e.xray.SetOverride("x-up", "")
	e.tick()
	if q := e.source().Selection.Quarantine; len(q) != 1 {
		t.Fatalf("quarantine %v", q)
	}
	if ev := e.logs.events(t, "source_dependency_unhealthy"); len(ev) != 1 || ev[0]["quarantineReleased"] != 0.0 {
		t.Fatalf("source_dependency_unhealthy %v", ev)
	}
}

// ReplaceFailed rechecks the wait under the target lock: another worker of
// the same tick may have started it after this tunnel's check.
func TestReplaceFailedWhileWaiting(t *testing.T) {
	e, clock := waitEnv(t, 180)
	e.mem.Update(e.key, func(tg *state.Target) { tg.EnsureWait("wg").Since = new(clock.Now()) })
	tn := e.tunnel(0).Mapping
	e.setTunnel(0, func(st *state.Tunnel) { st.Monitor.FailuresInRow = 5 })
	n := len(e.xray.Calls())
	done, err := ReplaceFailed(t.Context(), e.deps, e.sub, e.sub.Sources[0], tn.BaseTunnelID)
	if err != nil || done || len(e.callsSince(n)) != 0 || len(e.source().Selection.Quarantine) != 0 {
		t.Fatalf("done %v, err %v, calls %q", done, err, e.callsSince(n))
	}
}

// balWaitEnv is waitEnv whose source "wg" depends on the balancer pool-en of
// the same Xray (selects nothing until the test sets its override). With
// withUp it also keeps the dependency on source "up" (mixed); otherwise
// "up" is only a second dependent source without tunnels.
func balWaitEnv(t *testing.T, grace int, withUp, withPool bool) (*env, *waitClock) {
	t.Helper()
	e, clock := waitEnv(t, grace)
	dep := config.DependencyConfig{Balancer: "pool-en", Health: config.HealthBalancer, RecoverGraceSeconds: grace}
	if withUp {
		e.sub.Sources[0].DependsOn = append(e.sub.Sources[0].DependsOn, dep)
	} else {
		e.sub.Sources[0].DependsOn = []config.DependencyConfig{dep}
	}
	if withPool {
		e.xray.AddBalancer("pool-en", []string{"none-"}, "random")
	}
	return e, clock
}

func balancerCalls(calls []string, tag string) int {
	return strings.Count("\n"+strings.Join(calls, "\n")+"\n", "\nGetBalancerInfo:"+tag+"\n")
}

// An unhealthy dependency balancer starts the wait once; the failing tunnel
// is only repaired (ListInbounds, remove, add).
func TestWaitDependencyBalancer(t *testing.T) {
	e, _ := balWaitEnv(t, 180, false, true)
	tn := e.tunnel(0).Mapping
	e.deps.Probe = failPort(tn.Port)
	e.setTunnel(0, func(st *state.Tunnel) { st.Monitor.FailuresInRow = 5 })
	n := len(e.xray.Calls())
	e.tick()
	e.tick()
	calls := e.callsSince(n)
	if hasReplacement(calls) || len(e.source().Selection.Quarantine) != 0 {
		t.Fatalf("replaced: %q", calls)
	}
	repair := []string{
		"GetBalancerInfo:pool-en", "ListInbounds", "RemoveRule:" + tn.RouteTag, "RemoveOutbound:" + tn.OutboundTagInitial,
		"ListOutbounds", "AddOutbound:" + tn.OutboundWithoutPrefix.Tag, "AddRule:" + tn.RouteTag + "->" + tn.OutboundWithoutPrefix.Tag,
	}
	if got := calls[:len(repair)]; !slices.Equal(got, repair) {
		t.Fatalf("calls %q, want %q", got, repair)
	}
	ev := e.logs.events(t, "source_dependency_unhealthy")
	if len(ev) != 1 || ev[0]["level"] != "WARN" || ev[0]["health"] != "balancer" ||
		!slices.Equal(ev[0]["dependsOn"].([]any), []any{"balancer:pool-en"}) {
		t.Fatalf("source_dependency_unhealthy %v", ev)
	}
	if w := e.wait(); w.Since == nil || w.Deps["balancer:pool-en"].State != state.DepUnhealthy {
		t.Fatalf("wait %+v", w)
	}
}

// The dependency comes back: the pause holds the wait, replacement waits
// for its end, then FailuresInRow is reset.
func TestWaitDependencyBalancerRecovers(t *testing.T) {
	e, clock := balWaitEnv(t, 180, false, true)
	tn := e.tunnel(0).Mapping
	e.deps.Probe = failPort(tn.Port)
	e.setTunnel(0, func(st *state.Tunnel) { st.Monitor.FailuresInRow = 5 })
	e.tick()
	e.xray.SetOverride("pool-en", "via-en")
	clock.advance(time.Minute)
	n := len(e.xray.Calls())
	e.tick()
	w := e.wait()
	if w.Since == nil || w.Deps["balancer:pool-en"].RecoveredAt == nil || w.Deps["balancer:pool-en"].GraceUntil == nil || hasReplacement(e.callsSince(n)) {
		t.Fatalf("no pause: %+v, calls %q", w, e.callsSince(n))
	}
	e.deps.Probe = ok(200, 10)
	clock.advance(3 * time.Minute)
	e.tick()
	ev := e.logs.events(t, "source_dependency_recovered")
	if len(ev) != 1 || ev[0]["failuresReset"] != 1.0 || ev[0]["waitedForMs"] != float64((4*time.Minute).Milliseconds()) {
		t.Fatalf("source_dependency_recovered %v", ev)
	}
	if w := e.wait(); w.Since != nil || w.ResetAt == nil || e.tunnel(0).Monitor.FailuresInRow != 0 {
		t.Fatalf("wait %+v, monitor %+v", w, e.tunnel(0).Monitor)
	}
}

// Quarantines started within the pause before the wait are released, older
// ones stay.
func TestWaitDependencyBalancerQuarantine(t *testing.T) {
	e, clock := balWaitEnv(t, 180, false, true)
	e.xray.SetOverride("pool-en", "via-en")
	e.deps.Probe = failPort(e.tunnel(0).Mapping.Port)
	e.tick()
	e.tick() // replaceAfterFailures 2: replaced, its pair in quarantine
	if len(e.logs.events(t, "candidate_replaced")) != 1 || len(e.source().Selection.Quarantine) != 1 {
		t.Fatal("no replacement")
	}
	e.deps.Probe = ok(200, 10)
	clock.advance(time.Minute)
	e.xray.SetOverride("pool-en", "")
	e.tick()
	if q := e.source().Selection.Quarantine; len(q) != 0 {
		t.Fatalf("quarantine %v", q)
	}
	if ev := e.logs.events(t, "source_dependency_unhealthy"); len(ev) != 1 || ev[0]["quarantineReleased"] != 1.0 {
		t.Fatalf("source_dependency_unhealthy %v", ev)
	}

	e, clock = balWaitEnv(t, 180, false, true)
	e.deps.Probe = ok(200, 10)
	e.xray.SetOverride("pool-en", "via-en")
	e.tick()
	cands := e.source().Candidates
	e.mem.Update(e.key, func(tg *state.Target) {
		tg.Sources["wg"].Selection.QuarantinePair(cands[len(cands)-1].SelectionCandidate(), clock.Now().Add(-3*time.Minute-time.Second), 4)
	})
	e.xray.SetOverride("pool-en", "")
	e.tick()
	if q := e.source().Selection.Quarantine; len(q) != 1 {
		t.Fatalf("old quarantine released: %v", q)
	}
}

// The tick saw the balancer healthy, it went down before ReplaceFailed:
// ReplaceFailed starts the wait itself.
func TestWaitDependencyBalancerFresh(t *testing.T) {
	e, _ := balWaitEnv(t, 180, false, true)
	e.xray.SetOverride("pool-en", "via-en")
	e.deps.Probe = failPort(e.tunnel(0).Mapping.Port)
	e.setTunnel(0, func(st *state.Tunnel) { st.Monitor.FailuresInRow = 1 })
	dials := 0
	e.onDial = func() {
		dials++
		if dials == 2 { // after the tick's dependency check
			e.xray.SetOverride("pool-en", "")
		}
	}
	n := len(e.xray.Calls())
	e.tick()
	if hasReplacement(e.callsSince(n)) || len(e.source().Selection.Quarantine) != 0 || e.wait().Since == nil {
		t.Fatalf("calls %q, wait %+v", e.callsSince(n), e.wait())
	}
	if ev := e.logs.events(t, "source_dependency_unhealthy"); len(ev) != 1 {
		t.Fatalf("source_dependency_unhealthy %v", ev)
	}
}

// With a source and a balancer dependency the source waits while either is
// unhealthy; bad names both in config order; each has its own pause.
func TestWaitDependencyMixed(t *testing.T) {
	e, clock := balWaitEnv(t, 60, true, true)
	e.deps.Probe = ok(200, 10)
	e.tick()
	ev := e.logs.events(t, "source_dependency_unhealthy")
	if len(ev) != 1 || !slices.Equal(ev[0]["dependsOn"].([]any), []any{"up", "balancer:pool-en"}) {
		t.Fatalf("source_dependency_unhealthy %v", ev)
	}
	e.xray.SetOverride("x-up", "up-1")
	clock.advance(time.Minute)
	e.tick() // up pauses; pool-en still down
	w := e.wait()
	if w.Since == nil || w.Deps["up"].GraceUntil == nil || w.Deps["balancer:pool-en"].GraceUntil != nil {
		t.Fatalf("wait %+v", w)
	}
	e.xray.SetOverride("pool-en", "via-en")
	clock.advance(time.Minute)
	e.tick() // pool-en pauses, up's pause is over
	w = e.wait()
	if w.Since == nil || w.Deps["balancer:pool-en"].GraceUntil == nil || len(e.logs.events(t, "source_dependency_recovered")) != 0 {
		t.Fatalf("wait %+v", w)
	}
	clock.advance(2 * time.Minute)
	e.tick()
	if w := e.wait(); w.Since != nil || len(e.logs.events(t, "source_dependency_recovered")) != 1 {
		t.Fatalf("wait %+v", w)
	}
}

// Two sources waiting for one balancer cost one GetBalancerInfo per tick.
func TestWaitDependencyBalancerShared(t *testing.T) {
	e, _ := balWaitEnv(t, 180, false, true)
	e.sub.Sources[1].DependsOn = []config.DependencyConfig{{Balancer: "pool-en", Health: config.HealthBalancer, RecoverGraceSeconds: 180}}
	e.deps.Probe = ok(200, 10)
	n := len(e.xray.Calls())
	e.tick()
	if c := balancerCalls(e.callsSince(n), "pool-en"); c != 1 {
		t.Fatalf("GetBalancerInfo(pool-en) %d times: %q", c, e.callsSince(n))
	}
}

// A balancer Xray does not know is unknown: the source waits.
func TestWaitDependencyBalancerNotFound(t *testing.T) {
	e, _ := balWaitEnv(t, 180, false, false)
	e.deps.Probe = failPort(e.tunnel(0).Mapping.Port)
	e.setTunnel(0, func(st *state.Tunnel) { st.Monitor.FailuresInRow = 5 })
	n := len(e.xray.Calls())
	e.tick()
	if hasReplacement(e.callsSince(n)) {
		t.Fatalf("replaced: %q", e.callsSince(n))
	}
	if w := e.wait(); w.Since == nil || w.Deps["balancer:pool-en"].State != state.DepUnknown {
		t.Fatalf("wait %+v", w)
	}
	if ev := e.logs.events(t, "source_dependency_unhealthy"); len(ev) != 1 {
		t.Fatalf("source_dependency_unhealthy %v", ev)
	}
}

// While the API is unavailable nothing changes or is logged; the source
// counts as waiting.
func TestWaitDependencyBalancerXrayUnavailable(t *testing.T) {
	e, _ := balWaitEnv(t, 60, false, true)
	e.xray.SetOverride("pool-en", "via-en")
	e.deps.Probe = failPort(e.tunnel(0).Mapping.Port)
	e.xray.FailNext("GetBalancerInfo", status.Error(codes.Unavailable, "connection refused"))
	e.setTunnel(0, func(st *state.Tunnel) { st.Monitor.FailuresInRow = 5 })
	n := len(e.xray.Calls())
	e.tick()
	if hasReplacement(e.callsSince(n)) {
		t.Fatalf("replaced: %q", e.callsSince(n))
	}
	if ev := e.logs.events(t, "source_dependency_unhealthy"); len(ev) != 0 {
		t.Fatalf("source_dependency_unhealthy %v", ev)
	}
	if w := e.wait(); w.Since != nil || len(w.Deps) != 0 {
		t.Fatalf("wait %+v", w)
	}
}

// crossWaitEnv is waitEnv whose source "wg" depends only on balancer
// pool-blanc of subscription blanc-en (another Xray, never dialed): its
// health is read from the balancer selection state of blanc-en, which the
// test writes with e.en. dials counts the dials of the env.
func crossWaitEnv(t *testing.T, grace int) (*env, *waitClock, *int) {
	t.Helper()
	e, clock := waitEnv(t, grace)
	e.sub.Sources[0].DependsOn = []config.DependencyConfig{crossDep(grace)}
	e.deps.Subscriptions = []config.SubscriptionConfig{e.sub, {
		ID: "blanc-en", Enabled: true,
		Target: config.SubscriptionTargetConfig{Address: "10.9.9.9:8080", Balancers: []config.BalancerConfig{{
			Tag: "pool-blanc", Selection: &config.BalancerSelectionConfig{Enabled: true, Schedule: "*/15 * * * * *"},
		}}},
	}}
	dials := new(int)
	e.onDial = func() { *dials++ }
	return e, clock, dials
}

func crossDep(grace int) config.DependencyConfig {
	return config.DependencyConfig{Subscription: "blanc-en", Balancer: "pool-blanc", Health: config.HealthBalancer, RecoverGraceSeconds: grace}
}

const crossName = "balancer:blanc-en/pool-blanc"

// en sets the selection of pool-blanc of blanc-en: a successful check now
// that selects override ("" selects nothing).
func (e *env) en(now time.Time, override string) {
	key := state.Key("blanc-en", "10.9.9.9:8080")
	e.mem.Ensure(key)
	e.mem.Update(key, func(tg *state.Target) {
		s := &tg.EnsureBalancer("pool-blanc").Selection
		s.State, s.LastSuccessAt, s.LastCheckedAt, s.OverrideTarget = state.SelectionOK, new(now), new(now), override
	})
}

// An empty pool of another subscription starts the wait without any RPC
// or dial for the dependency; the pool back starts the pause, its end
// resets FailuresInRow.
func TestWaitCrossBalancer(t *testing.T) {
	e, clock, dials := crossWaitEnv(t, 60)
	e.deps.Probe = ok(200, 10)
	e.setTunnel(0, func(st *state.Tunnel) { st.Monitor.FailuresInRow = 1 })
	e.en(clock.Now(), "")
	n := len(e.xray.Calls())
	e.tick()
	if calls := e.callsSince(n); len(calls) != 0 || *dials != 0 {
		t.Fatalf("calls %q, dials %d", calls, *dials)
	}
	ev := e.logs.events(t, "source_dependency_unhealthy")
	if len(ev) != 1 || ev[0]["level"] != "WARN" || !slices.Equal(ev[0]["dependsOn"].([]any), []any{crossName}) {
		t.Fatalf("source_dependency_unhealthy %v", ev)
	}
	if w := e.wait(); w.Since == nil || w.Deps[crossName].State != state.DepUnhealthy {
		t.Fatalf("wait %+v", w)
	}
	clock.advance(15 * time.Second)
	e.en(clock.Now(), "blanc-1")
	e.tick()
	if w := e.wait(); w.Since == nil || w.Deps[crossName].GraceUntil == nil {
		t.Fatalf("no pause: %+v", w)
	}
	clock.advance(30 * time.Second)
	e.en(clock.Now(), "blanc-1")
	e.tick()
	if ev := e.logs.events(t, "source_dependency_recovered"); len(ev) != 0 {
		t.Fatalf("recovered in the pause %v", ev)
	}
	clock.advance(35 * time.Second)
	e.en(clock.Now(), "blanc-1")
	e.tick()
	if ev := e.logs.events(t, "source_dependency_recovered"); len(ev) != 1 {
		t.Fatalf("source_dependency_recovered %v", ev)
	}
	if w := e.wait(); w.Since != nil || w.ResetAt == nil || e.tunnel(0).Monitor.FailuresInRow != 0 {
		t.Fatalf("wait %+v, monitor %+v", w, e.tunnel(0).Monitor)
	}
	if *dials != 0 || len(e.callsSince(n)) != 0 {
		t.Fatalf("calls %q, dials %d", e.callsSince(n), *dials)
	}
}

// Unknown health of the other subscription's balancer starts the wait: its
// Xray is down, its state is stale, or the monitor has no subscriptions.
func TestWaitCrossBalancerUnknown(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(e *env, now time.Time)
	}{
		{"xray down", func(e *env, now time.Time) {
			e.en(now, "blanc-1")
			e.mem.Update(state.Key("blanc-en", "10.9.9.9:8080"), func(tg *state.Target) { tg.SetXrayDown("down", now) })
		}},
		{"stale", func(e *env, now time.Time) { e.en(now.Add(-46*time.Second), "blanc-1") }},
		{"never polled", func(*env, time.Time) {}},
		{"no subscriptions", func(e *env, now time.Time) { e.en(now, "blanc-1"); e.deps.Subscriptions = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, clock, _ := crossWaitEnv(t, 60)
			e.deps.Probe = ok(200, 10)
			tc.set(e, clock.Now())
			e.tick()
			if w := e.wait(); w.Since == nil || w.Deps[crossName].State != state.DepUnknown {
				t.Fatalf("wait %+v", w)
			}
		})
	}
}

// While the own Xray is down nothing changes or is logged, whatever the
// other subscription's balancer says.
func TestWaitCrossBalancerOwnXrayDown(t *testing.T) {
	e, clock, _ := crossWaitEnv(t, 60)
	e.deps.Probe = ok(200, 10)
	e.en(clock.Now(), "")
	e.mem.Update(e.key, func(tg *state.Target) { tg.SetXrayDown("down", clock.Now()) })
	e.tick()
	if w := e.wait(); w.Since != nil || len(w.Deps) != 0 {
		t.Fatalf("wait %+v", w)
	}
	if ev := e.logs.events(t, "source_dependency_unhealthy"); len(ev) != 0 {
		t.Fatalf("source_dependency_unhealthy %v", ev)
	}
}

// The tick read the pool healthy, it emptied before ReplaceFailed:
// ReplaceFailed reads the state again and starts the wait: no replacement
// (the tunnel is repaired), no quarantine.
func TestWaitCrossBalancerFresh(t *testing.T) {
	e, clock, dials := crossWaitEnv(t, 180)
	e.en(clock.Now(), "blanc-1")
	e.deps.Probe = failPort(e.tunnel(0).Mapping.Port)
	e.setTunnel(0, func(st *state.Tunnel) { st.Monitor.FailuresInRow = 1 })
	e.onDial = func() {
		*dials++
		if *dials == 1 { // ReplaceFailed: the tick dialed nothing for the dependency
			e.en(clock.Now(), "")
		}
	}
	n := len(e.xray.Calls())
	e.tick()
	if calls := e.callsSince(n); hasReplacement(calls) || len(e.source().Selection.Quarantine) != 0 || e.wait().Since == nil {
		t.Fatalf("calls %q, wait %+v", calls, e.wait())
	}
	if ev := e.logs.events(t, "source_dependency_unhealthy"); len(ev) != 1 {
		t.Fatalf("source_dependency_unhealthy %v", ev)
	}
}

// A source dependency and another subscription's balancer: both named in
// config order while bad, each with its own health.
func TestWaitCrossBalancerMixed(t *testing.T) {
	e, clock, _ := crossWaitEnv(t, 60)
	e.sub.Sources[0].DependsOn = []config.DependencyConfig{
		{Source: "up", Health: config.HealthBalancer, RecoverGraceSeconds: 60}, crossDep(60),
	}
	e.deps.Probe = ok(200, 10)
	e.en(clock.Now(), "")
	e.tick()
	ev := e.logs.events(t, "source_dependency_unhealthy")
	if len(ev) != 1 || !slices.Equal(ev[0]["dependsOn"].([]any), []any{"up", crossName}) {
		t.Fatalf("source_dependency_unhealthy %v", ev)
	}
	e.xray.SetOverride("x-up", "up-1")
	e.tick()
	if w := e.wait(); w.Since == nil || w.Deps["up"].GraceUntil == nil || w.Deps[crossName].State != state.DepUnhealthy {
		t.Fatalf("wait %+v", w)
	}
}

// The same tag in the own Xray (healthy) and in the other subscription
// (empty) are two dependencies: the source waits for the other one.
func TestWaitCrossBalancerTagCollision(t *testing.T) {
	e, clock, _ := crossWaitEnv(t, 60)
	e.sub.Sources[0].DependsOn = []config.DependencyConfig{
		{Balancer: "pool-blanc", Health: config.HealthBalancer, RecoverGraceSeconds: 60}, crossDep(60),
	}
	e.xray.AddBalancer("pool-blanc", []string{"none-"}, "random")
	e.xray.SetOverride("pool-blanc", "up-1")
	e.deps.Probe = ok(200, 10)
	e.en(clock.Now(), "")
	e.tick()
	ev := e.logs.events(t, "source_dependency_unhealthy")
	if len(ev) != 1 || !slices.Equal(ev[0]["dependsOn"].([]any), []any{crossName}) {
		t.Fatalf("source_dependency_unhealthy %v", ev)
	}
	if w := e.wait(); w.Deps["balancer:pool-blanc"].State != state.DepHealthy {
		t.Fatalf("wait %+v", w)
	}
}
