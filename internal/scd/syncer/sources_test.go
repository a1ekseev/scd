package syncer

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/a1ekseev/scd/internal/scd/config"
	"github.com/a1ekseev/scd/internal/scd/state"
	"github.com/a1ekseev/scd/internal/scd/topology"
	"github.com/a1ekseev/scd/internal/scd/xrayapi"
	"github.com/a1ekseev/scd/internal/scd/xrayapi/xrayfake"
)

// twoSourceSub has sources "a" (/a, ports 20000..20010) and "b" (/b, ports
// 20100..20110) on addrA with SOCKS inbounds and the fixed resources of
// the fake below.
func twoSourceSub() config.SubscriptionConfig {
	t := target(addrA)
	t.InboundSocks = &config.InboundSocksConfig{Listen: "127.0.0.1"}
	t.FixedOutbounds, t.FixedInbounds, t.FixedRouting = []string{"direct"}, []string{"api"}, []string{"keep"}
	a, b := source("a", "/a"), source("b", "/b")
	a.PortRange = &config.PortRange{Start: 20000, End: 20010}
	b.PortRange = &config.PortRange{Start: 20100, End: 20110}
	return config.SubscriptionConfig{ID: "sub", Enabled: true, Sources: []config.SourceConfig{a, b}, Target: t}
}

func newSourcesFixture(t *testing.T, sub config.SubscriptionConfig, text map[string]string, opts ...xrayfake.Option) *fixture {
	t.Helper()
	f := newFixture(t, cfg(allKinds, sub), text)
	s, _ := xrayfake.Start(t, opts...)
	f.x.servers[addrA] = s
	return f
}

// touchesSource reports whether an RPC names a resource of source id.
func touchesSource(call, id string) bool {
	_, tag, _ := strings.Cut(call, ":")
	for _, p := range []string{"x-observe-" + id + "-", id + "-", "in-" + id + "-", "route-" + id + "-"} {
		if strings.HasPrefix(tag, p) {
			return true
		}
	}
	return false
}

func TestSourcesAppliedSeparately(t *testing.T) {
	f := newSourcesFixture(t, twoSourceSub(), map[string]string{"/a": vienna, "/b": berlin},
		xrayfake.WithStaticOutbounds("direct"), xrayfake.WithStaticInbounds("api"))
	r := f.run(t)
	if r.Failed != 0 || len(r.Targets) != 2 || r.Targets[0].SourceID != "a" || r.Targets[1].SourceID != "b" || r.Added != 6 {
		t.Fatalf("report %+v", r)
	}
	srv := f.x.servers[addrA]
	var ports []int
	f.s.Memory.View(state.Key("sub", addrA), func(tg *state.Target) {
		for _, tn := range tg.Tunnels {
			ports = append(ports, tn.Mapping.Port)
			if !strings.HasPrefix(tn.Mapping.BaseTunnelID, tn.Mapping.SourceID+"-") {
				t.Errorf("tunnel %s of source %s", tn.Mapping.BaseTunnelID, tn.Mapping.SourceID)
			}
		}
	})
	if !slices.Equal(ports, []int{20000, 20100}) {
		t.Errorf("ports %v", ports)
	}

	// Only source b changes: no RPC names a resource of source a.
	n := len(srv.Calls())
	f.in.set("/b", berlin+"\n"+vienna)
	r = f.run(t)
	if r.Failed != 0 || len(r.Targets[0].UnchangedKinds) != 3 || len(r.Targets[1].Resources) != 3 {
		t.Fatalf("second report %+v", r)
	}
	for _, call := range srv.Calls()[n:] {
		if touchesSource(call, "a") {
			t.Errorf("RPC on source a: %s", call)
		}
		if strings.HasPrefix(call, "Remove") && !touchesSource(call, "b") {
			t.Errorf("removal outside source b: %s", call)
		}
	}
	for _, e := range f.logs.events(t, "manifest_built") {
		if e["sourceId"] != "a" && e["sourceId"] != "b" || e["subscriptionId"] != "sub" {
			t.Errorf("manifest_built %v", e)
		}
	}
}

func TestSourceLoadFailureKeepsItsResources(t *testing.T) {
	f := newSourcesFixture(t, twoSourceSub(), map[string]string{"/a": vienna, "/b": berlin})
	f.run(t)
	srv := f.x.servers[addrA]
	before := srv.OutboundTags()
	n := len(srv.Calls())
	f.in.fail["/a"] = errors.New("boom")
	f.in.set("/b", berlin+"\n"+vienna)
	r := f.run(t)
	if r.Targets[0].SourceID != "a" || r.Targets[0].Failed != 3 || r.Targets[1].Failed != 0 {
		t.Fatalf("report %+v", r.Targets)
	}
	for _, call := range srv.Calls()[n:] {
		if touchesSource(call, "a") {
			t.Errorf("RPC on the failed source: %s", call)
		}
	}
	if got := srv.OutboundTags(); len(got) != len(before)+1 || !slices.Contains(got, before[0]) {
		t.Errorf("outbounds %v, before %v", got, before)
	}
	if e := f.logs.events(t, "subscription_fetch_failed"); len(e) != 1 || e[0]["sourceId"] != "a" {
		t.Errorf("subscription_fetch_failed %v", e)
	}
}

