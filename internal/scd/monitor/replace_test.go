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

	"github.com/a1ekseev/scd/internal/scd/apply"
	"github.com/a1ekseev/scd/internal/scd/config"
	"github.com/a1ekseev/scd/internal/scd/manifest"
	"github.com/a1ekseev/scd/internal/scd/resources"
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
		in, err := resources.Inbound(tn)
		if err != nil {
			t.Fatal(err)
		}
		if err := e.client.AddInbound(t.Context(), in); err != nil {
			t.Fatal(err)
		}
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
		"RemoveRule:" + old.RouteTag, "RemoveOutbound:" + old.OutboundWithoutPrefix.Tag, "RemoveOutbound:" + old.OutboundTagInitial,
		"RemoveInbound:" + old.InboundTag, "ListOutbounds", "AddOutbound:" + next.OutboundTagInitial, "AddInbound:" + next.InboundTag, "AddRule:" + next.RouteTag + "->" + next.OutboundTagInitial,
	}
	if got := e.callsSince(n); !slices.Equal(got, want) {
		t.Fatalf("calls %q, want %q", got, want)
	}
	if ev := e.logs.events(t, "candidate_replaced"); len(ev) != 1 || ev[0]["reasonCode"] != "failed" || ev[0]["fromTag"] != old.BaseTunnelID {
		t.Fatalf("candidate_replaced %v", ev)
	}
	if q := e.source().Selection.Quarantine; len(q) != 1 {
		t.Fatalf("quarantine %v", q)
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
