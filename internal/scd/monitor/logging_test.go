package monitor

import (
	"context"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	core "github.com/xtls/xray-core/core"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/a1ekseev/scd/internal/scd/apply"
	"github.com/a1ekseev/scd/internal/scd/config"
	"github.com/a1ekseev/scd/internal/scd/manifest"
	"github.com/a1ekseev/scd/internal/scd/normalize"
	"github.com/a1ekseev/scd/internal/scd/socks"
	"github.com/a1ekseev/scd/internal/scd/state"
	"github.com/a1ekseev/scd/internal/scd/topology"
	"github.com/a1ekseev/scd/internal/scd/xrayapi"
)

// levels returns the levels of the event in log order.
func (l *lockedBuffer) levels(t *testing.T, event string) []string {
	t.Helper()
	var out []string
	for _, e := range l.events(t, event) {
		out = append(out, e["level"].(string))
	}
	return out
}

// Two repairs failing with the same error are one error line and one debug
// line; a different error is an error line again. An error that is not an
// unavailable API leaves xray_unavailable out.
func TestRepairFailedEdge(t *testing.T) {
	e := newEnv(t, newSub(prefixed), linkAT)
	e.deps.Probe = failProbe
	for _, msg := range []string{"xray rejected outbound", "xray rejected outbound", "xray is busy"} {
		e.xray.FailNext("AddOutbound", status.Error(codes.Unknown, msg))
		if err := TunnelTick(t.Context(), e.deps, e.sub); err != nil {
			t.Fatal(err)
		}
	}
	if got := e.logs.levels(t, "tunnel_repair_failed"); !slices.Equal(got, []string{"ERROR", "DEBUG", "ERROR"}) {
		t.Errorf("tunnel_repair_failed levels %v", got)
	}
	if ev := e.logs.events(t, "tunnel_repair_failed"); ev[0]["sourceId"] != "vless" {
		t.Errorf("sourceId %v", ev[0])
	}
	if ev := e.logs.events(t, "xray_unavailable"); len(ev) != 0 {
		t.Errorf("xray_unavailable %v", ev)
	}
}

// tunnel_recovered follows the first good check after a failure, with the
// time since the first failed check; a healthy tunnel never logs it.
func TestTunnelRecovered(t *testing.T) {
	e := newEnv(t, newSub(nil), linkAT)
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	e.deps.Now = func() time.Time { return now }
	tick := func(probe func(context.Context, socks.Request) (socks.Response, error)) {
		t.Helper()
		e.deps.Probe = probe
		if err := TunnelTick(t.Context(), e.deps, e.sub); err != nil {
			t.Fatal(err)
		}
		now = now.Add(time.Minute)
	}
	tick(ok(200, 10))
	tick(ok(200, 10))
	tick(failProbe)
	tick(failProbe)
	tick(ok(200, 10))
	tick(ok(200, 10))
	ev := e.logs.events(t, "tunnel_recovered")
	if len(ev) != 1 || ev[0]["level"] != "INFO" || ev[0]["failedForMs"] != 120000.0 || ev[0]["sourceId"] != "vless" ||
		ev[0]["tunnelId"] != e.tunnel(0).Mapping.BaseTunnelID || ev[0]["subscriptionId"] != "source-1" {
		t.Fatalf("tunnel_recovered %v", ev)
	}
	if m := e.tunnel(0).Monitor; m.FailingSince != nil || m.LastRepairError != "" {
		t.Fatalf("monitor %+v", m)
	}
}

// healthy → degraded → degraded → healthy is one warn and one info.
func TestBalancerDegradedRecovered(t *testing.T) {
	e := newEnv(t, newSub(withBalancer(204, "", false)))
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	e.deps.Now = func() time.Time { return now }
	for _, code := range []int{204, 500, 500, 204, 204} {
		e.deps.Probe = ok(code, 10)
		if err := BalancerTick(t.Context(), e.deps, e.sub); err != nil {
			t.Fatal(err)
		}
		now = now.Add(time.Minute)
	}
	if got := e.logs.levels(t, "balancer_degraded"); !slices.Equal(got, []string{"WARN"}) {
		t.Errorf("balancer_degraded %v", got)
	}
	rec := e.logs.events(t, "balancer_recovered")
	if len(rec) != 1 || rec[0]["level"] != "INFO" || rec[0]["failedForMs"] != 120000.0 || rec[0]["subscriptionId"] != "source-1" {
		t.Errorf("balancer_recovered %v", rec)
	}
}

