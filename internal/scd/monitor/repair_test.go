package monitor

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xtls/xray-core/app/router"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/a1ekseev/scd/internal/scd/apply"
	"github.com/a1ekseev/scd/internal/scd/config"
	"github.com/a1ekseev/scd/internal/scd/outbound"
	"github.com/a1ekseev/scd/internal/scd/resources"
	"github.com/a1ekseev/scd/internal/scd/socks"
	"github.com/a1ekseev/scd/internal/scd/state"
	"github.com/a1ekseev/scd/internal/scd/topology"
	"github.com/a1ekseev/scd/internal/scd/xrayapi"
)

func prefixed(c *config.SubscriptionConfig) { c.Target.ObservatorySubjectSelectorPrefix = prefix }

func failProbe(context.Context, socks.Request) (socks.Response, error) {
	return socks.Response{}, errors.New("probe failed")
}

// 03 §10.6 p.7: removeRule → removeOutbound(initial) → addOutbound(unprefixed)
// → addRule(route → unprefixed).
func TestRepairSequence(t *testing.T) {
	e := newEnv(t, newSub(prefixed), linkAT)
	tn := e.tunnel(0).Mapping
	e.install(tn)
	before := len(e.xray.Calls())
	e.deps.Probe = failProbe

	if err := TunnelTick(t.Context(), e.deps, e.sub); err != nil {
		t.Fatal(err)
	}
	unprefixed := tn.OutboundWithoutPrefix.Tag
	want := []string{
		"RemoveRule:" + tn.RouteTag,
		"RemoveOutbound:" + prefix + unprefixed,
		"AddOutbound:" + unprefixed,
		"AddRule:" + tn.RouteTag + "->" + unprefixed,
	}
	if got := e.callsSince(before); !slices.Equal(got, want) {
		t.Errorf("calls = %q\nwant    %q", got, want)
	}
	if got := e.xray.OutboundTags(); !slices.Equal(got, []string{unprefixed}) {
		t.Errorf("xray outbounds = %v", got)
	}
	if got := e.xray.Rules(); len(got) != 1 || got[0] != (xrayapi.Rule{Tag: unprefixed, RuleTag: tn.RouteTag}) {
		t.Errorf("xray rules = %+v", got)
	}
	cur := e.tunnel(0)
	if cur.Mapping.OutboundTagCurrent != unprefixed {
		t.Errorf("current = %q, want %q", cur.Mapping.OutboundTagCurrent, unprefixed)
	}
	m := cur.Monitor
	if m.State != state.TunnelDegraded || m.LastLatencyMs != nil || m.LastStatusCode != nil || m.ConsecutiveFailures != 0 ||
		m.LastError != "Tunnel repaired; awaiting next successful monitor check." {
		t.Errorf("monitor = %+v", m)
	}
	logs := e.logs.events(t, "tunnel_repaired")
	if len(logs) != 1 || logs[0]["level"] != "WARN" || logs[0]["msg"] != "Tunnel repaired after failed monitor probe." ||
		logs[0]["tunnelId"] != tn.BaseTunnelID || logs[0]["outboundTag"] != unprefixed ||
		logs[0]["subscriptionId"] != "source-1" || logs[0]["targetAddress"] != "127.0.0.1:8080" {
		t.Errorf("tunnel_repaired logs = %v", logs)
	}
}

func TestRepairFailureKeepsTunnel(t *testing.T) {
	e := newEnv(t, newSub(prefixed), linkAT)
	tn := e.tunnel(0).Mapping
	e.deps.Probe = failProbe
	e.xray.FailNext("AddOutbound", status.Error(codes.Unknown, "xray rejected outbound"))

	if err := TunnelTick(t.Context(), e.deps, e.sub); err != nil {
		t.Fatal(err)
	}
	cur := e.tunnel(0)
	if cur.Mapping.OutboundTagCurrent != tn.OutboundTagInitial {
		t.Errorf("current = %q, want unchanged", cur.Mapping.OutboundTagCurrent)
	}
	m := cur.Monitor
	if m.State != state.TunnelDegraded || m.LastError != "xray rejected outbound" || m.LastFailureAt == nil || m.ConsecutiveFailures != 1 {
		t.Errorf("monitor = %+v", m)
	}
	logs := e.logs.events(t, "tunnel_repair_failed")
	if len(logs) != 1 || logs[0]["level"] != "ERROR" || logs[0]["error"] != "xray rejected outbound" ||
		logs[0]["msg"] != "Tunnel repair failed." {
		t.Errorf("tunnel_repair_failed logs = %v", logs)
	}
}

