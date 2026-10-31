//go:build xraylive

package livetest

import (
	"context"
	"crypto/ecdh"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/xtls/xray-core/core"
	"github.com/xtls/xray-core/infra/conf"
	"golang.org/x/net/proxy"

	"github.com/a1ekseev/scd/internal/common/candidate"
	"github.com/a1ekseev/scd/internal/scd/config"
	"github.com/a1ekseev/scd/internal/scd/monitor"
	"github.com/a1ekseev/scd/internal/scd/outbound"
	"github.com/a1ekseev/scd/internal/scd/state"
	"github.com/a1ekseev/scd/internal/scd/syncer"
	"github.com/a1ekseev/scd/internal/scd/topology"
	"github.com/a1ekseev/scd/internal/scd/xrayapi"
)

// envRemoveCloses enables the checks that need an Xray whose
// RemoveOutbound closes the handler (Xray PR #7112).
const envRemoveCloses = "SCD_XRAY_REMOVE_CLOSES"

// chainUpWait bounds the wait for the WireGuard tunnel to come up through
// the upper stage (it retries its handshake on its own; about 35 s).
const chainUpWait = 90 * time.Second

// upperStage is the fixed outbound of the upper stage: a freedom redirect
// to the WireGuard peer, so the lower stage reaches its peer only through
// it. Observatory probes through it reach the HTTP 204 server on the TCP
// port with the peer's number.
func upperStage(t *testing.T, tag string, peerPort int) *core.OutboundHandlerConfig {
	t.Helper()
	raw := `{"tag": "` + tag + `", "protocol": "freedom", "settings": {"redirect": "127.0.0.1:` + strconv.Itoa(peerPort) + `"}}`
	var dc conf.OutboundDetourConfig
	if err := json.Unmarshal([]byte(raw), &dc); err != nil {
		t.Fatal(err)
	}
	h, err := dc.Build()
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// failsThroughSOCKS reports whether a GET through socksAddr fails (an
// error or no tunnelBody); the client gives up after 5 s.
func failsThroughSOCKS(t *testing.T, socksAddr string) bool {
	t.Helper()
	dl, err := proxy.SOCKS5("tcp", socksAddr, nil, proxy.Direct)
	if err != nil {
		t.Fatal(err)
	}
	hc := &http.Client{Transport: &http.Transport{DialContext: dl.(proxy.ContextDialer).DialContext, DisableKeepAlives: true}, Timeout: 5 * time.Second}
	body, err := get(t.Context(), hc)
	return err != nil || body != tunnelBody
}

// waitThroughSOCKS waits up to d for a GET through socksAddr to answer
// tunnelBody.
func waitThroughSOCKS(t *testing.T, socksAddr string, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for failsThroughSOCKS(t, socksAddr) {
		if time.Now().After(deadline) {
			t.Fatalf("no answer through %s after %s", socksAddr, d)
		}
		time.Sleep(time.Second)
	}
}

// cascade is a lower-stage WireGuard source "wg" that dials through the
// loopback chain-up into balancer x-up, on the chain Xray, with the
// WireGuard peer of its candidate on peerPort.
type cascade struct {
	xc       xrayapi.Client
	peerPort int
	src      config.SourceConfig
	tc       config.SubscriptionTargetConfig
	mem      *state.Memory
	log      *syncBuffer
}

// newCascade starts the chain Xray and the WireGuard peer of the lower
// stage (an HTTP 204 server listens on the TCP port of the peer's number,
// for observatory probes through a freedom upper stage) and writes the
// candidate file of source wg. Its endpoint is an unused port: the lower
// stage reaches the peer only through an upper stage that redirects there.
func newCascade(t *testing.T) *cascade {
	t.Helper()
	px := launchXrayConf(t, "testdata/chain", nil)
	c := &cascade{xc: px.client, mem: state.New(), log: px.log}
	peer, key := wgKey(t), wgKey(t)
	c.peerPort = freeUDPPort(t, true)
	probe, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(c.peerPort)))
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }), ReadHeaderTimeout: time.Second}
	go func() { _ = srv.Serve(probe) }()
	t.Cleanup(func() { _ = srv.Close() })
	launchXrayConf(t, "testdata/wireguard-peer", map[string]string{
		"{{WG_PORT}}":             strconv.Itoa(c.peerPort),
		"{{PEER_SECRET_KEY}}":     b64(peer.Bytes()),
		"{{CLIENT_A_PUBLIC_KEY}}": b64(key.PublicKey().Bytes()),
		"{{CLIENT_B_PUBLIC_KEY}}": b64(wgKey(t).PublicKey().Bytes()),
		"{{HTTP_ADDR}}":           tunnelServer(t),
	})
	path := filepath.Join(t.TempDir(), "wg")
	line := candidate.FormatWireGuard(candidate.WireGuard{
		Provider: "static", PrivateKey: [32]byte(key.Bytes()),
		// Not loopback: inside a WireGuard upper stage the netstack drops a
		// loopback destination; the upper stage redirects it to the peer.
		Endpoint:  net.JoinHostPort("192.0.2.1", strconv.Itoa(freeUDPPort(t, false))),
		PublicKey: [32]byte(peer.PublicKey().Bytes()),
		// Not 127.0.0.1: the address inside the tunnel is the peer's subnet.
		Address: []netip.Prefix{netip.MustParsePrefix("10.2.0.2/32")},
		MTU:     1420, AllowedIPs: []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0")},
		Label: "\U0001F1F3\U0001F1F1 Amsterdam",
	}) + "\n"
	if err := os.WriteFile(path, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	start := freePortRange(t, 1)
	c.tc = config.SubscriptionTargetConfig{
		Address: "live-xray", TimeoutMs: int(rpcTimeout.Milliseconds()),
		FixedOutbounds:                   []string{"chain-fail", "direct", "chain-up", "chain-missing", "x-live-up-1", "x-live-up-2"},
		FixedInbounds:                    []string{"api", "socks-proxy"},
		FixedRouting:                     []string{"chain-up"},
		InboundSocks:                     &config.InboundSocksConfig{Listen: "127.0.0.1"},
		ObservatorySubjectSelectorPrefix: "x-live-",
	}
	c.src = config.SourceConfig{
		ID: "wg", Type: config.SourceWireGuard, Input: path, Enabled: true, MaxActive: 1, MaxInputBytes: 1 << 20,
		PortRange: &config.PortRange{Start: start, End: start},
		Selection: &config.SelectionConfig{Strategy: config.StrategyScore, SwitchConfirmChecks: 1, DeadPairCooldownMinutes: 4, ReplaceAfterFailures: 1},
		WireGuard: &config.WireGuardConfig{EndpointDomainStrategy: "UseIPv4"},
		Balancer:  "x-wg", DialerProxy: "chain-up",
	}
	return c
}

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