// The same remote ping failure twice is one warn line, then debug.
func TestRemotePingFailedEdge(t *testing.T) {
	e := newEnv(t, newSub(withBalancer(204, "https://push.example.test/api/push/token", false)))
	e.deps.Probe = ok(204, 10)
	var pushes []directCall
	e.deps.Direct = recordDirect(&pushes, 500)
	for range 2 {
		if err := BalancerTick(t.Context(), e.deps, e.sub); err != nil {
			t.Fatal(err)
		}
		e.bg.Wait()
	}
	e.deps.Direct = recordDirect(&pushes, 502)
	if err := BalancerTick(t.Context(), e.deps, e.sub); err != nil {
		t.Fatal(err)
	}
	e.bg.Wait()
	if got := e.logs.levels(t, "balancer_monitor_remote_ping_failed"); !slices.Equal(got, []string{"WARN", "DEBUG", "WARN"}) {
		t.Errorf("remote ping failed levels %v", got)
	}
}

// A repaired tunnel with an observatory prefix recovers with one info line:
// tunnel_rejoined_balancer with failedForMs, no tunnel_recovered.
func TestRejoinIsTheRecoveryLine(t *testing.T) {
	e := newEnv(t, newSub(prefixed), linkAT)
	e.install(e.tunnel(0).Mapping)
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	e.deps.Now = func() time.Time { return now }
	for _, probe := range []func(context.Context, socks.Request) (socks.Response, error){failProbe, ok(200, 10), ok(200, 10)} {
		e.deps.Probe = probe
		if err := TunnelTick(t.Context(), e.deps, e.sub); err != nil {
			t.Fatal(err)
		}
		now = now.Add(time.Minute)
	}
	if ev := e.logs.events(t, "tunnel_recovered"); len(ev) != 0 {
		t.Errorf("tunnel_recovered %v", ev)
	}
	rj := e.logs.events(t, "tunnel_rejoined_balancer")
	if len(rj) != 1 || rj[0]["failedForMs"] != 60000.0 {
		t.Errorf("tunnel_rejoined_balancer %v", rj)
	}
}

// clock is a test clock for Deps.Now.
type clock struct{ now time.Time }

func newClock(e *env) *clock {
	c := &clock{time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)}
	e.deps.Now = func() time.Time { return c.now }
	return c
}

// tickAt runs one tunnel tick at offset at from the clock start with probe.
func (e *env) tickAt(c *clock, at time.Duration, probe func(context.Context, socks.Request) (socks.Response, error)) {
	e.t.Helper()
	c.now = time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC).Add(at)
	e.deps.Probe = probe
	if err := TunnelTick(e.t.Context(), e.deps, e.sub); err != nil {
		e.t.Fatal(err)
	}
}

// flapEnv is one prefixed tunnel installed in the fake.
func flapEnv(t *testing.T) (*env, *clock) {
	e := newEnv(t, newSub(prefixed), linkAT)
	e.install(e.tunnel(0).Mapping)
	return e, newClock(e)
}

// flaps runs n failed-then-good check pairs, one minute apart, from start.
func (e *env) flaps(c *clock, start time.Duration, n int) {
	e.t.Helper()
	for i := range n {
		at := start + time.Duration(2*i)*time.Minute
		e.tickAt(c, at, failProbe)
		e.tickAt(c, at+time.Minute, ok(200, 10))
	}
}