// The RPC sequence of a repair is not cut short by a cancelled context
// (Review Focus 1, D7).
func TestRepairNotCanceledByContext(t *testing.T) {
	e := newEnv(t, newSub(prefixed), linkAT)
	id := e.tunnel(0).Mapping.BaseTunnelID
	ctx, cancel := context.WithCancel(t.Context())
	e.xray.Delay("AddOutbound", 100*time.Millisecond)
	e.onDial = cancel

	if err := Repair(ctx, e.deps, e.sub, id); err != nil {
		t.Fatal(err)
	}
	cur := e.tunnel(0)
	if cur.Mapping.OutboundTagCurrent != cur.Mapping.OutboundWithoutPrefix.Tag ||
		cur.Monitor.LastError != "Tunnel repaired; awaiting next successful monitor check." {
		t.Errorf("tunnel = %+v / %+v", cur.Mapping.OutboundTagCurrent, cur.Monitor)
	}
}

func TestRepairUnknownTunnelIsNoop(t *testing.T) {
	e := newEnv(t, newSub(prefixed), linkAT)
	if err := Repair(t.Context(), e.deps, e.sub, "gone"); err != nil {
		t.Fatal(err)
	}
	if err := Rejoin(t.Context(), e.deps, e.sub, "gone"); err != nil {
		t.Fatal(err)
	}
	if e.dials.Load() != 0 {
		t.Errorf("dials = %d", e.dials.Load())
	}
}

// 03 §10.6 p.8.
func TestRejoinAfterSuccess(t *testing.T) {
	e := newEnv(t, newSub(prefixed), linkAT)
	e.repairTunnel(0)
	e.deps.Probe = ok(200, 25)

	if err := TunnelTick(t.Context(), e.deps, e.sub); err != nil {
		t.Fatal(err)
	}
	cur := e.tunnel(0)
	if e.dials.Load() != 1 || cur.Mapping.OutboundTagCurrent != cur.Mapping.OutboundTagInitial {
		t.Errorf("dials=%d current=%q", e.dials.Load(), cur.Mapping.OutboundTagCurrent)
	}
	if cur.Monitor.State != state.TunnelHealthy || deref(cur.Monitor.LastLatencyMs) != 25 {
		t.Errorf("monitor = %+v", cur.Monitor)
	}
}

// 03 §10.6 p.9: rejoin fails before any RPC (the mutation client cannot be
// created): the probe result stays, with the rejoin error.
func TestRejoinFailureKeepsHealthy(t *testing.T) {
	e := newEnv(t, newSub(prefixed), linkAT)
	e.repairTunnel(0)
	e.deps.Probe = ok(200, 25)
	e.deps.Dial = func(string, time.Duration) (xrayapi.Client, error) { return nil, errors.New("xray rejected rejoin") }

	if err := TunnelTick(t.Context(), e.deps, e.sub); err != nil {
		t.Fatal(err)
	}
	cur := e.tunnel(0)
	if cur.Monitor.State != state.TunnelHealthy || deref(cur.Monitor.LastLatencyMs) != 25 {
		t.Errorf("monitor = %+v", cur.Monitor)
	}
	if cur.Mapping.OutboundTagCurrent != cur.Mapping.OutboundWithoutPrefix.Tag {
		t.Errorf("current = %q", cur.Mapping.OutboundTagCurrent)
	}
	if cur.Monitor.LastError != "Tunnel healthy but balancer rejoin failed: xray rejected rejoin" {
		t.Errorf("lastError = %q", cur.Monitor.LastError)
	}
	logs := e.logs.events(t, "tunnel_rejoin_failed")
	if len(logs) != 1 || logs[0]["level"] != "ERROR" || logs[0]["error"] != "xray rejected rejoin" ||
		logs[0]["msg"] != "Tunnel balancer rejoin failed after successful monitor probe." {
		t.Errorf("tunnel_rejoin_failed logs = %v", logs)
	}
}

