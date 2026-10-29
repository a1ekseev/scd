package topology

import (
	"strings"
	"testing"
	"time"

	"github.com/a1ekseev/scd/internal/common/vless"
	"github.com/a1ekseev/scd/internal/scd/config"
	"github.com/a1ekseev/scd/internal/scd/manifest"
	"github.com/a1ekseev/scd/internal/scd/normalize"
	"github.com/a1ekseev/scd/internal/scd/outbound"
)

const (
	berlin = "vless://7d1b6590-1069-4372-92be-8d0a0ae6eaf6@example.com:443?type=tcp&security=tls#🇩🇪 Берлин, Германия"
	vienna = "vless://7d1b6590-1069-4372-92be-8d0a0ae6eaf5@example.com:443?type=tcp&security=tls#🇦🇹 Вена, Австрия"
	vision = "vless://7d1b6590-1069-4372-92be-8d0a0ae6eaf5@example.com:443?security=reality&encryption=none&fp=chrome&headerType=none&type=tcp&flow=xtls-rprx-vision&sni=io.example.test&pbk=OHWtGCfZc0FXULj2ZYeW_vlKIlurak79i6AXun9oJQg&sid=cbad7f159438fee4#🇩🇪 Германия, Extra"
	tlsVis = "vless://7d1b6590-1069-4372-92be-8d0a0ae6eaf5@example.com:443?type=tcp&security=tls&flow=xtls-rprx-vision#🇩🇪 Германия, Extra"
)

func build(lines ...string) manifest.Manifest {
	return manifest.Build(strings.Join(lines, "\n"), "inline", normalize.Policy{TagPrefix: "vless"}, time.Unix(0, 0))
}

func target(mut func(*config.SubscriptionTargetConfig)) config.SubscriptionTargetConfig {
	t := config.SubscriptionTargetConfig{
		Address:      "127.0.0.1:8080",
		InboundSocks: &config.InboundSocksConfig{Listen: "127.0.0.1"},
	}
	if mut != nil {
		mut(&t)
	}
	return t
}

// buildSrc builds the topology of source "vless" with ports 20000..20010,
// or the given range.
func buildSrc(m manifest.Manifest, t config.SubscriptionTargetConfig, pr ...config.PortRange) (Topology, []vless.Skip) {
	src := config.SourceConfig{ID: "vless", PortRange: &config.PortRange{Start: 20000, End: 20010}}
	if len(pr) > 0 {
		src.PortRange = &pr[0]
	}
	return BuildSource(m, src, t)
}

func TestAllocatesStablePortsAndTargetScopedTags(t *testing.T) { // T1
	m := build(berlin, vienna)
	if len(m.Entries) != 2 {
		t.Fatalf("entries = %d, skipped = %+v", len(m.Entries), m.Skipped)
	}
	topo, skipped := buildSrc(m, target(func(c *config.SubscriptionTargetConfig) {
		c.ObservatorySubjectSelectorPrefix = "x-observe-"
	}))
	if len(skipped) > 0 {
		t.Fatalf("skipped = %+v", skipped)
	}
	if len(topo.Tunnels) != 2 {
		t.Fatalf("tunnels = %d", len(topo.Tunnels))
	}
	a, b := topo.Tunnels[0], topo.Tunnels[1]
	if a.Port != 20000 || b.Port != 20001 {
		t.Errorf("ports = %d, %d", a.Port, b.Port)
	}
	if !strings.HasPrefix(a.BaseOutboundTag, "vless-at-") || !strings.HasPrefix(b.BaseOutboundTag, "vless-de-") {
		t.Errorf("order = %s, %s", a.BaseOutboundTag, b.BaseOutboundTag)
	}
	if a.OutboundTagInitial != "x-observe-"+a.BaseOutboundTag || a.OutboundTagCurrent != a.OutboundTagInitial {
		t.Errorf("outbound tags = %q / %q", a.OutboundTagInitial, a.OutboundTagCurrent)
	}
	if a.OutboundInitial.Tag != a.OutboundTagInitial || a.OutboundInitial.Normalized.Tag != a.OutboundTagInitial || a.OutboundInitial.JSON.GetTag() != a.OutboundTagInitial {
		t.Errorf("initial outbound tag mismatch: %+v", a.OutboundInitial.Tag)
	}
	if a.OutboundWithoutPrefix.Tag != a.BaseOutboundTag || a.OutboundWithoutPrefix.JSON.GetTag() != a.BaseOutboundTag {
		t.Errorf("withoutPrefix tag = %q", a.OutboundWithoutPrefix.Tag)
	}
	if a.BaseTunnelID != a.BaseOutboundTag || a.InboundTag != "in-"+a.BaseOutboundTag || a.RouteTag != "route-"+a.BaseOutboundTag {
		t.Errorf("tags = %q %q %q", a.BaseTunnelID, a.InboundTag, a.RouteTag)
	}
	if a.Listen != "127.0.0.1" || a.CountryISO2 != "AT" || !strings.Contains(a.DisplayName, "Вена") {
		t.Errorf("listen/country/name = %q %q %q", a.Listen, a.CountryISO2, a.DisplayName)
	}
}