// The third flap within the window is one tunnel_flapping warn; from then
// on the tunnel's repairs and rejoins are debug.
func TestFlappingAfterThreeFlaps(t *testing.T) {
	e, c := flapEnv(t)
	e.flaps(c, 0, 3)
	if got := e.logs.levels(t, "tunnel_repaired"); !slices.Equal(got, []string{"WARN", "WARN", "DEBUG"}) {
		t.Errorf("tunnel_repaired %v", got)
	}
	if got := e.logs.levels(t, "tunnel_rejoined_balancer"); !slices.Equal(got, []string{"INFO", "INFO", "DEBUG"}) {
		t.Errorf("tunnel_rejoined_balancer %v", got)
	}
	ev := e.logs.events(t, "tunnel_flapping")
	if len(ev) != 1 || ev[0]["level"] != "WARN" || ev[0]["flaps"] != 3.0 || ev[0]["windowMin"] != 60.0 ||
		ev[0]["probeError"] != "probe failed" || ev[0]["tunnelId"] != e.tunnel(0).Mapping.BaseTunnelID || ev[0]["sourceId"] != "vless" {
		t.Fatalf("tunnel_flapping %v", ev)
	}
	e.flaps(c, 6*time.Minute, 1)
	if got := e.logs.levels(t, "tunnel_repaired"); got[3] != "DEBUG" || len(e.logs.events(t, "tunnel_flapping")) != 1 {
		t.Errorf("fourth flap: tunnel_repaired %v", got)
	}
	if got := e.logs.levels(t, "tunnel_rejoined_balancer"); got[3] != "DEBUG" {
		t.Errorf("fourth flap: tunnel_rejoined_balancer %v", got)
	}
	if f := e.tunnel(0).Monitor.Flap; f.Count != 4 {
		t.Errorf("flap %+v", f)
	}
}

// Flaps further apart than the window never start an episode.
func TestFlapWindowSlides(t *testing.T) {
	e, c := flapEnv(t)
	for i := range 3 {
		e.flaps(c, time.Duration(31*i)*time.Minute, 1)
	}
	if ev := e.logs.events(t, "tunnel_flapping"); len(ev) != 0 {
		t.Fatalf("tunnel_flapping %v", ev)
	}
	if got := e.logs.levels(t, "tunnel_repaired"); !slices.Equal(got, []string{"WARN", "WARN", "WARN"}) {
		t.Errorf("tunnel_repaired %v", got)
	}
}

// An episode ends with tunnel_stable after stableAfter without a failed
// check; the next flap is warn again.
func TestTunnelStable(t *testing.T) {
	e, c := flapEnv(t)
	e.flaps(c, 0, 3) // the last failed check at 4 min
	e.tickAt(c, 63*time.Minute, ok(200, 10))
	if ev := e.logs.events(t, "tunnel_stable"); len(ev) != 0 {
		t.Fatalf("tunnel_stable too early %v", ev)
	}
	e.tickAt(c, 64*time.Minute, ok(200, 10))
	ev := e.logs.events(t, "tunnel_stable")
	if len(ev) != 1 || ev[0]["level"] != "INFO" || ev[0]["flaps"] != 3.0 || ev[0]["flappedForMs"] != 240000.0 {
		t.Fatalf("tunnel_stable %v", ev)
	}
	e.flaps(c, 70*time.Minute, 1)
	if got := e.logs.levels(t, "tunnel_repaired"); got[3] != "WARN" {
		t.Errorf("tunnel_repaired %v", got)
	}
}

// A flapping tunnel that fails downAfter checks in a row logs tunnel_down
// once and leaves the episode: its recovery is info again.
func TestTunnelDown(t *testing.T) {
	e, c := flapEnv(t)
	e.flaps(c, 0, 3)
	for i := range 4 {
		e.tickAt(c, time.Duration(10+i)*time.Minute, failProbe)
	}
	ev := e.logs.events(t, "tunnel_down")
	if len(ev) != 1 || ev[0]["level"] != "WARN" || ev[0]["failuresInRow"] != 3.0 || ev[0]["flaps"] != 4.0 || ev[0]["probeError"] != "probe failed" {
		t.Fatalf("tunnel_down %v", ev)
	}
	e.tickAt(c, 14*time.Minute, ok(200, 10))
	rj := e.logs.events(t, "tunnel_rejoined_balancer")
	if last := rj[len(rj)-1]; last["level"] != "INFO" || last["failedForMs"] != 240000.0 {
		t.Fatalf("tunnel_rejoined_balancer %v", last)
	}
}

// A repair that fails is no flap.
func TestFlapNotCountedWithoutRepair(t *testing.T) {
	e, c := flapEnv(t)
	for i := range 3 {
		e.xray.FailNext("RemoveOutbound", status.Error(codes.Unknown, "remove failed"))
		e.flaps(c, time.Duration(2*i)*time.Minute, 1)
	}
	if ev := e.logs.events(t, "tunnel_flapping"); len(ev) != 0 || len(e.logs.events(t, "tunnel_repaired")) != 0 {
		t.Fatalf("tunnel_flapping %v", ev)
	}
}