// Remove before add: removeRule → removeOutbound(unprefixed) →
// addOutbound(initial) → addRule(route → initial).
func TestRejoinSequence(t *testing.T) {
	e := newEnv(t, newSub(prefixed), linkAT)
	e.repairTunnel(0)
	e.setTunnel(0, func(tn *state.Tunnel) { tn.Monitor.LastError = "previous repair" })
	tn := e.tunnel(0).Mapping
	before := len(e.xray.Calls())
	e.deps.Probe = ok(200, 25)

	if err := TunnelTick(t.Context(), e.deps, e.sub); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"RemoveRule:" + tn.RouteTag,
		"RemoveOutbound:" + tn.OutboundWithoutPrefix.Tag,
		"AddOutbound:" + tn.OutboundTagInitial,
		"AddRule:" + tn.RouteTag + "->" + tn.OutboundTagInitial,
	}
	if got := e.callsSince(before); !slices.Equal(got, want) {
		t.Errorf("calls = %q\nwant    %q", got, want)
	}
	if got := e.xray.OutboundTags(); !slices.Equal(got, []string{tn.OutboundTagInitial}) {
		t.Errorf("xray outbounds = %v", got)
	}
	cur := e.tunnel(0)
	if cur.Mapping.OutboundTagCurrent != tn.OutboundTagInitial || cur.Monitor.LastError != "" ||
		cur.Monitor.State != state.TunnelHealthy {
		t.Errorf("tunnel = %q / %+v", cur.Mapping.OutboundTagCurrent, cur.Monitor)
	}
	logs := e.logs.events(t, "tunnel_rejoined_balancer")
	if len(logs) != 1 || logs[0]["level"] != "INFO" || logs[0]["outboundTag"] != tn.OutboundTagInitial ||
		logs[0]["msg"] != "Tunnel rejoined Xray balancer after successful monitor probe." {
		t.Errorf("tunnel_rejoined_balancer logs = %v", logs)
	}
}

// 03 §10.6 p.11.
func TestRejoinSkippedWhenBalanced(t *testing.T) {
	e := newEnv(t, newSub(prefixed), linkAT)
	e.deps.Probe = ok(200, 25)
	if err := TunnelTick(t.Context(), e.deps, e.sub); err != nil {
		t.Fatal(err)
	}
	cur := e.tunnel(0)
	if e.dials.Load() != 0 {
		t.Errorf("dials = %d, want 0", e.dials.Load())
	}
	if cur.Monitor.State != state.TunnelHealthy || cur.Mapping.OutboundTagCurrent != cur.Mapping.OutboundTagInitial {
		t.Errorf("tunnel = %q / %+v", cur.Mapping.OutboundTagCurrent, cur.Monitor)
	}
}

// Without a prefix initial == unprefixed: nothing to rejoin.
func TestRejoinSkippedWithoutPrefix(t *testing.T) {
	e := newEnv(t, newSub(nil), linkAT)
	e.deps.Probe = ok(200, 25)
	if err := TunnelTick(t.Context(), e.deps, e.sub); err != nil {
		t.Fatal(err)
	}
	if e.dials.Load() != 0 {
		t.Errorf("dials = %d, want 0", e.dials.Load())
	}
}

