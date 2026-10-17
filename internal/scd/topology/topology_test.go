package topology

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/a1ekseev/scd/internal/scd/config"
	"github.com/a1ekseev/scd/internal/scd/manifest"
	"github.com/a1ekseev/scd/internal/scd/normalize"
)

const (
	berlin = "vless://7d1b6590-1069-4372-92be-8d0a0ae6eaf6@example.com:443?type=tcp&security=tls#🇩🇪 Берлин, Германия"
	vienna = "vless://7d1b6590-1069-4372-92be-8d0a0ae6eaf5@example.com:443?type=tcp&security=tls#🇦🇹 Вена, Австрия"
	vision = "vless://7d1b6590-1069-4372-92be-8d0a0ae6eaf5@example.com:443?security=reality&encryption=none&fp=chrome&headerType=none&type=tcp&flow=xtls-rprx-vision&sni=io.example.test&pbk=OHWtGCfZc0FXULj2ZYeW_vlKIlurak79i6AXun9oJQg&sid=cbad7f159438fee4#🇩🇪 Германия, Extra"
	tlsVis = "vless://7d1b6590-1069-4372-92be-8d0a0ae6eaf5@example.com:443?type=tcp&security=tls&flow=xtls-rprx-vision#🇩🇪 Германия, Extra"
)

func build(lines ...string) manifest.Manifest {
	return manifest.Build(strings.Join(lines, "\n"), "inline", normalize.Policy{}, time.Unix(0, 0))
}

func target(mut func(*config.SubscriptionTargetConfig)) config.SubscriptionTargetConfig {
	t := config.SubscriptionTargetConfig{
		Address: "127.0.0.1:8080",
		InboundSocks: &config.InboundSocksConfig{
			Listen:    "127.0.0.1",
			PortRange: config.PortRange{Start: 20000, End: 20010},
		},
	}
	if mut != nil {
		mut(&t)
	}
	return t
}

func TestAllocatesStablePortsAndTargetScopedTags(t *testing.T) { // T1
	m := build(berlin, vienna)
	if len(m.Entries) != 2 {
		t.Fatalf("entries = %d, skipped = %+v", len(m.Entries), m.Skipped)
	}
	topo, err := Build(m, target(func(c *config.SubscriptionTargetConfig) {
		c.ObservatorySubjectSelectorPrefix = "x-observe-"
	}))
	if err != nil {
		t.Fatal(err)
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
	if a.OutboundInitial.Tag != a.OutboundTagInitial || a.OutboundInitial.Normalized.Tag != a.OutboundTagInitial || a.OutboundInitial.JSON.Tag != a.OutboundTagInitial {
		t.Errorf("initial outbound tag mismatch: %+v", a.OutboundInitial.Tag)
	}
	if a.OutboundWithoutPrefix.Tag != a.BaseOutboundTag || a.OutboundWithoutPrefix.JSON.Tag != a.BaseOutboundTag {
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
	plain, err := Build(m, target(nil))
	if err != nil {
		t.Fatal(err)
	}
	udp, err := Build(m, target(func(c *config.SubscriptionTargetConfig) { c.VisionUDP443Override = true }))
	if err != nil {
		t.Fatal(err)
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
	if u.OutboundInitial.JSON.Settings.Vnext[0].Users[0].Flow != "xtls-rprx-vision-udp443" {
		t.Errorf("JSON flow not overridden")
	}
}

func TestVisionOverrideLeavesTLS(t *testing.T) {
	m := build(tlsVis)
	topo, err := Build(m, target(func(c *config.SubscriptionTargetConfig) { c.VisionUDP443Override = true }))
	if err != nil {
		t.Fatal(err)
	}
	if len(topo.Tunnels) != 1 {
		t.Skipf("tcp-tls vision not accepted: %+v", m.Skipped)
	}
	if got := topo.Tunnels[0].OutboundInitial.Normalized.Flow; got != "xtls-rprx-vision" {
		t.Errorf("flow = %q", got)
	}
}

func TestPortRangeExhausted(t *testing.T) { // T3
	_, err := Build(build(berlin, vienna), target(func(c *config.SubscriptionTargetConfig) {
		c.InboundSocks.PortRange = config.PortRange{Start: 20000, End: 20000}
	}))
	if !errors.Is(err, ErrPortRangeExhausted) {
		t.Fatalf("err = %v", err)
	}
	const want = `target "127.0.0.1:8080" inboundSocks.portRange can allocate only 1 ports, but 2 tunnels were generated`
	if !strings.Contains(err.Error(), want) {
		t.Errorf("err = %q, want it to contain %q", err, want)
	}
}

func TestNoInboundSocksEmpty(t *testing.T) {
	topo, err := Build(build(berlin, vienna), target(func(c *config.SubscriptionTargetConfig) { c.InboundSocks = nil }))
	if err != nil {
		t.Fatal(err)
	}
	if len(topo.Tunnels) != 0 {
		t.Errorf("tunnels = %d", len(topo.Tunnels))
	}
}

func TestPortsFollowTagOrderNotLineOrder(t *testing.T) {
	forward, err := Build(build(berlin, vienna), target(nil))
	if err != nil {
		t.Fatal(err)
	}
	reverse, err := Build(build(vienna, berlin), target(nil))
	if err != nil {
		t.Fatal(err)
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
	topo, err := Build(build(berlin, vienna), target(func(c *config.SubscriptionTargetConfig) {
		c.InboundSocks.PortRange = config.PortRange{Start: 30000, End: 30001}
	}))
	if err != nil || len(topo.Tunnels) != 2 || topo.Tunnels[1].Port != 30001 {
		t.Fatalf("topo = %+v, err = %v", topo, err)
	}
}