// A tunnel without an observatory prefix is repaired on every failed check
// of a series; only the first repair is warn.
func TestNoPrefixRepairedOncePerSeries(t *testing.T) {
	e := newEnv(t, newSub(nil), linkAT)
	e.install(e.tunnel(0).Mapping)
	c := newClock(e)
	for i := range 3 {
		e.tickAt(c, time.Duration(i)*time.Minute, failProbe)
	}
	if got := e.logs.levels(t, "tunnel_repaired"); !slices.Equal(got, []string{"WARN", "DEBUG", "DEBUG"}) {
		t.Errorf("tunnel_repaired %v", got)
	}
}

// downClient fails every ListInbounds as an unreachable Xray, with texts
// from texts in turn.
type downClient struct {
	xrayapi.Client
	texts []string
	n     *atomic.Int32
}

func (c downClient) ListInbounds(context.Context) ([]*core.InboundHandlerConfig, error) {
	i := int(c.n.Add(1)-1) % len(c.texts)
	return nil, &xrayapi.Error{Message: c.texts[i], Unavailable: true}
}

func (e *env) xrayDown(texts ...string) {
	n := &atomic.Int32{}
	e.wrap = func(c xrayapi.Client) xrayapi.Client { return downClient{c, texts, n} }
}

// An unreachable Xray is one xray_unavailable line for the target; the
// repairs that fail with it are debug. A good probe does not end it, a
// successful API call does (xray_recovered).
func TestXrayUnavailableOncePerTarget(t *testing.T) {
	e := newEnv(t, newSub(prefixed), linkAT, linkBE, linkBG)
	for _, tn := range e.tunnels() {
		e.install(tn.Mapping)
	}
	c := newClock(e)
	e.xrayDown("connection error: desc = \"transport: Error while dialing: dial tcp: lookup xray: no such host\"")
	e.tickAt(c, 0, failProbe)
	if got := e.logs.levels(t, "xray_unavailable"); !slices.Equal(got, []string{"ERROR"}) {
		t.Fatalf("xray_unavailable %v", got)
	}
	if got := e.logs.levels(t, "tunnel_repair_failed"); !slices.Equal(got, []string{"DEBUG", "DEBUG", "DEBUG"}) {
		t.Fatalf("tunnel_repair_failed %v", got)
	}
	n := len(e.logs.String())
	e.tickAt(c, time.Minute, failProbe)
	if strings.Contains(e.logs.String()[n:], `"level":"INFO"`) || strings.Contains(e.logs.String()[n:], `"level":"WARN"`) ||
		strings.Contains(e.logs.String()[n:], `"level":"ERROR"`) {
		t.Fatalf("second tick:\n%s", e.logs.String()[n:])
	}
	e.wrap = nil
	e.tickAt(c, 2*time.Minute, ok(200, 10))
	if ev := e.logs.events(t, "xray_recovered"); len(ev) != 0 {
		t.Fatalf("xray_recovered after a probe %v", ev)
	}
	e.tickAt(c, 3*time.Minute, failProbe)
	ev := e.logs.events(t, "xray_recovered")
	if len(ev) != 1 || ev[0]["level"] != "INFO" || ev[0]["failedForMs"] != 180000.0 || ev[0]["targetAddress"] != "127.0.0.1:8080" {
		t.Fatalf("xray_recovered %v", ev)
	}
}

// Texts that alternate within one outage are still one line.
func TestXrayDownTextAlternates(t *testing.T) {
	e := newEnv(t, newSub(prefixed), linkAT)
	c := newClock(e)
	e.xrayDown("Request to Xray API at 127.0.0.1:8080 timed out after 5000ms.", "connection error")
	for i := range 4 {
		e.tickAt(c, time.Duration(i)*time.Minute, failProbe)
	}
	if got := e.logs.levels(t, "xray_unavailable"); !slices.Equal(got, []string{"ERROR"}) {
		t.Fatalf("xray_unavailable %v", got)
	}
}