// 03 §10.6 p.12: a failed add is rolled back to the unprefixed outbound,
// remove before add: removeOutbound(initial) → addOutbound(unprefixed) →
// addRule(route → unprefixed).
func TestRejoinRollback(t *testing.T) {
	for _, method := range []string{"AddOutbound", "AddRule"} {
		t.Run(method, func(t *testing.T) {
			e := newEnv(t, newSub(prefixed), linkAT)
			e.repairTunnel(0)
			tn := e.tunnel(0).Mapping
			before := len(e.xray.Calls())
			e.deps.Probe = ok(200, 25)
			e.xray.FailNext(method, status.Error(codes.Unknown, "rejoin add failed"))

			if err := TunnelTick(t.Context(), e.deps, e.sub); err != nil {
				t.Fatal(err)
			}
			unprefixed := tn.OutboundWithoutPrefix.Tag
			want := []string{
				"RemoveRule:" + tn.RouteTag,
				"RemoveOutbound:" + unprefixed,
				"AddOutbound:" + tn.OutboundTagInitial,
			}
			if method == "AddRule" {
				want = append(want, "AddRule:"+tn.RouteTag+"->"+tn.OutboundTagInitial)
			}
			want = append(want,
				"RemoveOutbound:"+tn.OutboundTagInitial,
				"AddOutbound:"+unprefixed,
				"AddRule:"+tn.RouteTag+"->"+unprefixed,
			)
			if got := e.callsSince(before); !slices.Equal(got, want) {
				t.Errorf("calls = %q\nwant    %q", got, want)
			}
			if got := e.xray.OutboundTags(); !slices.Equal(got, []string{unprefixed}) {
				t.Errorf("xray outbounds = %v", got)
			}
			if got := e.xray.Rules(); len(got) != 1 || got[0] != (xrayapi.Rule{Tag: unprefixed, RuleTag: tn.RouteTag}) {
				t.Errorf("xray rules = %+v", got)
			}
			cur := e.tunnel(0)
			if cur.Monitor.State != state.TunnelHealthy || deref(cur.Monitor.LastLatencyMs) != 25 {
				t.Errorf("monitor = %+v", cur.Monitor)
			}
			if cur.Mapping.OutboundTagCurrent != unprefixed {
				t.Errorf("current = %q", cur.Mapping.OutboundTagCurrent)
			}
			if cur.Monitor.LastError != "Tunnel healthy but balancer rejoin failed: rejoin add failed" {
				t.Errorf("lastError = %q", cur.Monitor.LastError)
			}
			if len(e.logs.events(t, "tunnel_rejoin_failed")) != 1 {
				t.Error("no tunnel_rejoin_failed")
			}
		})
	}
}

// failRollbackAddRule makes every AddRule after the first fail too.
type failRollbackAddRule struct {
	xrayapi.Client
	e *env
	n *atomic.Int32
}

func (c failRollbackAddRule) AddRule(ctx context.Context, r *router.RoutingRule) error {
	if c.n.Add(1) > 1 {
		c.e.xray.FailNext("AddRule", status.Error(codes.Unknown, "rollback addRule failed"))
	}
	return c.Client.AddRule(ctx, r)
}

// 03 §10.6 p.13.
func TestRejoinRollbackFailureDoesNotFailTick(t *testing.T) {
	e := newEnv(t, newSub(prefixed), linkAT, linkBE)
	e.repairTunnel(0)
	repaired, balanced := e.tunnel(0).Mapping, e.tunnel(1).Mapping
	before := len(e.xray.Calls())
	var addRules atomic.Int32
	e.wrap = func(c xrayapi.Client) xrayapi.Client { return failRollbackAddRule{Client: c, e: e, n: &addRules} }
	e.xray.FailNext("AddRule", status.Error(codes.Unknown, "addRule prefixed failed"))
	var ports []int
	e.deps.Probe = func(_ context.Context, r socks.Request) (socks.Response, error) {
		ports = append(ports, r.ProxyPort)
		return socks.Response{StatusCode: 200, LatencyMs: 25}, nil
	}
	e.sub.Target.Monitor.MaxParallel = 1 // ports is not guarded

	if err := TunnelTick(t.Context(), e.deps, e.sub); err != nil {
		t.Fatalf("tick failed: %v", err)
	}
	slices.Sort(ports)
	if !slices.Equal(ports, []int{repaired.Port, balanced.Port}) {
		t.Errorf("probed ports = %v", ports)
	}
	r, b := e.tunnel(0), e.tunnel(1)
	if r.Monitor.State != state.TunnelHealthy ||
		r.Monitor.LastError != "Tunnel healthy but balancer rejoin failed: addRule prefixed failed" ||
		r.Mapping.OutboundTagCurrent != repaired.OutboundWithoutPrefix.Tag {
		t.Errorf("repaired tunnel = %q / %+v", r.Mapping.OutboundTagCurrent, r.Monitor)
	}
	if b.Monitor.State != state.TunnelHealthy || b.Monitor.LastError != "" {
		t.Errorf("balanced tunnel = %+v", b.Monitor)
	}
	calls := e.callsSince(before)
	if !slices.Contains(calls, "RemoveOutbound:"+repaired.OutboundTagInitial) {
		t.Errorf("calls = %q, want rollback RemoveOutbound of the initial tag", calls)
	}
	if n := strings.Count(strings.Join(calls, " "), "AddRule:"); n != 2 {
		t.Errorf("AddRule calls = %d (%q)", n, calls)
	}
}

