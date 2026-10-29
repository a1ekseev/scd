//go:build xraylive

package livetest

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/xtls/xray-core/core"

	"github.com/a1ekseev/scd/internal/common/candidate"
	"github.com/a1ekseev/scd/internal/scd/apply"
	"github.com/a1ekseev/scd/internal/scd/config"
	"github.com/a1ekseev/scd/internal/scd/manifest"
	"github.com/a1ekseev/scd/internal/scd/monitor"
	"github.com/a1ekseev/scd/internal/scd/outbound"
	"github.com/a1ekseev/scd/internal/scd/selection"
	"github.com/a1ekseev/scd/internal/scd/state"
	"github.com/a1ekseev/scd/internal/scd/syncer"
	"github.com/a1ekseev/scd/internal/scd/topology"
	"github.com/a1ekseev/scd/internal/scd/xrayapi"
)

// A real Xray accepts the WireGuard outbounds scd builds (AddOutbound) and
// removes them; no network is used: the device starts on the first
// connection, which this test never makes.
func TestLiveWireGuardOutbound(t *testing.T) {
	c, _, _ := startXray(t)
	b, err := os.ReadFile(root + "docker/fixtures/wireguard/candidates.txt")
	if err != nil {
		t.Fatal(err)
	}
	m := manifest.BuildWireGuard(string(b), "wg", "wg", "UseIPv4", time.Unix(0, 0))
	if len(m.Entries) == 0 {
		t.Fatal("no entries")
	}
	for _, e := range m.Entries {
		h, err := outbound.ToProto(e.JSONOutbound.WithTag("x-observe-" + e.Tag))
		if err != nil {
			t.Fatal(err)
		}
		if err := c.AddOutbound(t.Context(), h); err != nil {
			t.Fatalf("AddOutbound %s: %v", e.Tag, err)
		}
	}
	list, err := c.ListOutbounds(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var tags []string
	for _, h := range list {
		if strings.HasPrefix(h.GetTag(), "x-observe-wg-") {
			tags = append(tags, h.GetTag())
		}
	}
	if len(tags) != len(m.Entries) {
		t.Fatalf("Xray has %v, added %d", tags, len(m.Entries))
	}
	for _, tag := range tags {
		if err := c.RemoveOutbound(t.Context(), tag); err != nil {
			t.Fatalf("RemoveOutbound %s: %v", tag, err)
		}
	}
	list, err = c.ListOutbounds(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(list, func(h *core.OutboundHandlerConfig) bool { return strings.HasPrefix(h.GetTag(), "x-observe-wg-") }) {
		t.Fatal("WireGuard outbounds left in Xray")
	}
}

// cappedClient fails the test when an AddOutbound would put more than max
// outbounds of the source (tags with one of prefixes) into Xray.
type cappedClient struct {
	xrayapi.Client
	t        *testing.T
	max      int
	prefixes []string
}

func (c cappedClient) AddOutbound(ctx context.Context, h *core.OutboundHandlerConfig) error {
	outs, err := c.ListOutbounds(ctx)
	if err != nil {
		return err
	}
	n := 0
	for _, o := range outs {
		if slices.ContainsFunc(c.prefixes, func(p string) bool { return strings.HasPrefix(o.Tag, p) }) {
			n++
		}
	}
	if n >= c.max {
		c.t.Errorf("AddOutbound(%s) with %d outbounds of the source in Xray, maxActive %d", h.Tag, n, c.max)
	}
	return c.Client.AddOutbound(ctx, h)
}

// TestLiveWireGuardReplaceAndFill: a full apply of two WireGuard tunnels,
// then ReplaceTunnel of slot 0, RemoveTunnel of slot 1 and AddTunnel into
// it, against a real Xray without network; never more than maxActive
// outbounds of the source, and at the end exactly the expected outbounds,
// inbounds and rules.
func TestLiveWireGuardReplaceAndFill(t *testing.T) {
	xc, _, _ := startXray(t)
	c := cappedClient{Client: xc, t: t, max: 2, prefixes: []string{"x-live-wg-", "wg-"}}
	b, err := os.ReadFile(root + "docker/fixtures/wireguard/candidates.txt")
	if err != nil {
		t.Fatal(err)
	}
	tc, _ := liveTarget(t, 3)
	tc.ObservatorySubjectSelectorPrefix = "x-live-"
	start := freePortRange(t, 3)
	src := config.SourceConfig{
		ID: "wg", Type: config.SourceWireGuard, Enabled: true, MaxActive: 2,
		PortRange: &config.PortRange{Start: start, End: start + 2},
		Selection: &config.SelectionConfig{Strategy: config.StrategyScore, SwitchConfirmChecks: 1, DeadPairCooldownMinutes: 4, ReplaceAfterFailures: 1},
		WireGuard: &config.WireGuardConfig{EndpointDomainStrategy: "UseIPv4"},
	}
	m := manifest.BuildWireGuard(string(b), "wg", "wg", "UseIPv4", time.Unix(0, 0))
	var topo topology.Topology
	for _, kind := range apply.Order {
		p, err := apply.PlanForSource(kind, m, src, tc)
		if err != nil {
			t.Fatal(err)
		}
		if r := apply.Apply(t.Context(), c, p, tc, apply.Meta{SubscriptionID: "live"}); r.Failed != 0 {
			t.Fatalf("%s failed: %+v", kind, r.Items)
		}
		topo = *p.Topology
	}
	if len(topo.Tunnels) != 2 {
		t.Fatalf("active %d", len(topo.Tunnels))
	}

	byTag := map[string]manifest.Entry{}
	var cands []selection.Candidate
	for _, e := range selection.Rank(m.Entries, src.Selection.Strategy, manifest.Entry.SelectionCandidate) {
		byTag[e.Tag] = e
		cands = append(cands, e.SelectionCandidate())
	}
	active, keys := map[string]bool{}, map[[32]byte]bool{}
	for _, tn := range topo.Tunnels {
		active[tn.BaseTunnelID], keys[byTag[tn.BaseTunnelID].SelectionCandidate().Key] = true, true
	}
	pick := func() manifest.Entry {
		t.Helper()
		next, ok := selection.Replacement(cands, active, keys, &selection.State{}, time.Unix(0, 0))
		if !ok {
			t.Fatal("no candidate left")
		}
		active[next.Tag], keys[next.Key] = true, true
		return byTag[next.Tag]
	}

	old0, old1 := topo.Tunnels[0], topo.Tunnels[1]
	delete(keys, byTag[old0.BaseTunnelID].SelectionCandidate().Key)
	new0 := topology.NewTunnel(pick(), src.ID, tc, old0.Port)
	if _, err := apply.ReplaceTunnel(t.Context(), c, old0, new0); err != nil {
		t.Fatalf("ReplaceTunnel: %v", err)
	}
	if _, err := apply.RemoveTunnel(t.Context(), c, old1); err != nil {
		t.Fatalf("RemoveTunnel: %v", err)
	}
	delete(keys, byTag[old1.BaseTunnelID].SelectionCandidate().Key)
	new1 := topology.NewTunnel(pick(), src.ID, tc, old1.Port)
	if err := apply.AddTunnel(t.Context(), c, new1); err != nil {
		t.Fatalf("AddTunnel: %v", err)
	}

	s := readState(t, xc)
	var outs, ins []string
	for _, tag := range s.outbounds {
		if strings.HasPrefix(tag, "x-live-wg-") || strings.HasPrefix(tag, "wg-") {
			outs = append(outs, tag)
		}
	}
	for _, tag := range s.inbounds {
		if strings.HasPrefix(tag, "in-wg-") {
			ins = append(ins, tag)
		}
	}
	var rules []xrayapi.Rule
	for _, r := range s.rules {
		if strings.HasPrefix(r.RuleTag, "route-wg-") {
			rules = append(rules, r)
		}
	}
	slices.SortFunc(rules, func(a, b xrayapi.Rule) int { return strings.Compare(a.RuleTag, b.RuleTag) })
	wantOuts := []string{new0.OutboundTagInitial, new1.OutboundTagInitial}
	wantIns := []string{new0.InboundTag, new1.InboundTag}
	wantRules := []xrayapi.Rule{{Tag: new0.OutboundTagInitial, RuleTag: new0.RouteTag}, {Tag: new1.OutboundTagInitial, RuleTag: new1.RouteTag}}
	slices.Sort(wantOuts)
	slices.Sort(wantIns)
	slices.SortFunc(wantRules, func(a, b xrayapi.Rule) int { return strings.Compare(a.RuleTag, b.RuleTag) })
	if !slices.Equal(outs, wantOuts) || !slices.Equal(ins, wantIns) || !slices.Equal(rules, wantRules) {
		t.Fatalf("outbounds %v / %v, inbounds %v / %v, rules %v / %v", outs, wantOuts, ins, wantIns, rules, wantRules)
	}
}

func (c cappedClient) Close() error { return nil }

// TestLiveWireGuardSyncer runs Syncer against a real Xray through
// cappedClient: a full apply, a replacement of a gone candidate, a removal
// without replacement and a fill of the empty slot; never more than
// maxActive outbounds of the source at an AddOutbound, and at the end
// exactly the tunnels of the state.
func TestLiveWireGuardSyncer(t *testing.T) {
	xc, _, _ := startXray(t)
	b, err := os.ReadFile(root + "docker/fixtures/wireguard/candidates.txt")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	path := filepath.Join(t.TempDir(), "wg")
	write := func(ns ...int) {
		var s strings.Builder
		for _, n := range ns {
			s.WriteString(lines[n-1] + "\n")
		}
		if err := os.WriteFile(path, []byte(s.String()), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	tc, _ := liveTarget(t, 3)
	tc.ObservatorySubjectSelectorPrefix = "x-live-"
	start := freePortRange(t, 3)
	src := config.SourceConfig{
		ID: "wg", Type: config.SourceWireGuard, Input: path, Enabled: true, MaxActive: 2, MaxInputBytes: 1 << 20,
		PortRange: &config.PortRange{Start: start, End: start + 2},
		Selection: &config.SelectionConfig{Strategy: config.StrategyScore, SwitchMarginPercent: 95, SwitchConfirmChecks: 1, DeadPairCooldownMinutes: 4, ReplaceAfterFailures: 1},
		WireGuard: &config.WireGuardConfig{EndpointDomainStrategy: "UseIPv4"},
	}
	sub := config.SubscriptionConfig{ID: "live", Enabled: true, Sources: []config.SourceConfig{src}, Target: tc}
	on := config.ResourceToggle{Enabled: true}
	s := &syncer.Syncer{
		Config: config.Loaded{Path: "/live/config.yml", Config: config.AppConfig{
			Subscriptions: []config.SubscriptionConfig{sub},
			Resources:     config.ResourceConfig{Outbounds: on, Inbounds: on, Routing: on},
		}},
		Memory: state.New(),
		Log:    slog.New(slog.DiscardHandler),
		Dial: func(string, time.Duration) (xrayapi.Client, error) {
			return cappedClient{Client: xc, t: t, max: 2, prefixes: []string{"x-live-wg-", "wg-"}}, nil
		},
	}
	for i, set := range [][]int{{1, 2, 4, 5}, {1, 4, 5}, {1}, {1, 3}} {
		write(set...)
		r, err := s.Run(t.Context())
		if err != nil || r.Failed != 0 {
			t.Fatalf("sync %d: %+v, %v", i, r, err)
		}
	}
	var want []string
	s.Memory.View(state.Key(sub.ID, tc.Address), func(tg *state.Target) {
		for _, tn := range tg.Tunnels {
			want = append(want, tn.Mapping.OutboundTagCurrent)
		}
	})
	slices.Sort(want)
	var got []string
	for _, tag := range readState(t, xc).outbounds {
		if strings.HasPrefix(tag, "x-live-wg-") || strings.HasPrefix(tag, "wg-") {
			got = append(got, tag)
		}
	}
	if len(want) != 2 || !slices.Equal(got, want) {
		t.Fatalf("Xray %v, state %v", got, want)
	}
}

// wgKey returns a fresh X25519 private key.
func wgKey(t *testing.T) *ecdh.PrivateKey {
	t.Helper()
	k, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// TestLiveWireGuardTraffic: a second Xray with a WireGuard inbound on
// 127.0.0.1 is the peer of two candidates (keys A and B). scd syncs a
// source file with candidate A, then repairs and rejoins the tunnel, then
// the file switches to candidate B and the sync replaces the tunnel in its
// slot. After each step an HTTP request through the tunnel's SOCKS inbound
// reaches the HTTP server behind the peer, and Xray holds exactly one
// outbound of the source.
func TestLiveWireGuardTraffic(t *testing.T) {
	xc, _, _ := startXray(t)
	peer, keyA, keyB := wgKey(t), wgKey(t), wgKey(t)
	b64 := func(b []byte) string { return base64.StdEncoding.EncodeToString(b) }
	wgPort := freeUDPPort(t, false)
	launchXrayConf(t, "testdata/wireguard-peer", map[string]string{
		"{{WG_PORT}}":             strconv.Itoa(wgPort),
		"{{PEER_SECRET_KEY}}":     b64(peer.Bytes()),
		"{{CLIENT_A_PUBLIC_KEY}}": b64(keyA.PublicKey().Bytes()),
		"{{CLIENT_B_PUBLIC_KEY}}": b64(keyB.PublicKey().Bytes()),
		"{{HTTP_ADDR}}":           tunnelServer(t),
	})
	line := func(k *ecdh.PrivateKey, addr string) string {
		return candidate.FormatWireGuard(candidate.WireGuard{
			Provider:   "static",
			PrivateKey: [32]byte(k.Bytes()),
			Endpoint:   net.JoinHostPort("127.0.0.1", strconv.Itoa(wgPort)),
			PublicKey:  [32]byte(peer.PublicKey().Bytes()),
			Address:    []netip.Prefix{netip.MustParsePrefix(addr)},
			MTU:        1420,
			AllowedIPs: []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0")},
			Label:      "\U0001F1F3\U0001F1F1 Amsterdam",
		}) + "\n"
	}
	path := filepath.Join(t.TempDir(), "wg")
	write := func(s string) {
		if err := os.WriteFile(path, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	tc, _ := liveTarget(t, 1)
	tc.ObservatorySubjectSelectorPrefix = "x-live-"
	start := freePortRange(t, 1)
	src := config.SourceConfig{
		ID: "wg", Type: config.SourceWireGuard, Input: path, Enabled: true, MaxActive: 1, MaxInputBytes: 1 << 20,
		PortRange: &config.PortRange{Start: start, End: start},
		Selection: &config.SelectionConfig{Strategy: config.StrategyScore, SwitchConfirmChecks: 1, DeadPairCooldownMinutes: 4, ReplaceAfterFailures: 1},
		WireGuard: &config.WireGuardConfig{EndpointDomainStrategy: "UseIPv4"},
	}
	sub := config.SubscriptionConfig{ID: "live", Enabled: true, Sources: []config.SourceConfig{src}, Target: tc}
	on := config.ResourceToggle{Enabled: true}
	prefixes := []string{"x-live-wg-", "wg-"}
	mem := state.New()
	key := state.Key(sub.ID, tc.Address)
	s := &syncer.Syncer{
		Config: config.Loaded{Path: "/live/config.yml", Config: config.AppConfig{
			Subscriptions: []config.SubscriptionConfig{sub},
			Resources:     config.ResourceConfig{Outbounds: on, Inbounds: on, Routing: on},
		}},
		Memory: mem,
		Log:    slog.New(slog.DiscardHandler),
		Dial: func(string, time.Duration) (xrayapi.Client, error) {
			return cappedClient{Client: xc, t: t, max: 1, prefixes: prefixes}, nil
		},
	}
	sync := func() {
		t.Helper()
		if r, err := s.Run(t.Context()); err != nil || r.Failed != 0 {
			t.Fatalf("sync: %+v, %v", r, err)
		}
	}
	tunnel := func() topology.Tunnel {
		t.Helper()
		var tns []topology.Tunnel
		mem.View(key, func(tg *state.Target) {
			for _, tn := range tg.Tunnels {
				tns = append(tns, tn.Mapping)
			}
		})
		if len(tns) != 1 {
			t.Fatalf("state has %d tunnels, want 1", len(tns))
		}
		return tns[0]
	}
	// check: the source has exactly the tunnel's current outbound in Xray,
	// and the tunnel carries traffic.
	check := func(step string) topology.Tunnel {
		t.Helper()
		tn := tunnel()
		var outs []string
		for _, tag := range readState(t, xc).outbounds {
			if slices.ContainsFunc(prefixes, func(p string) bool { return strings.HasPrefix(tag, p) }) {
				outs = append(outs, tag)
			}
		}
		if !slices.Equal(outs, []string{tn.OutboundTagCurrent}) {
			t.Fatalf("%s: outbounds of the source %v, want [%s]", step, outs, tn.OutboundTagCurrent)
		}
		getThroughSOCKS(t, net.JoinHostPort(tn.Listen, strconv.Itoa(tn.Port)))
		return tn
	}

	write(line(keyA, "10.2.0.2/32"))
	sync()
	first := check("sync A")
	if first.OutboundTagCurrent != first.OutboundTagInitial || !strings.HasPrefix(first.OutboundTagInitial, "x-live-") {
		t.Fatalf("tunnel %+v not on its prefixed outbound", first)
	}

	d := monitor.Deps{
		Memory: mem,
		Log:    slog.New(slog.DiscardHandler),
		Dial: func(string, time.Duration) (xrayapi.Client, error) {
			return sharedClient{Client: xc, t: t, tags: []string{first.OutboundTagInitial, first.OutboundWithoutPrefix.Tag}}, nil
		},
	}
	if err := monitor.Repair(t.Context(), d, sub, first.BaseTunnelID); err != nil {
		t.Fatalf("repair: %v", err)
	}
	if tn := check("repair"); tn.OutboundTagCurrent != first.OutboundWithoutPrefix.Tag {
		t.Fatalf("repair: outbound %s, want %s", tn.OutboundTagCurrent, first.OutboundWithoutPrefix.Tag)
	}
	if err := monitor.Rejoin(t.Context(), d, sub, first.BaseTunnelID); err != nil {
		t.Fatalf("rejoin: %v", err)
	}
	if tn := check("rejoin"); tn.OutboundTagCurrent != first.OutboundTagInitial {
		t.Fatalf("rejoin: outbound %s, want %s", tn.OutboundTagCurrent, first.OutboundTagInitial)
	}

	write(line(keyB, "10.2.0.3/32"))
	sync()
	if tn := check("replace with B"); tn.BaseTunnelID == first.BaseTunnelID || tn.Port != first.Port {
		t.Fatalf("replace: tunnel %s port %d, was %s port %d", tn.BaseTunnelID, tn.Port, first.BaseTunnelID, first.Port)
	}
}
