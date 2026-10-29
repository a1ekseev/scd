package monitor

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/a1ekseev/scd/internal/scd/apply"
	"github.com/a1ekseev/scd/internal/scd/config"
	"github.com/a1ekseev/scd/internal/scd/manifest"
	"github.com/a1ekseev/scd/internal/scd/selection"
	"github.com/a1ekseev/scd/internal/scd/socks"
	"github.com/a1ekseev/scd/internal/scd/state"
	"github.com/a1ekseev/scd/internal/scd/topology"
)

// wgEnv is an env with wireguard source "wg" (maxActive 2 by score, ports
// 20100..20103) whose last sync had the fixture lines; its active tunnels
// are installed in the fake.
func wgEnv(t *testing.T, replaceAfter int, lines ...int) *env {
	t.Helper()
	b, err := os.ReadFile("../../../docker/fixtures/wireguard/candidates.txt")
	if err != nil {
		t.Fatal(err)
	}
	all := strings.Split(strings.TrimSpace(string(b)), "\n")
	var text []string
	for _, n := range lines {
		text = append(text, all[n-1])
	}
	sub := newSub(func(c *config.SubscriptionConfig) {
		c.Target.ObservatorySubjectSelectorPrefix = prefix
		c.Sources = []config.SourceConfig{{
			ID: "wg", Type: config.SourceWireGuard, Enabled: true, MaxActive: 2,
			PortRange: &config.PortRange{Start: 20100, End: 20103},
			Selection: &config.SelectionConfig{Strategy: config.StrategyScore, SwitchConfirmChecks: 1, DeadPairCooldownMinutes: 4, ReplaceAfterFailures: replaceAfter},
			WireGuard: &config.WireGuardConfig{EndpointDomainStrategy: "UseIPv4"},
		}}
	})
	e := newEnv(t, sub)
	m := manifest.BuildWireGuard(strings.Join(text, "\n"), "inline", "wg", "UseIPv4", time.Unix(0, 0))
	topo, _ := topology.BuildSource(m, sub.Sources[0], sub.Target)
	e.mem.ReplaceSourceTopology(e.key, "wg", topo, map[apply.Kind]string{apply.KindOutbound: "h", apply.KindInbound: "h", apply.KindRouting: "h"})
	e.mem.Update(e.key, func(tg *state.Target) {
		tg.Sources["wg"].Candidates = selection.Rank(m.Entries, config.StrategyScore, manifest.Entry.SelectionCandidate)
	})
	for _, tn := range topo.Tunnels {
		e.install(tn)
		e.addInbound(tn)
	}
	return e
}

// failPort fails the probes of port and passes the others.
func failPort(port int) func(context.Context, socks.Request) (socks.Response, error) {
	return func(ctx context.Context, r socks.Request) (socks.Response, error) {
		if r.ProxyPort == port {
			return failProbe(ctx, r)
		}
		return ok(200, 10)(ctx, r)
	}
}

func (e *env) source() state.Source {
	var s state.Source
	e.mem.View(e.key, func(tg *state.Target) { s = *tg.Sources["wg"] })
	return s
}