// mutations returns the outbound and rule mutations among calls.
func mutations(calls []string) []string {
	var out []string
	for _, c := range calls {
		if !strings.HasPrefix(c, "List") {
			out = append(out, c)
		}
	}
	return out
}

// A tunnel that is already out of the balancer is not repaired again: the
// failed checks only update its monitor state, with no dial and no RPC.
func TestRepairOncePerWithdrawal(t *testing.T) {
	for _, failures := range []int{2, 5} {
		e := newEnv(t, newSub(prefixed), linkAT)
		tn := e.tunnel(0).Mapping
		e.install(tn)
		before := len(e.xray.Calls())
		e.deps.Probe = failProbe

		for range failures {
			if err := TunnelTick(t.Context(), e.deps, e.sub); err != nil {
				t.Fatal(err)
			}
		}
		if got := mutations(e.callsSince(before)); len(got) != 4 {
			t.Errorf("failures=%d: calls = %q, want one repair", failures, got)
		}
		if e.dials.Load() != 1 {
			t.Errorf("failures=%d: dials = %d, want 1", failures, e.dials.Load())
		}
		cur := e.tunnel(0)
		m := cur.Monitor
		if cur.Mapping.OutboundTagCurrent != tn.OutboundWithoutPrefix.Tag || m.State != state.TunnelDegraded ||
			m.LastError != "probe failed" || m.ConsecutiveFailures != failures-1 || m.LastFailureAt == nil {
			t.Errorf("failures=%d: tunnel = %q / %+v", failures, cur.Mapping.OutboundTagCurrent, m)
		}
		if n := len(e.logs.events(t, "tunnel_repaired")); n != 1 {
			t.Errorf("failures=%d: tunnel_repaired = %d", failures, n)
		}
		skipped := e.logs.events(t, "tunnel_repair_skipped")
		if len(skipped) != failures-1 || skipped[0]["level"] != "DEBUG" ||
			skipped[0]["outboundTag"] != tn.OutboundWithoutPrefix.Tag || skipped[0]["tunnelId"] != tn.BaseTunnelID {
			t.Errorf("failures=%d: tunnel_repair_skipped = %v", failures, skipped)
		}

		// A success rejoins; the next failure repairs once again.
		before = len(e.xray.Calls())
		e.deps.Probe = ok(200, 25)
		if err := TunnelTick(t.Context(), e.deps, e.sub); err != nil {
			t.Fatal(err)
		}
		if got := e.tunnel(0).Mapping.OutboundTagCurrent; got != tn.OutboundTagInitial {
			t.Errorf("failures=%d: current after success = %q", failures, got)
		}
		e.deps.Probe = failProbe
		for range 2 {
			if err := TunnelTick(t.Context(), e.deps, e.sub); err != nil {
				t.Fatal(err)
			}
		}
		want := []string{
			"RemoveRule:" + tn.RouteTag,
			"RemoveOutbound:" + tn.OutboundWithoutPrefix.Tag,
			"AddOutbound:" + tn.OutboundTagInitial,
			"AddRule:" + tn.RouteTag + "->" + tn.OutboundTagInitial,
			"RemoveRule:" + tn.RouteTag,
			"RemoveOutbound:" + tn.OutboundTagInitial,
			"AddOutbound:" + tn.OutboundWithoutPrefix.Tag,
			"AddRule:" + tn.RouteTag + "->" + tn.OutboundWithoutPrefix.Tag,
		}
		if got := mutations(e.callsSince(before)); !slices.Equal(got, want) {
			t.Errorf("failures=%d: calls = %q\nwant    %q", failures, got, want)
		}
		if n := len(e.logs.events(t, "tunnel_repaired")); n != 2 {
			t.Errorf("failures=%d: tunnel_repaired = %d, want 2", failures, n)
		}
	}
}

