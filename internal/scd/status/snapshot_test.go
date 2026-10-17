package status

import (
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/a1ekseev/scd/internal/scd/config"
	"github.com/a1ekseev/scd/internal/scd/manifest"
	"github.com/a1ekseev/scd/internal/scd/state"
	"github.com/a1ekseev/scd/internal/scd/topology"
)

const (
	testUUID = "7d1b6590-1069-4372-92be-8d0a0ae6eaf5"
	linkAT   = "vless://" + testUUID + "@vpn.example.test:443?type=tcp&security=tls#🇦🇹 Вена, Австрия, Extra"
)

func discardLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

// testTarget is the TS createTarget of the runtime tests: inboundSocks
// 127.0.0.1:20000-20010, prefix x-observe-, fixed outbounds direct/blocked.
func testTarget() config.SubscriptionTargetConfig {
	return config.SubscriptionTargetConfig{
		Address: "127.0.0.1:8080", TimeoutMs: 5000,
		FixedOutbounds: []string{"direct", "blocked"}, FixedInbounds: []string{}, FixedRouting: []string{},
		InboundSocks:                     &config.InboundSocksConfig{Listen: "127.0.0.1", PortRange: config.PortRange{Start: 20000, End: 20010}},
		Monitor:                          config.TargetMonitorConfig{MaxParallel: 10, Retry: config.RetryConfig{Attempts: 1, DelayMs: 250}},
		BalancerMonitor:                  config.TargetBalancerMonitorConfig{Retry: config.RetryConfig{Attempts: 1, DelayMs: 250}, RemotePing: config.TargetBalancerRemotePingConfig{TimeoutMs: 5000}},
		ObservatorySubjectSelectorPrefix: "x-observe-",
	}
}