// The balancer probe goes through the external SOCKS, not the API: it
// does not end an outage.
func TestBalancerProbeKeepsXrayDown(t *testing.T) {
	e := newEnv(t, newSub(withBalancer(204, "", false)))
	e.mem.Update(e.key, func(tg *state.Target) { tg.SetXrayDown("down", time.Now()) })
	e.deps.Probe = ok(204, 10)
	if err := BalancerTick(t.Context(), e.deps, e.sub); err != nil {
		t.Fatal(err)
	}
	down := false
	e.mem.View(e.key, func(tg *state.Target) { down = tg.XrayDown != nil })
	if !down || len(e.logs.events(t, "xray_recovered")) != 0 {
		t.Fatalf("down %v, logs %s", down, e.logs.String())
	}
}

// A tunnel whose inbound is gone from Xray is not repaired: one
// ListInbounds, its source's hashes reset, xray_state_lost warn for the
// first tunnel of the target and debug for the next. Another source with
// its inbounds in place is repaired as usual.
func TestRepairMissingInbound(t *testing.T) {
	sub := newSub(func(c *config.SubscriptionConfig) {
		prefixed(c)
		c.Sources = append(c.Sources, config.SourceConfig{ID: "b", Type: "vless", Enabled: true, PortRange: &config.PortRange{Start: 20100, End: 20110}})
	})
	e := newEnv(t, sub, linkAT, linkBG)
	m := manifest.Build(linkBE, "inline", normalize.Policy{TagPrefix: "b"}, time.Unix(0, 0))
	topo, _ := topology.BuildSource(m, sub.Sources[1], sub.Target)
	e.mem.ReplaceSourceTopology(e.key, "b", topo, map[apply.Kind]string{apply.KindOutbound: "h"})
	for _, tn := range e.tunnels() {
		e.install(tn.Mapping)
	}
	e.addInbound(topo.Tunnels[0])
	var lost []state.Tunnel
	for _, tn := range e.tunnels() {
		if tn.Mapping.SourceID == "vless" {
			lost = append(lost, tn)
			if err := e.client.RemoveInbound(t.Context(), tn.Mapping.InboundTag); err != nil {
				t.Fatal(err)
			}
		}
	}
	n := len(e.xray.Calls())
	e.deps.Probe = func(ctx context.Context, r socks.Request) (socks.Response, error) {
		if r.ProxyPort == topo.Tunnels[0].Port {
			return ok(200, 10)(ctx, r)
		}
		return failProbe(ctx, r)
	}
	if err := TunnelTick(t.Context(), e.deps, e.sub); err != nil {
		t.Fatal(err)
	}
	if got := e.callsSince(n); !slices.Equal(got, []string{"ListInbounds", "ListInbounds"}) {
		t.Fatalf("calls %q", got)
	}
	if got := e.logs.levels(t, "xray_state_lost"); !slices.Equal(got, []string{"WARN", "DEBUG"}) {
		t.Fatalf("xray_state_lost %v", got)
	}
	if len(e.logs.events(t, "tunnel_repaired")) != 0 || len(lost) != 2 || e.tunnel(1).Monitor.LastError != stateLostMessage {
		t.Fatalf("tunnel %+v", e.tunnel(1).Monitor)
	}
	var hashA, hashB int
	e.mem.View(e.key, func(tg *state.Target) { hashA, hashB = len(tg.Sources["vless"].Hashes), len(tg.Sources["b"].Hashes) })
	if hashA != 0 || hashB != 1 {
		t.Fatalf("hashes vless %d, b %d", hashA, hashB)
	}
	e.deps.Probe = failPort(topo.Tunnels[0].Port)
	n = len(e.xray.Calls())
	if err := TunnelTick(t.Context(), e.deps, e.sub); err != nil {
		t.Fatal(err)
	}
	if ev := e.logs.events(t, "tunnel_repaired"); len(ev) != 1 || ev[0]["sourceId"] != "b" || ev[0]["level"] != "WARN" {
		t.Fatalf("tunnel_repaired %v", ev)
	}
	if got := mutations(e.callsSince(n)); len(got) != 4 {
		t.Fatalf("calls %q", got)
	}
}