func TestVisionOverridePerTarget(t *testing.T) { // T2
	m := build(vision)
	plain, skipped := buildSrc(m, target(nil))
	if len(skipped) > 0 {
		t.Fatalf("skipped = %+v", skipped)
	}
	udp, skipped := buildSrc(m, target(func(c *config.SubscriptionTargetConfig) { c.VisionUDP443Override = true }))
	if len(skipped) > 0 {
		t.Fatalf("skipped = %+v", skipped)
	}
	if len(plain.Tunnels) != 1 || len(udp.Tunnels) != 1 {
		t.Fatalf("tunnels = %d, %d (skipped %+v)", len(plain.Tunnels), len(udp.Tunnels), m.Skipped)
	}
	p, u := plain.Tunnels[0], udp.Tunnels[0]
	for name, got := range map[string]string{"initial": p.OutboundInitial.Normalized.Flow, "without": p.OutboundWithoutPrefix.Normalized.Flow} {
		if got != "xtls-rprx-vision" {
			t.Errorf("plain %s flow = %q", name, got)
		}
	}
	for name, got := range map[string]string{"initial": u.OutboundInitial.Normalized.Flow, "without": u.OutboundWithoutPrefix.Normalized.Flow} {
		if got != "xtls-rprx-vision-udp443" {
			t.Errorf("override %s flow = %q", name, got)
		}
	}
	if u.OutboundInitial.JSON.(outbound.Outbound).Settings.Vnext[0].Users[0].Flow != "xtls-rprx-vision-udp443" {
		t.Errorf("JSON flow not overridden")
	}
}

func TestVisionOverrideLeavesTLS(t *testing.T) {
	m := build(tlsVis)
	topo, skipped := buildSrc(m, target(func(c *config.SubscriptionTargetConfig) { c.VisionUDP443Override = true }))
	if len(skipped) > 0 {
		t.Fatalf("skipped = %+v", skipped)
	}
	if len(topo.Tunnels) != 1 {
		t.Fatalf("tcp-tls vision not accepted: %+v", m.Skipped)
	}
	if got := topo.Tunnels[0].OutboundInitial.Normalized.Flow; got != "xtls-rprx-vision" {
		t.Errorf("flow = %q", got)
	}
}

func TestSourceIDAndNoPortRange(t *testing.T) {
	topo, skipped := buildSrc(build(berlin), target(nil))
	if len(skipped) > 0 || len(topo.Tunnels) != 1 || topo.Tunnels[0].SourceID != "vless" {
		t.Fatalf("topo = %+v, skipped = %v", topo, skipped)
	}
	topo, skipped = BuildSource(build(berlin), config.SourceConfig{ID: "vless"}, target(nil))
	if len(skipped) > 0 || len(topo.Tunnels) != 0 {
		t.Fatalf("without portRange: %+v, %v", topo, skipped)
	}
}

