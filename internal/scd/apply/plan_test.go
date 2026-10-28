package apply_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/a1ekseev/scd/internal/scd/apply"
	"github.com/a1ekseev/scd/internal/scd/config"
	"github.com/a1ekseev/scd/internal/scd/manifest"
	"github.com/a1ekseev/scd/internal/scd/normalize"
	"github.com/a1ekseev/scd/internal/scd/outbound"
	"github.com/a1ekseev/scd/internal/scd/topology"
	"github.com/a1ekseev/scd/internal/scd/xrayapi"
	"github.com/a1ekseev/scd/internal/scd/xrayapi/xrayfake"
)

const (
	vienna  = "vless://7d1b6590-1069-4372-92be-8d0a0ae6eaf5@example.com:443?type=tcp&security=tls#🇦🇹 Вена, Австрия"
	berlin  = "vless://7d1b6590-1069-4372-92be-8d0a0ae6eaf6@example.com:443?type=tcp&security=tls#🇩🇪 Берлин, Германия"
	reality = "vless://7d1b6590-1069-4372-92be-8d0a0ae6eaf7@example.com:443?security=reality&encryption=none&fp=chrome&type=tcp&flow=xtls-rprx-vision&sni=io.example.test&pbk=OHWtGCfZc0FXULj2ZYeW_vlKIlurak79i6AXun9oJQg&sid=cbad7f159438fee4#🇳🇱 Амстердам, Нидерланды"
	trojan  = "trojan://secret@example.com:443#unsupported"
)

func buildManifest(t *testing.T, lines ...string) manifest.Manifest {
	t.Helper()
	m := manifest.Build(strings.Join(lines, "\n"), "inline", normalize.Policy{TagPrefix: "vless"}, time.Unix(0, 0))
	if len(m.Entries) == 0 {
		t.Fatalf("no entries, skipped = %+v", m.Skipped)
	}
	return m
}

func targetConfig(mut func(*config.SubscriptionTargetConfig)) config.SubscriptionTargetConfig {
	tc := config.SubscriptionTargetConfig{Address: target, TimeoutMs: 5000}
	if mut != nil {
		mut(&tc)
	}
	return tc
}

func withSocks(tc *config.SubscriptionTargetConfig) {
	tc.InboundSocks = &config.InboundSocksConfig{Listen: "127.0.0.1"}
}

func withPrefix(tc *config.SubscriptionTargetConfig) {
	tc.ObservatorySubjectSelectorPrefix = "x-observe-"
}

// source is the source the test manifests come from (tag prefix "vless").
var source = config.SourceConfig{ID: "vless", Type: "vless", PortRange: &config.PortRange{Start: 20000, End: 20010}}