// buildTopology builds the topology of text for target t as sync does.
func buildTopology(t *testing.T, sub config.SubscriptionConfig, text string) topology.Topology {
	t.Helper()
	m, err := manifest.FromSubscription(manifest.LoadedSubscription{Config: sub, Source: "/x", Content: text, Encoding: "plain"}, time.Unix(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	topo, err := topology.Build(m, sub.Target)
	if err != nil {
		t.Fatal(err)
	}
	return topo
}

// T4 (01 §17.2): the snapshot carries display name, country, endpoint and
// the ids of the "<subscriptionId>::<targetAddress>" key.
func TestBuildTunnelsExposesDisplayNameCountryEndpoint(t *testing.T) {
	sub := config.SubscriptionConfig{ID: "source-1", Enabled: true, Target: testTarget()}
	mem := state.New()
	mem.ReplaceTopology(state.Key("source-1", "127.0.0.1:8080"), buildTopology(t, sub, linkAT), nil)

	got := BuildTunnels(mem)
	if len(got) != 1 {
		t.Fatalf("tunnels %d", len(got))
	}
	tn := got[0]
	if tn.SubscriptionID != "source-1" || tn.TargetAddress != "127.0.0.1:8080" || tn.CountryISO2 != "AT" ||
		tn.Endpoint != "127.0.0.1:20000" || !strings.Contains(tn.DisplayName, "Вена") || tn.State != "idle" {
		t.Fatalf("%+v", tn)
	}
	if tn.BalancerMonitorState != "idle" || tn.BalancerMonitorRemotePingState != "idle" {
		t.Fatalf("balancer fields %+v", tn)
	}
}

// T5 (01 §17.2): balanced is true while the prefixed tag is current and
// false after a repair switched to the unprefixed outbound.
func TestBalancedFollowsPrefix(t *testing.T) {
	sub := config.SubscriptionConfig{ID: "source-1", Enabled: true, Target: testTarget()}
	topo := buildTopology(t, sub, linkAT)
	key := state.Key("source-1", "127.0.0.1:8080")
	mem := state.New()
	mem.ReplaceTopology(key, topo, nil)

	if b := BuildTunnels(mem)[0].Balanced; b == nil || !*b {
		t.Fatalf("balanced before repair: %v", b)
	}
	mem.Update(key, func(st *state.Target) {
		st.Tunnels[0].Mapping.OutboundTagCurrent = st.Tunnels[0].Mapping.OutboundWithoutPrefix.Tag
	})
	if b := BuildTunnels(mem)[0].Balanced; b == nil || *b {
		t.Fatalf("balanced after repair: %v", b)
	}
}

func TestBalancedUndefinedCases(t *testing.T) {
	for _, tc := range []struct {
		name string
		tn   topology.Tunnel
		want *bool
	}{
		{"no prefix", topology.Tunnel{OutboundTagInitial: "a", OutboundTagCurrent: "a", BaseOutboundTag: "a", OutboundWithoutPrefix: topology.PreparedOutbound{Tag: "a"}}, nil},
		{"no unprefixed tag", topology.Tunnel{OutboundTagInitial: "p-a", OutboundTagCurrent: "p-a"}, nil},
		{"initial does not end with tag", topology.Tunnel{OutboundTagInitial: "p-a", OutboundTagCurrent: "p-a", OutboundWithoutPrefix: topology.PreparedOutbound{Tag: "b"}}, nil},
		{"base tag fallback", topology.Tunnel{OutboundTagInitial: "p-a", OutboundTagCurrent: "p-a", BaseOutboundTag: "a"}, new(true)},
		{"repaired", topology.Tunnel{OutboundTagInitial: "p-a", OutboundTagCurrent: "a", OutboundWithoutPrefix: topology.PreparedOutbound{Tag: "a"}}, new(false)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Balanced(tc.tn)
			if (got == nil) != (tc.want == nil) || (got != nil && *got != *tc.want) {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
}

func TestBuildTunnelsSortsBySubscriptionTargetName(t *testing.T) {
	mem := state.New()
	add := func(sub, addr string, names ...string) {
		var topo topology.Topology
		for i, n := range names {
			topo.Tunnels = append(topo.Tunnels, topology.Tunnel{BaseTunnelID: n, DisplayName: n, Listen: "127.0.0.1", Port: 20000 + i})
		}
		mem.ReplaceTopology(state.Key(sub, addr), topo, nil)
	}
	add("sub-b", "127.0.0.1:8080", "Zeta", "Alpha")
	add("sub-a", "127.0.0.1:8082", "Beta")
	add("sub-a", "127.0.0.1:8080", "Gamma")

	var got []string
	for _, tn := range BuildTunnels(mem) {
		got = append(got, tn.SubscriptionID+"|"+tn.TargetAddress+"|"+tn.DisplayName)
	}
	want := []string{"sub-a|127.0.0.1:8080|Gamma", "sub-a|127.0.0.1:8082|Beta", "sub-b|127.0.0.1:8080|Alpha", "sub-b|127.0.0.1:8080|Zeta"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v", got)
	}
}

// 03 §10.5 item 8.
func TestGroupSortsByLatencyIdleLast(t *testing.T) {
	tunnels := []StatusTunnel{
		{SubscriptionID: "sub-1", TargetAddress: "127.0.0.1:8080", DisplayName: "Unknown", State: "degraded"},
		{SubscriptionID: "sub-1", TargetAddress: "127.0.0.1:8080", DisplayName: "Idle", State: "idle", LastLatencyMs: new(1)},
		{SubscriptionID: "sub-1", TargetAddress: "127.0.0.1:8080", DisplayName: "Slow", State: "healthy", LastLatencyMs: new(250)},
		{SubscriptionID: "sub-1", TargetAddress: "127.0.0.1:8080", DisplayName: "Fast", State: "healthy", LastLatencyMs: new(20)},
	}
	groups := GroupTunnels(tunnels)
	if len(groups) != 1 || len(groups[0].Targets) != 1 {
		t.Fatalf("%+v", groups)
	}
	var got []string
	for _, tn := range groups[0].Targets[0].Tunnels {
		got = append(got, tn.DisplayName)
	}
	if want := []string{"Fast", "Slow", "Idle", "Unknown"}; !slices.Equal(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
	// balancerMonitor comes from the first unsorted tunnel; state defaults to idle.
	if bm := groups[0].Targets[0].BalancerMonitor; bm.State != "idle" {
		t.Fatalf("balancer %+v", bm)
	}
}

// 03 §10.5 item 7.
func TestGroupSortsSubscriptionsAndTargets(t *testing.T) {
	tunnels := []StatusTunnel{
		{SubscriptionID: "sub-b", TargetAddress: "127.0.0.1:8081", DisplayName: "B", State: "idle", BalancerMonitorState: "healthy"},
		{SubscriptionID: "sub-a", TargetAddress: "127.0.0.1:8082", DisplayName: "A2", State: "idle"},
		{SubscriptionID: "sub-a", TargetAddress: "127.0.0.1:8080", DisplayName: "A1", State: "idle"},
	}
	groups := GroupTunnels(tunnels)
	var got []string
	for _, s := range groups {
		for _, tg := range s.Targets {
			got = append(got, s.SubscriptionID+"|"+tg.TargetAddress)
		}
	}
	if want := []string{"sub-a|127.0.0.1:8080", "sub-a|127.0.0.1:8082", "sub-b|127.0.0.1:8081"}; !slices.Equal(got, want) {
		t.Fatalf("got %v", got)
	}
	if groups[1].Targets[0].BalancerMonitor.State != "healthy" {
		t.Fatalf("balancer %+v", groups[1].Targets[0].BalancerMonitor)
	}
}

func TestConfiguredGroupsIncludeTargetsWithoutTunnels(t *testing.T) {
	cfg := config.AppConfig{Subscriptions: []config.SubscriptionConfig{
		{ID: "sub-c", Enabled: true, Target: config.SubscriptionTargetConfig{Address: "127.0.0.1:8090"}},
		{ID: "sub-off", Enabled: false, Target: config.SubscriptionTargetConfig{Address: "127.0.0.1:9999"}},
		{ID: "sub-a", Enabled: true, Target: config.SubscriptionTargetConfig{Address: "127.0.0.1:8080"}},
	}}
	mem := state.New()
	key := state.Key("sub-a", "127.0.0.1:8080")
	mem.ReplaceTopology(key, topology.Topology{}, nil)
	mem.Update(key, func(st *state.Target) {
		st.Balancer.State = "healthy"
		st.Balancer.LastLatencyMs = new(17)
		st.Balancer.ConsecutiveFailures = 3
	})

	groups := BuildConfiguredGroups(BuildTunnels(mem), cfg, mem)
	if len(groups) != 2 || groups[0].SubscriptionID != "sub-a" || groups[1].SubscriptionID != "sub-c" {
		t.Fatalf("%+v", groups)
	}
	a := groups[0].Targets[0]
	if a.TargetAddress != "127.0.0.1:8080" || a.Tunnels == nil || len(a.Tunnels) != 0 ||
		a.BalancerMonitor.State != "healthy" || a.BalancerMonitor.LastLatencyMs == nil || *a.BalancerMonitor.LastLatencyMs != 17 ||
		a.BalancerMonitor.RemotePingState != "idle" {
		t.Fatalf("sub-a target %+v", a)
	}
	c := groups[1].Targets[0]
	if c.TargetAddress != "127.0.0.1:8090" || c.BalancerMonitor.State != "idle" || c.BalancerMonitor.RemotePingState != "" {
		t.Fatalf("sub-c target %+v", c)
	}
}