// FailuresInRow counts on through the repair (which resets
// ConsecutiveFailures) and the skipped repair of the withdrawn tunnel;
// the third failure replaces the tunnel in its slot: remove before add,
// its pair in quarantine.
func TestReplaceAfterFailuresInRow(t *testing.T) {
	e := wgEnv(t, 3, 1, 2, 4)
	old := e.tunnel(0).Mapping
	e.deps.Probe = failPort(old.Port)
	for i := 1; i <= 2; i++ {
		if err := TunnelTick(t.Context(), e.deps, e.sub); err != nil {
			t.Fatal(err)
		}
		if m := e.tunnel(0).Monitor; m.FailuresInRow != i || e.tunnel(0).Mapping.BaseTunnelID != old.BaseTunnelID {
			t.Fatalf("check %d: %+v", i, m)
		}
	}
	if n := len(e.logs.events(t, "tunnel_repair_skipped")); n != 1 {
		t.Fatalf("tunnel_repair_skipped %d", n)
	}
	n := len(e.xray.Calls())
	if err := TunnelTick(t.Context(), e.deps, e.sub); err != nil {
		t.Fatal(err)
	}
	next := e.tunnel(0).Mapping
	if next.BaseTunnelID == old.BaseTunnelID || next.Port != old.Port || e.tunnel(0).Monitor.FailuresInRow != 0 {
		t.Fatalf("tunnel %+v", e.tunnel(0))
	}
	want := []string{
		"ListInbounds", "RemoveRule:" + old.RouteTag, "RemoveOutbound:" + old.OutboundWithoutPrefix.Tag, "RemoveOutbound:" + old.OutboundTagInitial,
		"RemoveInbound:" + old.InboundTag, "ListOutbounds", "AddOutbound:" + next.OutboundTagInitial, "AddInbound:" + next.InboundTag, "AddRule:" + next.RouteTag + "->" + next.OutboundTagInitial,
	}
	if got := e.callsSince(n); !slices.Equal(got, want) {
		t.Fatalf("calls %q, want %q", got, want)
	}
	if ev := e.logs.events(t, "candidate_replaced"); len(ev) != 1 || ev[0]["reasonCode"] != "failed" || ev[0]["fromTag"] != old.BaseTunnelID ||
		ev[0]["failuresInRow"] != 3.0 || ev[0]["probeError"] == "" {
		t.Fatalf("candidate_replaced %v", ev)
	}
	if q := e.source().Selection.Quarantine; len(q) != 1 {
		t.Fatalf("quarantine %v", q)
	}
	// The new tunnel starts a new monitor: no flap episode carried over.
	if f := e.tunnel(0).Monitor.Flap; !f.Since.IsZero() || len(f.Times) != 0 {
		t.Fatalf("flap %+v", f)
	}
	if p := outboundPeak([]string{old.OutboundWithoutPrefix.Tag, e.tunnel(1).Mapping.OutboundTagInitial}, e.xray.Calls()[n:]); p > 2 {
		t.Fatalf("peak %d", p)
	}
}

// No candidate outside quarantine: the tunnel stays in repair with the
// no_replacement warning, logged once; a successful check clears it.
func TestNoReplacement(t *testing.T) {
	e := wgEnv(t, 1, 1, 2)
	tn := e.tunnel(0).Mapping
	e.deps.Probe = failPort(tn.Port)
	for range 2 {
		if err := TunnelTick(t.Context(), e.deps, e.sub); err != nil {
			t.Fatal(err)
		}
	}
	if cur := e.tunnel(0).Mapping; cur.BaseTunnelID != tn.BaseTunnelID || cur.OutboundTagCurrent != tn.OutboundWithoutPrefix.Tag {
		t.Fatalf("tunnel %+v", cur)
	}
	if ev := e.logs.events(t, "candidate_no_replacement"); len(ev) != 1 || ev[0]["level"] != "WARN" {
		t.Fatalf("candidate_no_replacement %v", ev)
	}
	src := e.source()
	if w := src.AllWarnings(); len(w) != 1 || w[0].Code != state.CodeNoReplacement {
		t.Fatalf("warnings %v", w)
	}
	e.deps.Probe = ok(200, 10)
	if err := TunnelTick(t.Context(), e.deps, e.sub); err != nil {
		t.Fatal(err)
	}
	if src := e.source(); len(src.AllWarnings()) != 0 || e.tunnel(0).Mapping.OutboundTagCurrent != tn.OutboundTagInitial {
		t.Fatalf("warnings %v, tunnel %+v", src.AllWarnings(), e.tunnel(0).Mapping)
	}
}

// While the source waits for a full apply (its hashes are reset), a failed
// tunnel is repaired, not replaced: no new candidate, no quarantine.
func TestNoReplaceWhileHashesReset(t *testing.T) {
	for _, reset := range []bool{true, false} {
		e := wgEnv(t, 1, 1, 2, 4)
		old := e.tunnel(0).Mapping
		if reset {
			e.mem.Update(e.key, func(tg *state.Target) { tg.ResetSourceHashes("wg") })
		}
		e.deps.Probe = failPort(old.Port)
		n := len(e.xray.Calls())
		if err := TunnelTick(t.Context(), e.deps, e.sub); err != nil {
			t.Fatal(err)
		}
		replaced := len(e.logs.events(t, "candidate_replaced")) == 1
		if replaced == reset || (len(e.source().Selection.Quarantine) == 1) == reset {
			t.Errorf("reset=%v: replaced %v, quarantine %v", reset, replaced, e.source().Selection.Quarantine)
		}
		if !reset {
			continue
		}
		want := []string{
			"ListInbounds", "RemoveRule:" + old.RouteTag, "RemoveOutbound:" + old.OutboundTagInitial, "ListOutbounds",
			"AddOutbound:" + old.OutboundWithoutPrefix.Tag, "AddRule:" + old.RouteTag + "->" + old.OutboundWithoutPrefix.Tag,
		}
		if got := e.callsSince(n); !slices.Equal(got, want) {
			t.Errorf("calls %q, want %q", got, want)
		}
	}
}