// syncBack is the sync's full replace in the fake: the repaired tunnel's
// unprefixed outbound and rule give way to the prefixed ones of m. It may
// run on a monitor worker goroutine, so it reports with Error.
func (e *env) syncBack(ctx context.Context, m topology.Tunnel) {
	e.t.Helper()
	h, err := outbound.ToProto(m.OutboundInitial.JSON)
	if err != nil {
		e.t.Error(err)
		return
	}
	for _, err := range []error{
		e.client.RemoveRule(ctx, m.RouteTag),
		e.client.RemoveOutbound(ctx, m.OutboundWithoutPrefix.Tag),
		e.client.AddOutbound(ctx, h),
		e.client.AddRule(ctx, resources.Rule(m, m.OutboundTagInitial)),
	} {
		if err != nil {
			e.t.Error(err)
		}
	}
}

// A sync that replaces the topology puts the tunnel back on its prefixed
// outbound, so the next failure repairs it again.
func TestRepairAgainAfterTopologyReplaced(t *testing.T) {
	e := newEnv(t, newSub(prefixed), linkAT)
	tn := e.tunnel(0).Mapping
	e.install(tn)
	e.deps.Probe = failProbe
	if err := TunnelTick(t.Context(), e.deps, e.sub); err != nil {
		t.Fatal(err)
	}

	e.syncBack(t.Context(), tn)
	e.mem.ReplaceSourceTopology(e.key, "vless", topoOf(t, e.sub, linkAT), map[apply.Kind]string{apply.KindOutbound: "hash-new"})
	before := len(e.xray.Calls())

	for range 3 {
		if err := TunnelTick(t.Context(), e.deps, e.sub); err != nil {
			t.Fatal(err)
		}
	}
	if got := mutations(e.callsSince(before)); len(got) != 4 || got[1] != "RemoveOutbound:"+tn.OutboundTagInitial {
		t.Errorf("calls = %q, want one repair", got)
	}
	if got := e.xray.OutboundTags(); !slices.Equal(got, []string{tn.OutboundWithoutPrefix.Tag}) {
		t.Errorf("xray outbounds = %v", got)
	}
	if n := len(e.logs.events(t, "tunnel_repaired")); n != 2 {
		t.Errorf("tunnel_repaired = %d, want 2", n)
	}
}

// A repair that failed leaves the mapping prefixed, so the next failure
// tries again; after that success the tunnel is left alone.
func TestRepairRetriedAfterFailedRepair(t *testing.T) {
	e := newEnv(t, newSub(prefixed), linkAT)
	tn := e.tunnel(0).Mapping
	e.install(tn)
	e.deps.Probe = failProbe
	e.xray.FailNext("AddOutbound", status.Error(codes.Unknown, "xray rejected outbound"))
	before := len(e.xray.Calls())

	for range 3 {
		if err := TunnelTick(t.Context(), e.deps, e.sub); err != nil {
			t.Fatal(err)
		}
	}
	calls := strings.Join(mutations(e.callsSince(before)), " ")
	if n := strings.Count(calls, "AddOutbound:"+tn.OutboundWithoutPrefix.Tag); n != 2 {
		t.Errorf("AddOutbound(unprefixed) = %d, want 2 (%s)", n, calls)
	}
	if e.dials.Load() != 2 {
		t.Errorf("dials = %d, want 2", e.dials.Load())
	}
	if len(e.logs.events(t, "tunnel_repair_failed")) != 1 || len(e.logs.events(t, "tunnel_repaired")) != 1 ||
		len(e.logs.events(t, "tunnel_repair_skipped")) != 1 {
		t.Errorf("logs = %s", e.logs.String())
	}
	if got := e.xray.OutboundTags(); !slices.Equal(got, []string{tn.OutboundWithoutPrefix.Tag}) {
		t.Errorf("xray outbounds = %v", got)
	}
}

// The "already withdrawn" decision is made under the target lock, not at the
// start of the tick: a sync that replaced the topology while the probe ran
// still gets its repair.
func TestRepairDecidedAfterConcurrentSync(t *testing.T) {
	e := newEnv(t, newSub(prefixed), linkAT)
	e.repairTunnel(0)
	tn := e.tunnel(0).Mapping
	topo := topoOf(t, e.sub, linkAT)
	var before int
	e.deps.Probe = func(ctx context.Context, _ socks.Request) (socks.Response, error) {
		unlock := e.mem.LockTarget(e.key)
		e.syncBack(ctx, tn)
		e.mem.ReplaceSourceTopology(e.key, "vless", topo, map[apply.Kind]string{apply.KindOutbound: "hash-new"})
		before = len(e.xray.Calls())
		unlock()
		return socks.Response{}, errors.New("probe failed")
	}

	if err := TunnelTick(t.Context(), e.deps, e.sub); err != nil {
		t.Fatal(err)
	}
	if got := mutations(e.callsSince(before)); len(got) != 4 || got[1] != "RemoveOutbound:"+tn.OutboundTagInitial {
		t.Errorf("calls = %q, want a repair of the prefixed outbound", got)
	}
	if n := len(e.logs.events(t, "tunnel_repaired")); n != 1 {
		t.Errorf("tunnel_repaired = %d, want 1", n)
	}
}