func mustPlan(t *testing.T, kind apply.Kind, m manifest.Manifest, tc config.SubscriptionTargetConfig) apply.Plan {
	t.Helper()
	p, err := apply.PlanForSource(kind, m, source, tc)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func sha(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func outboundTags(list []outbound.Outbound) []string {
	out := make([]string, 0, len(list))
	for _, o := range list {
		out = append(out, o.Tag)
	}
	return out
}

// S16 (service.test.ts:868).
func TestPlanOutboundStableForReorderedLines(t *testing.T) {
	first := mustPlan(t, apply.KindOutbound, buildManifest(t, vienna, berlin), targetConfig(nil))
	second := mustPlan(t, apply.KindOutbound, buildManifest(t, berlin, vienna), targetConfig(nil))

	if first.ManifestHash != second.ManifestHash {
		t.Errorf("hash differs: %s vs %s", first.ManifestHash, second.ManifestHash)
	}
	if !slices.Equal(first.ManagedIDs, second.ManagedIDs) {
		t.Errorf("managedIds differ: %v vs %v", first.ManagedIDs, second.ManagedIDs)
	}
	if len(first.ManagedIDs) != 2 || !slices.IsSorted(first.ManagedIDs) {
		t.Errorf("managedIds not sorted: %v", first.ManagedIDs)
	}
	if got := outboundTags(first.Outbounds); !slices.Equal(got, first.ManagedIDs) {
		t.Errorf("outbound tags = %v, managedIds = %v", got, first.ManagedIDs)
	}
	if first.ManifestHash != outbound.Hash(first.Outbounds) {
		t.Error("hash is not outbound.Hash of the outbounds")
	}
}

// S17 (service.test.ts:903).
func TestPlanOutboundAppliesObservatoryPrefix(t *testing.T) {
	m := buildManifest(t, vienna)
	base := mustPlan(t, apply.KindOutbound, m, targetConfig(nil))
	prefixed := mustPlan(t, apply.KindOutbound, m, targetConfig(func(tc *config.SubscriptionTargetConfig) {
		tc.FixedOutbounds = []string{"direct", "blocked"}
		withPrefix(tc)
	}))

	if base.ManifestHash == prefixed.ManifestHash {
		t.Error("prefix does not change the hash")
	}
	if !strings.HasPrefix(prefixed.ManagedIDs[0], "x-observe-") || !strings.HasPrefix(prefixed.Outbounds[0].Tag, "x-observe-") {
		t.Errorf("managedIds = %v, tag = %q", prefixed.ManagedIDs, prefixed.Outbounds[0].Tag)
	}
	if prefixed.Outbounds[0].Settings.Vnext[0].Users[0].Email != prefixed.Outbounds[0].Tag {
		t.Errorf("outbound is not built for the prefixed tag: %+v", prefixed.Outbounds[0])
	}
}

func TestPlanOutboundFromTopology(t *testing.T) {
	m := buildManifest(t, berlin, vienna, reality)
	override := func(tc *config.SubscriptionTargetConfig) { withPrefix(tc); tc.VisionUDP443Override = true }
	plain := mustPlan(t, apply.KindOutbound, m, targetConfig(override))
	socks := mustPlan(t, apply.KindOutbound, m, targetConfig(func(tc *config.SubscriptionTargetConfig) { override(tc); withSocks(tc) }))

	if socks.Topology == nil || len(socks.Topology.Tunnels) != 3 {
		t.Fatalf("topology = %+v", socks.Topology)
	}
	for i, tn := range socks.Topology.Tunnels {
		if !reflect.DeepEqual(socks.Outbounds[i], tn.OutboundInitial.JSON) {
			t.Errorf("outbound %d is not the tunnel's initial outbound", i)
		}
	}
	if !reflect.DeepEqual(plain.Outbounds, socks.Outbounds) || plain.ManifestHash != socks.ManifestHash {
		t.Error("topology and plain outbounds differ")
	}
	flows := []string{}
	for _, o := range socks.Outbounds {
		flows = append(flows, o.Settings.Vnext[0].Users[0].Flow)
	}
	if !slices.Contains(flows, "xtls-rprx-vision-udp443") {
		t.Errorf("flows = %v, want the udp443 override", flows)
	}
	if plain.Topology == nil || len(plain.Topology.Tunnels) != 0 {
		t.Errorf("plain topology = %+v, want empty", plain.Topology)
	}
}

func TestPlanInboundHash(t *testing.T) {
	m := buildManifest(t, vienna, berlin)
	p := mustPlan(t, apply.KindInbound, m, targetConfig(withSocks))

	tns := p.Topology.Tunnels
	want := `[{"inboundTag":"` + tns[0].InboundTag + `","listen":"127.0.0.1","port":20000},` +
		`{"inboundTag":"` + tns[1].InboundTag + `","listen":"127.0.0.1","port":20001}]`
	if p.ManifestHash != sha(want) {
		t.Errorf("hash = %s, want sha256(%s)", p.ManifestHash, want)
	}
	if !slices.Equal(p.ManagedIDs, []string{tns[0].InboundTag, tns[1].InboundTag}) {
		t.Errorf("managedIds = %v", p.ManagedIDs)
	}
	if p.Outbounds != nil {
		t.Errorf("inbound plan has outbounds")
	}

	empty := mustPlan(t, apply.KindInbound, m, targetConfig(nil))
	if empty.ManifestHash != sha("[]") || empty.ManagedIDs == nil || len(empty.ManagedIDs) != 0 {
		t.Errorf("empty plan: hash = %s, managedIds = %#v", empty.ManifestHash, empty.ManagedIDs)
	}
}

func TestPlanRoutingUsesInitialTag(t *testing.T) {
	m := buildManifest(t, vienna)
	p := mustPlan(t, apply.KindRouting, m, targetConfig(func(tc *config.SubscriptionTargetConfig) { withSocks(tc); withPrefix(tc) }))

	tn := p.Topology.Tunnels[0]
	want := `[{"routeTag":"route-` + tn.BaseTunnelID + `","inboundTag":"in-` + tn.BaseTunnelID +
		`","outboundTag":"x-observe-` + tn.BaseTunnelID + `"}]`
	if p.ManifestHash != sha(want) {
		t.Errorf("hash = %s, want sha256(%s)", p.ManifestHash, want)
	}
	if !slices.Equal(p.ManagedIDs, []string{"route-" + tn.BaseTunnelID}) {
		t.Errorf("managedIds = %v", p.ManagedIDs)
	}
}

func TestPlanSkipsEntriesBeyondPortRange(t *testing.T) {
	m := buildManifest(t, vienna, berlin)
	tc := targetConfig(withSocks)
	small := source
	small.PortRange = &config.PortRange{Start: 20000, End: 20000}
	for _, kind := range apply.Order {
		t.Run(string(kind), func(t *testing.T) {
			p, err := apply.PlanForSource(kind, m, small, tc)
			if err != nil {
				t.Fatal(err)
			}
			if len(p.ManagedIDs) != 1 || len(p.Topology.Tunnels) != 1 {
				t.Errorf("managedIds = %v", p.ManagedIDs)
			}
			if len(p.Skipped) != 1 || p.Skipped[0].Code != topology.CodePortRangeExhausted {
				t.Errorf("skipped = %+v", p.Skipped)
			}
			if p.Summary.PortRangeExhausted != 1 || p.Summary.Skipped != 1 || m.Summary.Skipped != 0 {
				t.Errorf("summary = %+v, manifest summary = %+v", *p.Summary, m.Summary)
			}
		})
	}
	// Capacity 0: no resource of any kind, not even an outbound.
	small.PortRange = &config.PortRange{Start: 20000, End: 19999}
	p, err := apply.PlanForSource(apply.KindOutbound, m, small, tc)
	if err != nil || len(p.Outbounds) != 0 || len(p.Skipped) != 2 {
		t.Errorf("outbounds = %d, skipped = %d, err = %v", len(p.Outbounds), len(p.Skipped), err)
	}
}

func TestPlanCarriesManifestMeta(t *testing.T) {
	m := buildManifest(t, vienna, trojan)
	if len(m.Skipped) != 1 {
		t.Fatalf("skipped = %+v", m.Skipped)
	}
	for _, kind := range apply.Order {
		p := mustPlan(t, kind, m, targetConfig(nil))
		if p.Kind != kind || p.SourceID != "vless" {
			t.Errorf("%s: kind = %q, sourceId = %q", kind, p.Kind, p.SourceID)
		}
		if !reflect.DeepEqual(p.Skipped, m.Skipped) {
			t.Errorf("%s: skipped = %+v", kind, p.Skipped)
		}
		if p.Summary == nil || *p.Summary != m.Summary {
			t.Errorf("%s: summary = %+v", kind, p.Summary)
		}
	}
}

func TestPlanUnknownKind(t *testing.T) {
	if _, err := apply.PlanForSource("balancer", buildManifest(t, vienna), source, targetConfig(nil)); err == nil {
		t.Error("no error for an unknown kind")
	}
}

func TestApplyAllKindsAgainstFake(t *testing.T) {
	srv, c := xrayfake.Start(t,
		xrayfake.WithStaticOutbounds("direct"),
		xrayfake.WithStaticInbounds("api"),
		xrayfake.WithStaticRules(xrayapi.Rule{Tag: "direct", RuleTag: "keep"}),
	)
	m := buildManifest(t, vienna, berlin, trojan)
	tc := targetConfig(func(tc *config.SubscriptionTargetConfig) {
		withSocks(tc)
		withPrefix(tc)
		tc.FixedOutbounds = []string{"direct"}
		tc.FixedInbounds = []string{"api"}
		tc.FixedRouting = []string{"keep"}
	})
	mt := apply.Meta{SubscriptionID: "sub-1", Now: func() time.Time { return fixedNow }}

	var plans []apply.Plan
	for _, kind := range apply.Order {
		p := mustPlan(t, kind, m, tc)
		plans = append(plans, p)
		r := apply.Apply(context.Background(), c, p, tc, mt)
		if r.Failed != 0 {
			t.Fatalf("%s failed: %+v", kind, r.Items)
		}
		if r.Kind != kind || r.SourceID != "vless" || r.SubscriptionID != "sub-1" || r.TargetAddress != target {
			t.Errorf("%s header = %+v", kind, r)
		}
		if !slices.Equal(r.AppliedIDs, p.ManagedIDs) {
			t.Errorf("%s appliedIds = %v, managedIds = %v", kind, r.AppliedIDs, p.ManagedIDs)
		}
		// TS: only the outbound report carries manifest.skipped
		// (apply-outbounds.ts buildBaseReport); inbound/routing carry [].
		wantSkipped := 0
		if kind == apply.KindOutbound {
			wantSkipped = 1
		}
		if r.Skipped == nil || len(r.Skipped) != wantSkipped {
			t.Errorf("%s skipped = %#v, want %d", kind, r.Skipped, wantSkipped)
		}
	}
	if got, want := srv.OutboundTags(), append([]string{"direct"}, plans[0].ManagedIDs...); !slices.Equal(got, want) {
		t.Errorf("outbounds = %v, want %v", got, want)
	}
	if got, want := srv.InboundTags(), append([]string{"api"}, plans[1].ManagedIDs...); !slices.Equal(got, want) {
		t.Errorf("inbounds = %v, want %v", got, want)
	}
	rules := srv.Rules()
	if len(rules) != 3 || rules[0].RuleTag != "keep" {
		t.Fatalf("rules = %v", rules)
	}
	for i, tn := range plans[2].Topology.Tunnels {
		if rules[i+1] != (xrayapi.Rule{Tag: tn.OutboundTagInitial, RuleTag: tn.RouteTag}) {
			t.Errorf("rule %d = %v", i+1, rules[i+1])
		}
	}
}

func TestApplyOutboundConflictCarriesSkipped(t *testing.T) {
	_, c := xrayfake.Start(t)
	m := buildManifest(t, vienna, trojan)
	p := mustPlan(t, apply.KindOutbound, m, targetConfig(nil))
	tc := targetConfig(func(tc *config.SubscriptionTargetConfig) { tc.FixedOutbounds = p.ManagedIDs })

	r := apply.Apply(context.Background(), c, p, tc, meta())
	if r.Failed != 1 || len(r.Skipped) != 1 {
		t.Errorf("failed = %d, skipped = %+v", r.Failed, r.Skipped)
	}
}

func TestApplyErrorBecomesFailedReport(t *testing.T) {
	m := buildManifest(t, vienna, trojan)
	tc := targetConfig(withSocks)
	methods := map[apply.Kind]string{
		apply.KindOutbound: "ListOutbounds",
		apply.KindInbound:  "ListInbounds",
		apply.KindRouting:  "ListRule",
	}
	for _, kind := range apply.Order {
		t.Run(string(kind), func(t *testing.T) {
			srv, c := xrayfake.Start(t)
			srv.FailNext(methods[kind], status.Error(codes.Unknown, "boom"))
			p := mustPlan(t, kind, m, tc)

			r := apply.Apply(context.Background(), c, p, tc, meta())
			want := []apply.Item{{ID: string(kind), Status: "failed", Message: "boom"}}
			if !slices.Equal(r.Items, want) || r.Failed != 1 || r.DurationMs != 0 {
				t.Errorf("report = %+v", r)
			}
			if !reflect.DeepEqual(r.Skipped, p.Skipped) {
				t.Errorf("skipped = %+v, want the plan's", r.Skipped)
			}
		})
	}
}