// A failed replacement leaves the slot empty and resets the source hashes.
func TestReplaceFailedEmptiesSlot(t *testing.T) {
	e := wgEnv(t, 1, 1, 2, 4)
	e.deps.Probe = failPort(e.tunnel(0).Mapping.Port)
	e.xray.FailNext("AddInbound", errors.New("boom"))
	if err := TunnelTick(t.Context(), e.deps, e.sub); err != nil {
		t.Fatal(err)
	}
	if len(e.tunnels()) != 1 || len(e.source().Hashes) != 0 || len(e.logs.events(t, "candidate_replace_failed")) != 1 {
		t.Fatalf("tunnels %d, hashes %v", len(e.tunnels()), e.source().Hashes)
	}
}

// sources[].monitor overrides the target's timeoutMs, retry.attempts and
// retry.delayMs, for a vless source too; unset keys keep the target's.
func TestSourceMonitorOverride(t *testing.T) {
	for _, tc := range []struct {
		name              string
		override          *config.SourceMonitorConfig
		timeout           time.Duration
		attempts, delayMs int
	}{
		{"none", nil, 5 * time.Second, 1, 0},
		{"all", &config.SourceMonitorConfig{
			Request: &config.SourceMonitorRequestConfig{TimeoutMs: 12000},
			Retry:   &config.SourceRetryConfig{Attempts: 3, DelayMs: new(700)},
		}, 12 * time.Second, 3, 700},
		{"attempts only", &config.SourceMonitorConfig{Retry: &config.SourceRetryConfig{Attempts: 2}}, 5 * time.Second, 2, 250},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sub := newSub(func(c *config.SubscriptionConfig) { c.Sources[0].Monitor = tc.override })
			e := newEnv(t, sub, linkAT)
			var (
				mu       sync.Mutex
				timeouts []time.Duration
				delays   []time.Duration
			)
			e.deps.Probe = func(_ context.Context, r socks.Request) (socks.Response, error) {
				mu.Lock()
				defer mu.Unlock()
				timeouts = append(timeouts, r.Timeout)
				return socks.Response{StatusCode: 500}, nil
			}
			e.deps.Sleep = func(_ context.Context, d time.Duration) error {
				delays = append(delays, d)
				return nil
			}
			if err := TunnelTick(t.Context(), e.deps, e.sub); err != nil {
				t.Fatal(err)
			}
			if len(timeouts) != tc.attempts || timeouts[0] != tc.timeout {
				t.Fatalf("probes %v", timeouts)
			}
			if tc.attempts > 1 && delays[0] != time.Duration(tc.delayMs)*time.Millisecond {
				t.Fatalf("delays %v", delays)
			}
		})
	}
}

// A tunnel whose inbound is gone from Xray is not replaced: one
// ListInbounds, xray_state_lost, no candidate event and no quarantine.
func TestNoReplaceWithoutInbound(t *testing.T) {
	e := wgEnv(t, 1, 1, 2, 4)
	old := e.tunnel(0).Mapping
	if err := e.client.RemoveInbound(t.Context(), old.InboundTag); err != nil {
		t.Fatal(err)
	}
	e.deps.Probe = failPort(old.Port)
	n := len(e.xray.Calls())
	if err := TunnelTick(t.Context(), e.deps, e.sub); err != nil {
		t.Fatal(err)
	}
	if got := e.callsSince(n); !slices.Equal(got, []string{"ListInbounds"}) {
		t.Fatalf("calls %q", got)
	}
	for _, ev := range []string{"candidate_replaced", "candidate_replace_failed", "candidate_no_replacement"} {
		if len(e.logs.events(t, ev)) != 0 {
			t.Errorf("%s logged", ev)
		}
	}
	if q := e.source().Selection.Quarantine; len(q) != 0 || len(e.source().Hashes) != 0 || len(e.logs.events(t, "xray_state_lost")) != 1 {
		t.Fatalf("quarantine %v, hashes %v", q, e.source().Hashes)
	}
}