// A failed RemoveOutbound stops repair and rejoin before any add, so the
// tunnel never has two outbounds; the route rule is restored to the old
// outbound and the failure is logged with the existing event.
func TestRemoveOutboundFailureStopsBeforeAdd(t *testing.T) {
	for _, rejoin := range []bool{false, true} {
		t.Run(map[bool]string{false: "repair", true: "rejoin"}[rejoin], func(t *testing.T) {
			e := newEnv(t, newSub(prefixed), linkAT)
			event, msg := "tunnel_repair_failed", "remove failed"
			if rejoin {
				e.repairTunnel(0)
				e.deps.Probe = ok(200, 25)
				event, msg = "tunnel_rejoin_failed", rejoinFailedPrefix+"remove failed"
			} else {
				e.install(e.tunnel(0).Mapping)
				e.deps.Probe = failProbe
			}
			tn := e.tunnel(0).Mapping
			old := tn.OutboundTagCurrent
			before := len(e.xray.Calls())
			e.xray.FailNext("RemoveOutbound", status.Error(codes.Unknown, "remove failed"))

			if err := TunnelTick(t.Context(), e.deps, e.sub); err != nil {
				t.Fatal(err)
			}
			want := []string{"RemoveRule:" + tn.RouteTag, "RemoveOutbound:" + old, "AddRule:" + tn.RouteTag + "->" + old}
			if got := e.callsSince(before); !slices.Equal(got, want) {
				t.Errorf("calls = %q\nwant    %q", got, want)
			}
			if got := e.xray.OutboundTags(); !slices.Equal(got, []string{old}) {
				t.Errorf("xray outbounds = %v", got)
			}
			if got := e.xray.Rules(); len(got) != 1 || got[0] != (xrayapi.Rule{Tag: old, RuleTag: tn.RouteTag}) {
				t.Errorf("xray rules = %+v", got)
			}
			cur := e.tunnel(0)
			if cur.Mapping.OutboundTagCurrent != old || cur.Monitor.LastError != msg {
				t.Errorf("tunnel = %q / %+v", cur.Mapping.OutboundTagCurrent, cur.Monitor)
			}
			if len(e.logs.events(t, event)) != 1 {
				t.Errorf("no %s: %s", event, e.logs.String())
			}
		})
	}
}

// An outbound already gone from Xray (a repeated repair or rejoin after a
// crash) is no error: the operation goes on and adds the new outbound.
func TestRemoveOutboundAbsentContinues(t *testing.T) {
	for _, rejoin := range []bool{false, true} {
		t.Run(map[bool]string{false: "repair", true: "rejoin"}[rejoin], func(t *testing.T) {
			e := newEnv(t, newSub(prefixed), linkAT)
			tn := e.tunnel(0).Mapping
			next := tn.OutboundWithoutPrefix.Tag
			e.deps.Probe = failProbe
			if rejoin {
				e.setTunnel(0, func(tn *state.Tunnel) { tn.Mapping.OutboundTagCurrent = tn.Mapping.OutboundWithoutPrefix.Tag })
				next = tn.OutboundTagInitial
				e.deps.Probe = ok(200, 25)
			}
			// Nothing is installed: the old outbound and rule are absent.
			if err := TunnelTick(t.Context(), e.deps, e.sub); err != nil {
				t.Fatal(err)
			}
			if got := e.xray.OutboundTags(); !slices.Equal(got, []string{next}) {
				t.Errorf("xray outbounds = %v, want [%s]", got, next)
			}
			if got := e.tunnel(0).Mapping.OutboundTagCurrent; got != next {
				t.Errorf("current = %q, want %q", got, next)
			}
		})
	}
}