// A tunnel that was failing before a full apply of the same tag logs its
// recovery with the time since the first failed check, unless the target
// lost its Xray state (xray_state_restored covers it).
func TestRecoveredAfterFullApply(t *testing.T) {
	for _, lost := range []bool{false, true} {
		e := newEnv(t, newSub(nil), linkAT)
		e.install(e.tunnel(0).Mapping)
		c := newClock(e)
		e.xray.FailNext("RemoveOutbound", status.Error(codes.Unknown, "remove failed"))
		e.tickAt(c, 0, failProbe)
		if lost {
			e.mem.Update(e.key, func(tg *state.Target) { tg.SetStateLost("vless", c.now) })
		}
		e.mem.ReplaceSourceTopology(e.key, "vless", topoOf(t, e.sub, linkAT), map[apply.Kind]string{apply.KindOutbound: "h"})
		e.tickAt(c, 2*time.Minute, ok(200, 10))
		ev := e.logs.events(t, "tunnel_recovered")
		if lost && len(ev) != 0 || !lost && (len(ev) != 1 || ev[0]["failedForMs"] != 120000.0) {
			t.Errorf("lost=%v: tunnel_recovered %v", lost, ev)
		}
	}
}

// A rejoin failing with the same error on every good probe is one error
// line, then debug; a new error is an error again, and a successful rejoin
// ends it (tunnel_rejoined_balancer).
func TestRejoinFailedEdge(t *testing.T) {
	e := newEnv(t, newSub(prefixed), linkAT)
	e.repairTunnel(0)
	for _, msg := range []string{"busy", "busy", "rejected"} {
		e.xray.FailNext("RemoveOutbound", status.Error(codes.Unknown, msg))
		e.deps.Probe = ok(200, 10)
		if err := TunnelTick(t.Context(), e.deps, e.sub); err != nil {
			t.Fatal(err)
		}
	}
	if got := e.logs.levels(t, "tunnel_rejoin_failed"); !slices.Equal(got, []string{"ERROR", "DEBUG", "ERROR"}) {
		t.Fatalf("tunnel_rejoin_failed %v", got)
	}
	if err := TunnelTick(t.Context(), e.deps, e.sub); err != nil {
		t.Fatal(err)
	}
	if m := e.tunnel(0).Monitor; m.LastRejoinError != "" || len(e.logs.events(t, "tunnel_rejoined_balancer")) != 1 {
		t.Fatalf("monitor %+v", m)
	}
}

// While Xray is unavailable a failed rejoin is debug next to
// xray_unavailable.
func TestRejoinFailedXrayUnavailable(t *testing.T) {
	e := newEnv(t, newSub(prefixed), linkAT)
	e.repairTunnel(0)
	e.xray.FailNext("RemoveOutbound", status.Error(codes.Unavailable, "connection error"))
	e.deps.Probe = ok(200, 10)
	if err := TunnelTick(t.Context(), e.deps, e.sub); err != nil {
		t.Fatal(err)
	}
	if got := e.logs.levels(t, "tunnel_rejoin_failed"); !slices.Equal(got, []string{"DEBUG"}) {
		t.Fatalf("tunnel_rejoin_failed %v", got)
	}
	if got := e.logs.levels(t, "xray_unavailable"); !slices.Equal(got, []string{"ERROR"}) {
		t.Fatalf("xray_unavailable %v", got)
	}
}

// A flap episode survives a full apply of the same tag and ends with
// tunnel_stable; a new tag starts without one.
func TestTunnelStableAfterFullApply(t *testing.T) {
	e, c := flapEnv(t)
	e.flaps(c, 0, 3)
	e.mem.ReplaceSourceTopology(e.key, "vless", topoOf(t, e.sub, linkAT, linkBE), map[apply.Kind]string{apply.KindOutbound: "h"})
	e.install(e.tunnel(1).Mapping)
	e.addInbound(e.tunnel(1).Mapping)
	if f := e.tunnel(1).Monitor.Flap; !f.Since.IsZero() {
		t.Fatalf("new tag carries %+v", f)
	}
	e.tickAt(c, 64*time.Minute, ok(200, 10))
	if ev := e.logs.events(t, "tunnel_stable"); len(ev) != 1 || ev[0]["tunnelId"] != e.tunnel(0).Mapping.BaseTunnelID {
		t.Fatalf("tunnel_stable %v", ev)
	}
}