// After the update, tags without the source id (here of an old source id
// "vless") and resources of a disabled source are orphans: the first sync
// removes them, rule → inbound → outbound, before any source is applied.
func TestFirstSyncRemovesOrphans(t *testing.T) {
	sub := twoSourceSub()
	sub.Sources[1].Enabled = false
	f := newSourcesFixture(t, sub, map[string]string{"/a": vienna},
		xrayfake.WithStaticOutbounds("direct", "x-observe-vless-at-1", "vless-de-1", "x-observe-b-1"),
		xrayfake.WithStaticInbounds("api", "in-vless-at-1", "in-b-1"),
		xrayfake.WithStaticRules(
			xrayapi.Rule{RuleTag: "keep", Tag: "direct"}, xrayapi.Rule{RuleTag: "", Tag: "direct"},
			xrayapi.Rule{RuleTag: "route-vless-at-1", Tag: "x-observe-vless-at-1"}, xrayapi.Rule{RuleTag: "route-b-1", Tag: "x-observe-b-1"},
		))
	key := state.Key("sub", addrA)
	f.s.Memory.ReplaceSourceTopology(key, "b", topologyOf("b-1"), nil)

	r := f.run(t)
	if r.Failed != 0 || len(r.Targets) != 1 || r.Targets[0].SourceID != "a" {
		t.Fatalf("report %+v", r)
	}
	srv := f.x.servers[addrA]
	var order []string
	for _, call := range srv.Calls() {
		if call == "RemoveRule:" {
			t.Error("RemoveRule with an empty tag")
		}
		if strings.HasPrefix(call, "Remove") && !touchesSource(call, "a") {
			order = append(order, call)
		}
		if strings.HasPrefix(call, "Add") {
			break
		}
	}
	if !slices.IsSortedFunc(order, func(x, y string) int { return strings.Compare(kindOf(x), kindOf(y)) }) {
		t.Errorf("orphans not removed rule → inbound → outbound: %v", order)
	}
	if len(order) != 7 || kindOf(order[0]) != "1rule" || kindOf(order[6]) != "3outbound" {
		t.Fatalf("orphan removals %v", order)
	}
	for i, call := range srv.Calls() {
		if strings.HasPrefix(call, "Add") {
			if i < 7 {
				t.Errorf("source applied before the orphans were removed: %v", srv.Calls())
			}
			break
		}
	}
	if len(srv.Rules()) != 3 { // keep, the rule without ruleTag, route-a-…
		t.Errorf("rules %v", srv.Rules())
	}
	if e := f.logs.events(t, "orphans_removed"); len(e) != 1 || len(e[0]["orphans"].([]any)) != 7 {
		t.Errorf("orphans_removed %v", e)
	}
	f.s.Memory.View(key, func(tg *state.Target) {
		if tg.Sources["b"] != nil || len(tg.Tunnels) != 1 || tg.Tunnels[0].Mapping.SourceID != "a" {
			t.Errorf("state of the disabled source kept: %+v", tg.Tunnels)
		}
	})
	// The next sync finds no orphans.
	r = f.run(t)
	if r.Failed != 0 || len(f.logs.events(t, "orphans_removed")) != 1 {
		t.Errorf("second sync %+v", r)
	}
}

// kindOf sorts the removals by kind: rule, inbound, outbound.
func kindOf(call string) string {
	switch {
	case strings.HasPrefix(call, "RemoveRule"):
		return "1rule"
	case strings.HasPrefix(call, "RemoveInbound"):
		return "2inbound"
	}
	return "3outbound"
}

func TestOrphanRemovalFailureStopsTarget(t *testing.T) {
	f := newSourcesFixture(t, twoSourceSub(), map[string]string{"/a": vienna, "/b": berlin},
		xrayfake.WithStaticOutbounds("direct", "old-1"))
	srv := f.x.servers[addrA]
	srv.FailNext("RemoveOutbound", status.Error(codes.Unknown, "boom"))
	r := f.run(t)
	if len(r.Targets) != 2 || r.Failed != 6 {
		t.Fatalf("report %+v", r)
	}
	for _, tr := range r.Targets {
		for _, res := range tr.Resources {
			if res.Items[0].Message != "orphan removal: boom" {
				t.Errorf("%s %s: %+v", tr.SourceID, res.Kind, res.Items)
			}
		}
	}
	if mutations(srv.Calls()) != 1 {
		t.Errorf("calls after the failure: %v", srv.Calls())
	}
	if e := f.logs.events(t, "orphan_remove_failed"); len(e) != 1 || e[0]["error"] != "boom" {
		t.Errorf("orphan_remove_failed %v", e)
	}
	// The next sync retries and applies.
	if r := f.run(t); r.Failed != 0 || r.Added != 6 {
		t.Errorf("retry %+v", r)
	}
	if slices.Contains(srv.OutboundTags(), "old-1") {
		t.Error("orphan kept")
	}
}

func TestReportsCarrySourceID(t *testing.T) {
	f := newSourcesFixture(t, twoSourceSub(), map[string]string{"/a": vienna, "/b": berlin})
	f.run(t)
	for _, ev := range []string{"apply_finished", "resource_apply_started", "manifest_built"} {
		for _, e := range f.logs.events(t, ev) {
			if e["sourceId"] != "a" && e["sourceId"] != "b" {
				t.Errorf("%s sourceId %v", ev, e["sourceId"])
			}
		}
	}
	// sourceCount keeps its meaning (subscriptions); enabledSourceCount is new.
	if e := f.logs.events(t, "sync_started"); len(e) != 1 || e[0]["sourceCount"] != float64(1) || e[0]["enabledSourceCount"] != float64(2) {
		t.Errorf("sync_started %v", e)
	}
	if e := f.logs.events(t, "subscription_fetched"); len(e) != 1 || len(e[0]["subscriptions"].([]any)) != 2 {
		t.Errorf("subscription_fetched %v", e)
	}
}

// topologyOf is a topology with one tunnel per tag.
func topologyOf(tags ...string) topology.Topology {
	var t topology.Topology
	for _, tag := range tags {
		t.Tunnels = append(t.Tunnels, topology.Tunnel{BaseTunnelID: tag, InboundTag: "in-" + tag, RouteTag: "route-" + tag})
	}
	return t
}