// While Xray is unavailable a failed replacement is debug next to
// xray_unavailable.
func TestReplaceFailedXrayUnavailable(t *testing.T) {
	e := wgEnv(t, 1, 1, 2, 4)
	e.deps.Probe = failPort(e.tunnel(0).Mapping.Port)
	e.xray.FailNext("AddInbound", status.Error(codes.Unavailable, "connection error"))
	if err := TunnelTick(t.Context(), e.deps, e.sub); err != nil {
		t.Fatal(err)
	}
	if got := e.logs.levels(t, "candidate_replace_failed"); !slices.Equal(got, []string{"DEBUG"}) {
		t.Fatalf("candidate_replace_failed %v", got)
	}
	if len(e.logs.events(t, "xray_unavailable")) != 1 {
		t.Fatal("no xray_unavailable")
	}
}

// A source without a live candidate is one episode: the first
// candidate_no_replacement is warn, then replacements, no_replacement and
// repairs of its tunnels are debug, until any of its tunnels passes a
// check (source_recovered).
func TestSourceWithoutLiveCandidate(t *testing.T) {
	e := wgEnv(t, 1, 1, 2, 4)
	e.sub.Target.Monitor.MaxParallel = 1
	c := newClock(e)
	for i := range 6 {
		e.tickAt(c, time.Duration(i)*3*time.Minute, failProbe) // the cooldown (4 min) ends between ticks
	}
	if got := e.logs.levels(t, "candidate_no_replacement"); len(got) < 2 || got[0] != "WARN" || slices.Contains(got[1:], "WARN") {
		t.Fatalf("candidate_no_replacement %v", got)
	}
	if got := e.logs.levels(t, "candidate_replaced"); len(got) < 2 || got[0] != "INFO" || slices.Contains(got[1:], "INFO") {
		t.Fatalf("candidate_replaced %v", got)
	}
	if got := e.logs.levels(t, "tunnel_repaired"); slices.Contains(got, "WARN") {
		t.Fatalf("tunnel_repaired %v", got)
	}
	e.tickAt(c, 20*time.Minute, ok(200, 10))
	e.tickAt(c, 21*time.Minute, ok(200, 10))
	ev := e.logs.events(t, "source_recovered")
	if len(ev) != 1 || ev[0]["level"] != "INFO" || ev[0]["sourceId"] != "wg" || ev[0]["failedForMs"] == 0.0 {
		t.Fatalf("source_recovered %v", ev)
	}
	if e.source().NoLiveSince != nil {
		t.Fatal("episode not ended")
	}
}

// One healthy and one dead tunnel are no episode: one warn
// candidate_no_replacement, no source_recovered. When the healthy one dies
// too the episode starts with its own warn and ends with one
// source_recovered.
func TestSourceEpisodeNeedsNoHealthyTunnel(t *testing.T) {
	e := wgEnv(t, 1, 1, 2)
	e.sub.Target.Monitor.MaxParallel = 1
	c := newClock(e)
	dead := e.tunnel(1).Mapping.Port
	for i := range 4 {
		e.tickAt(c, time.Duration(i)*time.Minute, failPort(dead))
	}
	if got := e.logs.levels(t, "candidate_no_replacement"); !slices.Equal(got, []string{"WARN"}) {
		t.Fatalf("candidate_no_replacement %v", got)
	}
	if ev := e.logs.events(t, "source_recovered"); len(ev) != 0 || e.source().NoLiveSince != nil {
		t.Fatalf("source_recovered %v", ev)
	}
	e.tickAt(c, 5*time.Minute, failProbe)
	e.tickAt(c, 6*time.Minute, failProbe)
	if got := e.logs.levels(t, "candidate_no_replacement"); !slices.Equal(got, []string{"WARN", "WARN"}) {
		t.Fatalf("candidate_no_replacement %v", got)
	}
	e.tickAt(c, 7*time.Minute, ok(200, 10))
	if ev := e.logs.events(t, "source_recovered"); len(ev) != 1 || ev[0]["level"] != "INFO" {
		t.Fatalf("source_recovered %v", ev)
	}
}
