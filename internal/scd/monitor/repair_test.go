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

	"github.com/a1ekseev/scd/internal/scd/config"
	"github.com/a1ekseev/scd/internal/scd/socks"
	"github.com/a1ekseev/scd/internal/scd/state"
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

// 03 §10.6 p.10: addOutbound(initial) → removeRule → addRule(route → initial)
// → removeOutbound(unprefixed).
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
		"AddOutbound:" + tn.OutboundTagInitial,
		"RemoveRule:" + tn.RouteTag,
		"AddRule:" + tn.RouteTag + "->" + tn.OutboundTagInitial,
		"RemoveOutbound:" + tn.OutboundWithoutPrefix.Tag,
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

// 03 §10.6 p.12.
func TestRejoinRollbackOnAddRuleFailure(t *testing.T) {
	e := newEnv(t, newSub(prefixed), linkAT)
	e.repairTunnel(0)
	tn := e.tunnel(0).Mapping
	before := len(e.xray.Calls())
	e.deps.Probe = ok(200, 25)
	e.xray.FailNext("AddRule", status.Error(codes.Unknown, "addRule prefixed failed"))

	if err := TunnelTick(t.Context(), e.deps, e.sub); err != nil {
		t.Fatal(err)
	}
	unprefixed := tn.OutboundWithoutPrefix.Tag
	want := []string{
		"AddOutbound:" + tn.OutboundTagInitial,
		"RemoveRule:" + tn.RouteTag,
		"AddRule:" + tn.RouteTag + "->" + tn.OutboundTagInitial,
		"RemoveRule:" + tn.RouteTag,
		"AddRule:" + tn.RouteTag + "->" + unprefixed,
		"RemoveOutbound:" + tn.OutboundTagInitial,
	}
	if got := e.callsSince(before); !slices.Equal(got, want) {
		t.Errorf("calls = %q\nwant    %q", got, want)
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
	if cur.Monitor.LastError != "Tunnel healthy but balancer rejoin failed: addRule prefixed failed" {
		t.Errorf("lastError = %q", cur.Monitor.LastError)
	}
	if len(e.logs.events(t, "tunnel_rejoin_failed")) != 1 {
		t.Error("no tunnel_rejoin_failed")
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