func TestNoInboundSocksEmpty(t *testing.T) {
	topo, skipped := buildSrc(build(berlin, vienna), target(func(c *config.SubscriptionTargetConfig) { c.InboundSocks = nil }))
	if len(skipped) > 0 {
		t.Fatalf("skipped = %+v", skipped)
	}
	if len(topo.Tunnels) != 0 {
		t.Errorf("tunnels = %d", len(topo.Tunnels))
	}
}

func TestPortsFollowTagOrderNotLineOrder(t *testing.T) {
	forward, skipped := buildSrc(build(berlin, vienna), target(nil))
	if len(skipped) > 0 {
		t.Fatalf("skipped = %+v", skipped)
	}
	reverse, skipped := buildSrc(build(vienna, berlin), target(nil))
	if len(skipped) > 0 {
		t.Fatalf("skipped = %+v", skipped)
	}
	if len(forward.Tunnels) != 2 || len(reverse.Tunnels) != 2 {
		t.Fatal("expected two tunnels")
	}
	for i := range forward.Tunnels {
		f, r := forward.Tunnels[i], reverse.Tunnels[i]
		if f.BaseTunnelID != r.BaseTunnelID || f.Port != r.Port {
			t.Errorf("tunnel %d: %s:%d vs %s:%d", i, f.BaseTunnelID, f.Port, r.BaseTunnelID, r.Port)
		}
	}
}

func TestExactCapacityFits(t *testing.T) {
	topo, skipped := buildSrc(build(berlin, vienna), target(nil), config.PortRange{Start: 30000, End: 30001})
	if len(skipped) > 0 || len(topo.Tunnels) != 2 || topo.Tunnels[1].Port != 30001 {
		t.Fatalf("topo = %+v, skipped = %v", topo, skipped)
	}
}

func TestPortRangeCapacity(t *testing.T) { // T3
	lines := []string{berlin, vienna, vision}
	for _, tc := range []struct {
		name    string
		pr      config.PortRange
		tunnels int
	}{
		{"zero", config.PortRange{Start: 20000, End: 19999}, 0},
		{"one", config.PortRange{Start: 20000, End: 20000}, 1},
		{"n-1", config.PortRange{Start: 20000, End: 20001}, 2},
		{"n", config.PortRange{Start: 20000, End: 20002}, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			topo, skipped := buildSrc(build(lines...), target(nil), tc.pr)
			if len(topo.Tunnels) != tc.tunnels || len(skipped) != len(lines)-tc.tunnels {
				t.Fatalf("tunnels = %d, skipped = %d", len(topo.Tunnels), len(skipped))
			}
			full, _ := buildSrc(build(lines...), target(nil))
			for i, tn := range topo.Tunnels {
				if tn.BaseTunnelID != full.Tunnels[i].BaseTunnelID || tn.Port != 20000+i {
					t.Errorf("tunnel %d = %s:%d, want %s:%d", i, tn.BaseTunnelID, tn.Port, full.Tunnels[i].BaseTunnelID, 20000+i)
				}
			}
			for i, sk := range skipped {
				want := full.Tunnels[tc.tunnels+i]
				if sk.Code != CodePortRangeExhausted || sk.Label == nil || *sk.Label != want.DisplayName || sk.Raw == "" || len(sk.Details) != 1 {
					t.Errorf("skip %d = %+v, want the entry %s", i, sk, want.BaseTunnelID)
				}
			}
		})
	}
}

func TestPortRangeOverflowStableAcrossInputOrder(t *testing.T) {
	pr := config.PortRange{Start: 20000, End: 20000}
	for _, lines := range [][]string{{berlin, vienna, vision}, {vision, berlin, vienna}, {vienna, vision, berlin}} {
		topo, skipped := buildSrc(build(lines...), target(nil), pr)
		if len(topo.Tunnels) != 1 || !strings.HasPrefix(topo.Tunnels[0].BaseTunnelID, "vless-at-") {
			t.Errorf("%d lines: tunnels = %+v", len(lines), topo.Tunnels)
		}
		var labels []string
		for _, sk := range skipped {
			labels = append(labels, *sk.Label)
		}
		if got := strings.Join(labels, "|"); got != "🇩🇪 Берлин, Германия|🇩🇪 Германия, Extra" {
			t.Errorf("skipped = %s", got)
		}
	}
}