// sub is the subscription of source src.
func (c *cascade) sub(src config.SourceConfig) config.SubscriptionConfig {
	return config.SubscriptionConfig{ID: "live", Enabled: true, Sources: []config.SourceConfig{src}, Target: c.tc}
}

// sync runs one sync of source src.
func (c *cascade) sync(t *testing.T, src config.SourceConfig) syncer.Report {
	t.Helper()
	on := config.ResourceToggle{Enabled: true}
	s := &syncer.Syncer{
		Config: config.Loaded{Path: "/live/config.yml", Config: config.AppConfig{
			Subscriptions: []config.SubscriptionConfig{c.sub(src)},
			Resources:     config.ResourceConfig{Outbounds: on, Inbounds: on, Routing: on},
		}},
		Memory: c.mem, Log: slog.New(slog.DiscardHandler),
		Dial: func(string, time.Duration) (xrayapi.Client, error) {
			return cappedClient{Client: c.xc, t: t, max: 1, prefixes: []string{"x-live-wg-", "wg-"}}, nil
		},
	}
	r, err := s.Run(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// tunnel returns the one tunnel of the source in state.
func (c *cascade) tunnel(t *testing.T) topology.Tunnel {
	t.Helper()
	var tns []topology.Tunnel
	c.mem.View(state.Key("live", c.tc.Address), func(tg *state.Target) {
		for _, tn := range tg.Tunnels {
			tns = append(tns, tn.Mapping)
		}
	})
	if len(tns) != 1 {
		t.Fatalf("state has %d tunnels, want 1", len(tns))
	}
	return tns[0]
}

func socksOf(tn topology.Tunnel) string { return net.JoinHostPort(tn.Listen, strconv.Itoa(tn.Port)) }

// TestLiveCascade: on the stock Xray, a WireGuard source whose outbound
// dials through the loopback chain-up (dialerProxy) into balancer x-up of
// the upper stage. With x-up empty the source is applied but carries no
// traffic (the default outbound chain-fail refuses); once the upper-stage
// outbound (freedom redirect) is added the tunnel comes up by itself and
// the upper balancer selects it (observatory alive through the HTTP 204 on
// the peer's TCP port). Checks before the apply stop a source with an
// unknown balancer or a missing dialerProxy without touching Xray.
func TestLiveCascade(t *testing.T) {
	c := newCascade(t)
	if r := c.sync(t, c.src); r.Failed != 0 {
		t.Fatalf("sync: %+v", r)
	}
	socksAddr := socksOf(c.tunnel(t))
	began := time.Now()
	if !failsThroughSOCKS(t, socksAddr) {
		t.Fatal("traffic went through without the upper stage")
	}
	t.Logf("without the upper stage the request failed in %s", time.Since(began).Round(time.Millisecond))
	// The request times out (TCP inside WireGuard waits for a handshake),
	// but every handshake packet is refused at once: chain-up has no
	// selection, so Xray's default chain-fail takes it (access log).
	if !strings.Contains(c.log.String(), "[chain-up >> chain-fail]") {
		t.Fatalf("no chain-up >> chain-fail in the Xray log:\n%s", c.log.String())
	}

	if err := c.xc.AddOutbound(t.Context(), upperStage(t, "x-live-up-1", c.peerPort)); err != nil {
		t.Fatal(err)
	}
	began = time.Now()
	waitThroughSOCKS(t, socksAddr, chainUpWait)
	t.Logf("the tunnel came up %s after the upper stage", time.Since(began).Round(time.Second))

	deadline := time.Now().Add(observeWait)
	for {
		info, err := c.xc.GetBalancerInfo(t.Context(), "x-up")
		if err == nil && slices.Equal(info.PrincipleTargets, []string{"x-live-up-1"}) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("x-up selection %+v, %v", info, err)
		}
		time.Sleep(200 * time.Millisecond)
	}

	before := readState(t, c.xc)
	for _, tc := range []struct {
		mutate func(*config.SourceConfig)
		code   string
	}{
		{func(s *config.SourceConfig) { s.Balancer = "x-nope" }, syncer.CodeBalancerNotFound},
		{func(s *config.SourceConfig) { s.DialerProxy = "chain-missing" }, syncer.CodeDialerProxyMissing},
	} {
		bad := c.src
		tc.mutate(&bad)
		r := c.sync(t, bad)
		if r.Failed == 0 || !strings.HasPrefix(r.Targets[0].Resources[0].Items[0].Message, tc.code+": ") {
			t.Fatalf("%s: %+v", tc.code, r)
		}
		if after := readState(t, c.xc); !reflect.DeepEqual(after, before) {
			t.Fatalf("%s: Xray %+v, before %+v", tc.code, after, before)
		}
	}
	waitThroughSOCKS(t, socksAddr, tunnelWait)
}

// TestLiveCascadeRemoveCloses needs an Xray whose RemoveOutbound closes the
// handler (PR #7112; SCD_XRAY_REMOVE_CLOSES=1): the upper stage is itself a
// WireGuard outbound to a second peer (as WARP under Proton). Removing it
// cuts the lower tunnel, a new upper stage brings it back, and a repair of
// the lower stage with the same key keeps it working.
func TestLiveCascadeRemoveCloses(t *testing.T) {
	if os.Getenv(envRemoveCloses) != "1" {
		t.Skip(envRemoveCloses + "=1 not set: the stock Xray keeps a removed handler's connections (PR #7112)")
	}
	c := newCascade(t)
	// The upper peer redirects everything (the lower stage's UDP and the
	// observatory probes) to the lower peer's port.
	upPeer, upA, upB := wgKey(t), wgKey(t), wgKey(t)
	upPort := freeUDPPort(t, false)
	launchXrayConf(t, "testdata/wireguard-peer", map[string]string{
		"{{WG_PORT}}":             strconv.Itoa(upPort),
		"{{PEER_SECRET_KEY}}":     b64(upPeer.Bytes()),
		"{{CLIENT_A_PUBLIC_KEY}}": b64(upA.PublicKey().Bytes()),
		"{{CLIENT_B_PUBLIC_KEY}}": b64(upB.PublicKey().Bytes()),
		"{{HTTP_ADDR}}":           net.JoinHostPort("127.0.0.1", strconv.Itoa(c.peerPort)),
	})
	upper := func(tag string, k *ecdh.PrivateKey, addr string) *core.OutboundHandlerConfig {
		t.Helper()
		w, err := outbound.ToProto(outbound.BuildWireGuard(candidate.WireGuard{
			Provider: "static", PrivateKey: [32]byte(k.Bytes()),
			Endpoint:  net.JoinHostPort("127.0.0.1", strconv.Itoa(upPort)),
			PublicKey: [32]byte(upPeer.PublicKey().Bytes()),
			Address:   []netip.Prefix{netip.MustParsePrefix(addr)},
			MTU:       1420, AllowedIPs: []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0")},
		}, tag, "UseIPv4", ""))
		if err != nil {
			t.Fatal(err)
		}
		return w
	}
	if r := c.sync(t, c.src); r.Failed != 0 {
		t.Fatalf("sync: %+v", r)
	}
	tn := c.tunnel(t)
	if err := c.xc.AddOutbound(t.Context(), upper("x-live-up-1", upA, "10.2.0.2/32")); err != nil {
		t.Fatal(err)
	}
	waitThroughSOCKS(t, socksOf(tn), chainUpWait)

	if err := c.xc.RemoveOutbound(t.Context(), "x-live-up-1"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for !failsThroughSOCKS(t, socksOf(tn)) {
		if time.Now().After(deadline) {
			t.Fatal("traffic still goes through after the upper stage was removed")
		}
		time.Sleep(time.Second)
	}
	if err := c.xc.AddOutbound(t.Context(), upper("x-live-up-2", upB, "10.2.0.3/32")); err != nil {
		t.Fatal(err)
	}
	waitThroughSOCKS(t, socksOf(tn), chainUpWait)

	d := monitor.Deps{
		Memory: c.mem, Log: slog.New(slog.DiscardHandler),
		Dial: func(string, time.Duration) (xrayapi.Client, error) {
			return sharedClient{Client: c.xc, t: t, tags: []string{tn.OutboundTagInitial, tn.OutboundWithoutPrefix.Tag}}, nil
		},
	}
	if err := monitor.Repair(context.WithoutCancel(t.Context()), d, c.sub(c.src), tn.BaseTunnelID); err != nil {
		t.Fatal(err)
	}
	waitThroughSOCKS(t, socksOf(c.tunnel(t)), chainUpWait)
}
