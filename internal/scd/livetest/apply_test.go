//go:build xraylive

package livetest

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"net"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/xtls/xray-core/core"

	"github.com/a1ekseev/scd/internal/scd/apply"
	"github.com/a1ekseev/scd/internal/scd/config"
	"github.com/a1ekseev/scd/internal/scd/manifest"
	"github.com/a1ekseev/scd/internal/scd/monitor"
	"github.com/a1ekseev/scd/internal/scd/normalize"
	"github.com/a1ekseev/scd/internal/scd/state"
	"github.com/a1ekseev/scd/internal/scd/xrayapi"
)

// The fixed resources of the testdata/xray templates.
var (
	fixedOutbounds = []string{"direct", "blocked"}
	fixedInbounds  = []string{"socks-proxy"}
	fixedRouting   = []string{"x-balancer"}
	fixedRule      = xrayapi.Rule{Tag: "", RuleTag: "x-balancer"}
)

// freePortRange returns the first port of n consecutive TCP ports on
// 127.0.0.1 that were free a moment ago.
func freePortRange(t *testing.T, n int) int {
	t.Helper()
	lc := &net.ListenConfig{}
	for range 50 {
		start := 20000 + rand.IntN(40000)
		var ls []net.Listener
		for p := start; p < start+n; p++ {
			l, err := lc.Listen(t.Context(), "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(p)))
			if err != nil {
				break
			}
			ls = append(ls, l)
		}
		for _, l := range ls {
			l.Close()
		}
		if len(ls) == n {
			return start
		}
	}
	t.Fatalf("no %d consecutive free ports", n)
	return 0
}

// liveTarget is the target config of the live Xray and its source "vless":
// the template's fixed resources and per-tunnel SOCKS inbounds on free
// ports. No observatory prefix, so the observatory never probes the
// fixture's (unreachable) hosts.
func liveTarget(t *testing.T, tunnels int) (config.SubscriptionTargetConfig, config.SourceConfig) {
	t.Helper()
	start := freePortRange(t, tunnels)
	src := config.SourceConfig{ID: "vless", Type: "vless", Enabled: true, PortRange: &config.PortRange{Start: start, End: start + tunnels - 1}}
	return config.SubscriptionTargetConfig{
		Address:        "live-xray",
		TimeoutMs:      int(rpcTimeout.Milliseconds()),
		FixedOutbounds: fixedOutbounds,
		FixedInbounds:  fixedInbounds,
		FixedRouting:   fixedRouting,
		InboundSocks:   &config.InboundSocksConfig{Listen: "127.0.0.1"},
	}, src
}

// fixtureManifest builds the manifest of docker/fixtures/subscription.txt.
func fixtureManifest(t *testing.T) manifest.Manifest {
	t.Helper()
	data, err := os.ReadFile(fixturesDir + "subscription.txt")
	if err != nil {
		t.Fatal(err)
	}
	m := manifest.Build(strings.TrimSpace(string(data)), "subscription.txt", normalize.Policy{AllowInsecure: true, TagPrefix: "vless"}, time.Now())
	if len(m.Entries) != 8 || len(m.Skipped) != 0 {
		t.Fatalf("subscription.txt: %d entries, skipped %+v; want 8 entries", len(m.Entries), m.Skipped)
	}
	return m
}

// sortedUnion returns the sorted concatenation of lists.
func sortedUnion(lists ...[]string) []string {
	return slices.Sorted(slices.Values(slices.Concat(lists...)))
}

// liveState is what live Xray lists after an apply.
type liveState struct {
	outbounds, inbounds []string
	rules               []xrayapi.Rule
}

func readState(t *testing.T, c xrayapi.Client) liveState {
	t.Helper()
	ctx := t.Context()
	var s liveState
	outs, err := c.ListOutbounds(ctx)
	if err != nil {
		t.Fatalf("ListOutbounds: %v", err)
	}
	for _, h := range outs {
		s.outbounds = append(s.outbounds, h.Tag)
	}
	ins, err := c.ListInbounds(ctx)
	if err != nil {
		t.Fatalf("ListInbounds: %v", err)
	}
	for _, h := range ins {
		s.inbounds = append(s.inbounds, h.Tag)
	}
	slices.Sort(s.outbounds)
	slices.Sort(s.inbounds)
	if s.rules, err = c.ListRules(ctx); err != nil {
		t.Fatalf("ListRules: %v", err)
	}
	return s
}

// TestLiveApplyReplacesManagedKeepsFixed (D2, 02 §2): the full apply
// (outbound → inbound → routing) against live Xray, run twice: the fixed
// direct, blocked, socks-proxy and ruleTag x-balancer stay, the managed
// resources are exactly the plan's, added the first time and replaced the
// second.
func TestLiveApplyReplacesManagedKeepsFixed(t *testing.T) {
	c, _, _ := startXray(t)
	m := fixtureManifest(t)
	tc, src := liveTarget(t, len(m.Entries))
	meta := apply.Meta{SubscriptionID: "live"}

	plans := map[apply.Kind]apply.Plan{}
	for _, kind := range apply.Order {
		p, err := apply.PlanForSource(kind, m, src, tc)
		if err != nil {
			t.Fatal(err)
		}
		plans[kind] = p
	}

	for run, wantStatus := range []string{apply.StatusAdded, apply.StatusReplaced} {
		t.Run("run-"+strconv.Itoa(run+1), func(t *testing.T) {
			for _, kind := range apply.Order {
				p := plans[kind]
				r := apply.Apply(t.Context(), c, p, tc, meta)
				if r.Failed != 0 {
					t.Fatalf("%s failed: %+v", kind, r.Items)
				}
				if !slices.Equal(r.AppliedIDs, p.ManagedIDs) {
					t.Errorf("%s appliedIds = %v, plan %v", kind, r.AppliedIDs, p.ManagedIDs)
				}
				var wantRemoved []string
				if run > 0 {
					wantRemoved = slices.Sorted(slices.Values(p.ManagedIDs))
				}
				if got := slices.Sorted(slices.Values(r.DeletedIDs)); !slices.Equal(got, wantRemoved) {
					t.Errorf("%s deletedIds = %v, want %v (never a fixed one)", kind, got, wantRemoved)
				}
				applied := 0
				for _, it := range r.Items {
					switch it.Status {
					case wantStatus:
						applied++
					case apply.StatusRemoved:
					default:
						t.Errorf("%s item %+v, want status %s", kind, it, wantStatus)
					}
				}
				if applied != len(p.ManagedIDs) {
					t.Errorf("%s has %d %s items, want %d", kind, applied, wantStatus, len(p.ManagedIDs))
				}
			}

			s := readState(t, c)
			if want := sortedUnion(fixedOutbounds, plans[apply.KindOutbound].ManagedIDs); !slices.Equal(s.outbounds, want) {
				t.Errorf("outbounds = %v, want %v", s.outbounds, want)
			}
			if want := sortedUnion(fixedInbounds, plans[apply.KindInbound].ManagedIDs); !slices.Equal(s.inbounds, want) {
				t.Errorf("inbounds = %v, want %v", s.inbounds, want)
			}
			wantRules := []xrayapi.Rule{fixedRule}
			for _, tn := range plans[apply.KindRouting].Topology.Tunnels {
				wantRules = append(wantRules, xrayapi.Rule{Tag: tn.OutboundTagInitial, RuleTag: tn.RouteTag})
			}
			if !slices.Equal(s.rules, wantRules) {
				t.Errorf("rules = %v, want %v", s.rules, wantRules)
			}
		})
	}
}

// sharedClient hands the live client to the monitor, which closes every
// client it dials, and fails the test if an outbound of the tunnel is added
// while another copy of it is still in Xray.
type sharedClient struct {
	xrayapi.Client
	t    *testing.T
	tags []string // the tunnel's prefixed and unprefixed outbound tags
}

func (c sharedClient) Close() error { return nil }

func (c sharedClient) AddOutbound(ctx context.Context, h *core.OutboundHandlerConfig) error {
	outs, err := c.ListOutbounds(ctx)
	if err != nil {
		return err
	}
	for _, o := range outs {
		if slices.Contains(c.tags, o.Tag) {
			c.t.Errorf("AddOutbound(%s) while %s is in Xray", h.Tag, o.Tag)
		}
	}
	return c.Client.AddOutbound(ctx, h)
}

// TestLiveRepairRejoinRemovesBeforeAdd: repair and rejoin against live Xray
// replace the tunnel's outbound remove before add, so Xray never holds both
// copies, and end with exactly one outbound and one rule for the tunnel. The
// prefix is not the template's observatory selector, so the observatory
// never probes the fixture's (unreachable) hosts.
func TestLiveRepairRejoinRemovesBeforeAdd(t *testing.T) {
	c, _, _ := startXray(t)
	m := fixtureManifest(t)
	tc, src := liveTarget(t, len(m.Entries))
	tc.ObservatorySubjectSelectorPrefix = "x-live-"
	sub := config.SubscriptionConfig{ID: "live", Enabled: true, Sources: []config.SourceConfig{src}, Target: tc}

	var plan apply.Plan
	for _, kind := range apply.Order {
		p, err := apply.PlanForSource(kind, m, src, tc)
		if err != nil {
			t.Fatal(err)
		}
		if r := apply.Apply(t.Context(), c, p, tc, apply.Meta{SubscriptionID: sub.ID}); r.Failed != 0 {
			t.Fatalf("%s failed: %+v", kind, r.Items)
		}
		plan = p
	}
	mem := state.New()
	key := state.Key(sub.ID, tc.Address)
	mem.ReplaceSourceTopology(key, src.ID, *plan.Topology, nil)
	tn := plan.Topology.Tunnels[0]
	unprefixed := tn.OutboundWithoutPrefix.Tag
	if tn.OutboundTagInitial == unprefixed {
		t.Fatalf("tunnel %s has no prefix", tn.BaseTunnelID)
	}
	tags := []string{tn.OutboundTagInitial, unprefixed}
	d := monitor.Deps{
		Memory: mem,
		Log:    slog.New(slog.DiscardHandler),
		Dial: func(string, time.Duration) (xrayapi.Client, error) {
			return sharedClient{Client: c, t: t, tags: tags}, nil
		},
	}

	for _, step := range []struct {
		name string
		run  func(context.Context, monitor.Deps, config.SubscriptionConfig, string) error
		want string
	}{
		{"repair", monitor.Repair, unprefixed},
		{"rejoin", monitor.Rejoin, tn.OutboundTagInitial},
	} {
		if err := step.run(t.Context(), d, sub, tn.BaseTunnelID); err != nil {
			t.Fatalf("%s: %v", step.name, err)
		}
		var cur state.Tunnel
		mem.View(key, func(tg *state.Target) {
			for _, x := range tg.Tunnels {
				if x.Mapping.BaseTunnelID == tn.BaseTunnelID {
					cur = *x
				}
			}
		})
		if cur.Mapping.OutboundTagCurrent != step.want {
			t.Errorf("%s: tunnel = %q / %+v", step.name, cur.Mapping.OutboundTagCurrent, cur.Monitor)
		}
		s := readState(t, c)
		var got []string
		for _, tag := range s.outbounds {
			if slices.Contains(tags, tag) {
				got = append(got, tag)
			}
		}
		if !slices.Equal(got, []string{step.want}) {
			t.Errorf("%s: tunnel outbounds = %v, want [%s]", step.name, got, step.want)
		}
		var rules []xrayapi.Rule
		for _, r := range s.rules {
			if r.RuleTag == tn.RouteTag {
				rules = append(rules, r)
			}
		}
		if want := []xrayapi.Rule{{Tag: step.want, RuleTag: tn.RouteTag}}; !slices.Equal(rules, want) {
			t.Errorf("%s: tunnel rules = %v, want %v", step.name, rules, want)
		}
	}
}
