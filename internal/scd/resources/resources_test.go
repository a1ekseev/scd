package resources_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"maps"
	"net"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/xtls/xray-core/app/proxyman"
	"github.com/xtls/xray-core/app/router"
	"github.com/xtls/xray-core/common/serial"
	"github.com/xtls/xray-core/core"
	"github.com/xtls/xray-core/proxy/freedom"
	"github.com/xtls/xray-core/proxy/socks"
	"google.golang.org/protobuf/proto"

	"github.com/a1ekseev/scd/internal/common/inputsrc"
	"github.com/a1ekseev/scd/internal/common/jsonout"
	"github.com/a1ekseev/scd/internal/common/subscription"
	vlesslink "github.com/a1ekseev/scd/internal/common/vless"
	"github.com/a1ekseev/scd/internal/scd/normalize"
	"github.com/a1ekseev/scd/internal/scd/outbound"
	"github.com/a1ekseev/scd/internal/scd/resources"
	"github.com/a1ekseev/scd/internal/scd/topology"
	"github.com/a1ekseev/scd/internal/scd/xrayapi"
	"github.com/a1ekseev/scd/internal/scd/xrayapi/xrayfake"
)

const (
	refDir     = "../../../testdata/reference/scd/status/runtime-state/"
	corpusDir  = "../../../testdata/reference/scd/corpus/"
	realShapes = "../../../docker/fixtures/real-shapes/"
	uuid       = "7d1b6590-1069-4372-92be-8d0a0ae6eaf5"
	pbk        = "OHWtGCfZc0FXULj2ZYeW_vlKIlurak79i6AXun9oJQg"
)

// Test links (example.test hosts, the reference UUID/REALITY key).
const (
	linkTCPTLS     = "vless://" + uuid + "@vpn.example.test:443?type=tcp&security=tls#🇦🇹 Вена, Австрия, Extra"
	linkVision     = "vless://" + uuid + "@vpn.example.test:443?security=reality&encryption=none&fp=chrome&headerType=none&type=tcp&flow=xtls-rprx-vision&sni=io.example.test&pbk=" + pbk + "&sid=cbad7f159438fee4#🇩🇪 Германия, Extra"
	linkRealityNoS = "vless://" + uuid + "@vpn.example.test:443?type=tcp&security=reality&fp=chrome&sni=io.example.test&pbk=" + pbk + "#r"
	linkWS         = "vless://" + uuid + "@198.51.100.10:443?type=ws&security=tls&sni=cdn.example.test&fp=firefox&alpn=h2,http/1.1&host=front.example.test&path=/ws#ws"
	linkWSNoPath   = "vless://" + uuid + "@198.51.100.10:443?type=ws&security=tls&sni=cdn.example.test#ws"
	linkTCPHTTP    = "vless://" + uuid + "@vpn.example.test:443?type=tcp&security=tls&sni=cdn.example.test&headerType=http&host=h.example.test&path=/hidden-a,/hidden-b#h"
	linkXHTTP      = "vless://" + uuid + "@vpn.example.test:443?type=xhttp&security=tls&sni=cdn.example.test&host=x.example.test&path=/xhttp-secret&mode=packet-up#x"
)

func normalizeLine(t *testing.T, sl subscription.Line) normalize.Normalized {
	t.Helper()
	l, skip := vlesslink.Parse(sl)
	if skip != nil {
		t.Fatalf("line %d skipped by the parser: %+v", sl.Number, *skip)
	}
	n, _, skip := normalize.Normalize(l, normalize.Policy{})
	if skip != nil {
		t.Fatalf("line %d skipped by normalize: %+v", sl.Number, *skip)
	}
	return n
}

func normalizeRaw(t *testing.T, raw string) normalize.Normalized {
	t.Helper()
	return normalizeLine(t, subscription.Line{Number: 1, Raw: raw, Trimmed: raw})
}

func toProto(t *testing.T, n normalize.Normalized) *core.OutboundHandlerConfig {
	t.Helper()
	h, err := outbound.ToProto(outbound.Build(n))
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func linkProto(t *testing.T, raw string) *core.OutboundHandlerConfig {
	t.Helper()
	return toProto(t, normalizeRaw(t, raw))
}

func describe(t *testing.T, h *core.OutboundHandlerConfig, includeSecrets bool) map[string]any {
	t.Helper()
	p, ok := resources.DescribeOutbound(h, includeSecrets)
	if !ok {
		t.Fatal("DescribeOutbound: not a VLESS outbound")
	}
	return p
}

// asJSON round-trips v through JSON, as the status server serves it.
func asJSON(t *testing.T, v any) any {
	t.Helper()
	b, err := jsonout.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// refRuntime returns runtime.<kind>[i] of a runtime-state reference.
func refRuntime(t *testing.T, name, kind string, i int) map[string]any {
	t.Helper()
	data, err := os.ReadFile(refDir + name + ".json")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Runtime map[string][]map[string]any `json:"runtime"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	return doc.Runtime[kind][i]
}

func tunnel(listen string) topology.Tunnel {
	return topology.Tunnel{InboundTag: "in-t1", RouteTag: "route-t1", Listen: listen, Port: 20000}
}

func TestInboundSocksShape(t *testing.T) {
	h, err := resources.Inbound(tunnel("127.0.0.1"))
	if err != nil {
		t.Fatal(err)
	}
	if h.Tag != "in-t1" {
		t.Fatalf("tag %q", h.Tag)
	}
	rm, err := h.ReceiverSettings.GetInstance()
	if err != nil {
		t.Fatal(err)
	}
	rc := rm.(*proxyman.ReceiverConfig)
	if r := rc.PortList.Range; len(r) != 1 || r[0].From != 20000 || r[0].To != 20000 {
		t.Fatalf("port list %v", rc.PortList)
	}
	if ip := net.IP(rc.Listen.GetIp()); !ip.Equal(net.IPv4(127, 0, 0, 1)) || len(ip) != 4 {
		t.Fatalf("listen %v", rc.Listen)
	}
	pm, err := h.ProxySettings.GetInstance()
	if err != nil {
		t.Fatal(err)
	}
	sc := pm.(*socks.ServerConfig)
	if !sc.UdpEnabled || sc.AuthType != socks.AuthType_NO_AUTH || len(sc.Accounts) != 0 {
		t.Fatalf("socks %v", sc)
	}
}

func TestInboundListenAllIPv6(t *testing.T) {
	h, err := resources.Inbound(tunnel("::"))
	if err != nil {
		t.Fatal(err)
	}
	rm, err := h.ReceiverSettings.GetInstance()
	if err != nil {
		t.Fatal(err)
	}
	if ip := rm.(*proxyman.ReceiverConfig).Listen.GetIp(); len(ip) != 16 || !net.IP(ip).Equal(net.IPv6unspecified) {
		t.Fatalf("listen ip %v", ip)
	}
}

func TestInboundListenDomain(t *testing.T) {
	h, err := resources.Inbound(tunnel("localhost"))
	if err != nil {
		t.Fatal(err)
	}
	rm, err := h.ReceiverSettings.GetInstance()
	if err != nil {
		t.Fatal(err)
	}
	if d := rm.(*proxyman.ReceiverConfig).Listen.GetDomain(); d != "localhost" {
		t.Fatalf("listen domain %q", d)
	}
	if _, err := resources.Inbound(tunnel("socks.example.test")); err == nil ||
		!strings.Contains(err.Error(), "in-t1") || !strings.Contains(err.Error(), "unable to listen on domain address") {
		t.Fatalf("err %v", err)
	}
}

func TestRuleShape(t *testing.T) {
	got := resources.Rule(tunnel("127.0.0.1"), "x-observe-t1")
	want := &router.RoutingRule{
		TargetTag:  &router.RoutingRule_Tag{Tag: "x-observe-t1"},
		InboundTag: []string{"in-t1"},
		RuleTag:    "route-t1",
	}
	if !proto.Equal(got, want) {
		t.Fatalf("rule %v", got)
	}
}

func TestDescribeOutboundMatchesReference(t *testing.T) {
	vision := normalizeRaw(t, linkVision)
	cases := []struct {
		name           string
		h              *core.OutboundHandlerConfig
		includeSecrets bool
	}{
		{"raw-and-secrets", linkProto(t, linkTCPTLS), true},
		{"redacted-default", linkProto(t, linkTCPTLS), false},
		{"repaired-fallback", linkProto(t, linkTCPTLS), false},
		{"vision-udp443", toProto(t, outbound.ForTarget(vision, vision.Tag, true)), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			i := 0
			if tc.name == "redacted-default" {
				i = 1 // 0 is the fixed outbound
			}
			want := refRuntime(t, tc.name, "outbounds", i)["parsed"]
			if got := asJSON(t, describe(t, tc.h, tc.includeSecrets)); !reflect.DeepEqual(got, want) {
				t.Fatalf("parsed\n got %v\nwant %v", got, want)
			}
		})
	}
}

func TestDescribeOutboundKeysMatchReference(t *testing.T) {
	// The TS ParsedRuntimeOutbound keys (runtime-state contract, D3) plus
	// the additive xhttpMode.
	want := []string{
		"address", "alpn", "encryption", "fingerprint", "flow", "host", "network", "path",
		"port", "protocol", "publicKey", "security", "shortId", "sni", "uuid", "xhttpMode",
	}
	ref := refRuntime(t, "raw-and-secrets", "outbounds", 0)["parsed"].(map[string]any)
	union := map[string]bool{}
	for _, raw := range []string{linkWS, linkVision, linkXHTTP} {
		for k := range describe(t, linkProto(t, raw), true) {
			union[k] = true
		}
	}
	for k := range ref {
		if !union[k] {
			t.Errorf("reference key %q never produced", k)
		}
	}
	if got := slices.Sorted(maps.Keys(union)); !slices.Equal(got, want) {
		t.Fatalf("keys\n got %v\nwant %v", got, want)
	}
}

func TestDescribeRedactsSecrets(t *testing.T) {
	mlkem := normalizeLine(t, corpusLine(t, 14)) // tcp-mlkem, security=none
	cases := []struct {
		name    string
		h       *core.OutboundHandlerConfig
		secrets []string
	}{
		{"reality", linkProto(t, linkVision), []string{uuid, "cbad7f159438fee4"}},
		{"tcp-mlkem", toProto(t, mlkem), []string{mlkem.UUID, lastSegment(mlkem.Encryption)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := describe(t, tc.h, false)
			for _, k := range []string{"uuid", "publicKey", "shortId"} {
				if v, ok := p[k]; ok {
					t.Errorf("%s present: %v", k, v)
				}
			}
			b, err := jsonout.Marshal(p)
			if err != nil {
				t.Fatal(err)
			}
			for _, s := range tc.secrets {
				if strings.Contains(string(b), s) {
					t.Errorf("secret %q leaked: %s", s, b)
				}
			}
			if tc.name == "tcp-mlkem" && p["encryption"] != "mlkem768x25519plus" {
				t.Errorf("encryption %v", p["encryption"])
			}
			full := describe(t, tc.h, true)
			if full["uuid"] == nil {
				t.Errorf("uuid missing with includeSecrets: %v", full)
			}
		})
	}
}

// lastSegment returns the key (last dot-separated part) of an ML-KEM
// encryption value.
func lastSegment(enc string) string {
	return enc[strings.LastIndex(enc, ".")+1:]
}

func corpusLine(t *testing.T, n int) subscription.Line {
	t.Helper()
	data, err := os.ReadFile(corpusDir + "d26-links.txt")
	if err != nil {
		t.Fatal(err)
	}
	raw := strings.Split(string(data), "\n")[n-1]
	return subscription.Line{Number: n, Raw: raw, Trimmed: strings.TrimSpace(raw)}
}

func TestDescribeRealityKeyFormats(t *testing.T) {
	key, err := base64.RawURLEncoding.DecodeString(pbk)
	if err != nil {
		t.Fatal(err)
	}
	p := describe(t, linkProto(t, linkVision), true)
	if want := base64.StdEncoding.EncodeToString(key); p["publicKey"] != want || !strings.HasSuffix(want, "=") {
		t.Errorf("publicKey %v, want %s", p["publicKey"], want)
	}
	if p["shortId"] != "cbad7f159438fee4" {
		t.Errorf("shortId %v", p["shortId"])
	}
	if a, ok := p["alpn"].([]string); !ok || len(a) != 0 {
		t.Errorf("alpn %#v", p["alpn"])
	}
	if p := describe(t, linkProto(t, linkRealityNoS), true); p["shortId"] != "0000000000000000" {
		t.Errorf("empty sid: shortId %v", p["shortId"])
	}
}

func TestDescribeOutboundRedactsXHTTPPath(t *testing.T) {
	data, err := os.ReadFile(realShapes + "remnawave.txt")
	if err != nil {
		t.Fatal(err)
	}
	content, _ := inputsrc.DecodeAuto(string(data))
	n := normalizeLine(t, subscription.ScanLines(content)[0])
	if n.Network != "xhttp" || n.Path == "" || !strings.HasPrefix(n.Encryption, "mlkem768x25519plus.") {
		t.Fatalf("fixture line 1: network=%q path set=%v", n.Network, n.Path != "")
	}
	h := toProto(t, n)

	p := describe(t, h, false)
	if p["path"] != "<redacted>" || p["encryption"] != "mlkem768x25519plus" {
		t.Fatalf("path=%v encryption=%v", p["path"], p["encryption"])
	}
	b, err := jsonout.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{n.Path, lastSegment(n.Encryption)} {
		if strings.Contains(string(b), s) {
			t.Fatalf("secret leaked: %s", b)
		}
	}

	full := describe(t, h, true)
	if full["path"] != n.Path {
		t.Fatal("includeSecrets: path differs from the link")
	}

	t.Run("xhttp-host", func(t *testing.T) {
		h := linkProto(t, linkXHTTP)
		p := describe(t, h, false)
		if p["host"] != "<redacted>" || p["path"] != "<redacted>" || p["xhttpMode"] != "packet-up" {
			t.Errorf("host=%v path=%v mode=%v", p["host"], p["path"], p["xhttpMode"])
		}
		if b, _ := jsonout.Marshal(p); strings.Contains(string(b), "x.example.test") || strings.Contains(string(b), "xhttp-secret") {
			t.Errorf("leaked: %s", b)
		}
	})

	for _, tc := range []struct {
		name, link, path string
	}{
		{"ws", linkWS, "/ws"},
		{"tcp-http-header", linkTCPHTTP, "/hidden-a,/hidden-b"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := linkProto(t, tc.link)
			if p := describe(t, h, false); p["path"] != "<redacted>" {
				t.Errorf("path %v", p["path"])
			}
			if p := describe(t, h, true); p["path"] != tc.path {
				t.Errorf("includeSecrets path %v", p["path"])
			}
		})
	}
	t.Run("ws-empty-path", func(t *testing.T) {
		if v, ok := describe(t, linkProto(t, linkWSNoPath), false)["path"]; ok {
			t.Errorf("path %v", v)
		}
	})
}

func TestDescribeXHTTPAndMLKEM(t *testing.T) {
	p := describe(t, linkProto(t, linkXHTTP), true)
	want := map[string]any{
		"protocol": "vless", "address": "vpn.example.test", "port": 443, "uuid": uuid,
		"encryption": "none", "network": "splithttp", "security": "tls", "sni": "cdn.example.test",
		"alpn": []string{}, "host": "x.example.test", "path": "/xhttp-secret", "xhttpMode": "packet-up",
	}
	if !reflect.DeepEqual(p, want) {
		t.Fatalf("xhttp\n got %#v\nwant %#v", p, want)
	}

	mlkem := normalizeLine(t, corpusLine(t, 14))
	p = describe(t, toProto(t, mlkem), true)
	for _, k := range []string{"security", "sni", "fingerprint", "alpn"} {
		if v, ok := p[k]; ok {
			t.Errorf("tcp-mlkem: %s = %v", k, v)
		}
	}
	if p["network"] != "tcp" || p["encryption"] == "" || strings.HasPrefix(p["encryption"].(string), "mlkem768x25519plus") {
		t.Errorf("tcp-mlkem: %v", p)
	}

	ws := describe(t, linkProto(t, linkWS), true)
	wantWS := map[string]any{
		"protocol": "vless", "address": "198.51.100.10", "port": 443, "uuid": uuid,
		"encryption": "none", "network": "websocket", "security": "tls", "sni": "cdn.example.test",
		"alpn": []string{"h2", "http/1.1"}, "fingerprint": "firefox", "host": "front.example.test", "path": "/ws",
	}
	if !reflect.DeepEqual(ws, wantWS) {
		t.Fatalf("ws\n got %#v\nwant %#v", ws, wantWS)
	}
}

func TestDescribeNonVlessReturnsFalse(t *testing.T) {
	h := &core.OutboundHandlerConfig{
		Tag:            "direct",
		SenderSettings: serial.ToTypedMessage(&proxyman.SenderConfig{}),
		ProxySettings:  serial.ToTypedMessage(&freedom.Config{}),
	}
	if p, ok := resources.DescribeOutbound(h, true); ok || p != nil {
		t.Fatalf("freedom described: %v", p)
	}
	if _, ok := resources.DescribeOutbound(&core.OutboundHandlerConfig{Tag: "x"}, true); ok {
		t.Fatal("empty handler described")
	}
	in := &core.InboundHandlerConfig{Tag: "x", ReceiverSettings: serial.ToTypedMessage(&proxyman.ReceiverConfig{}), ProxySettings: serial.ToTypedMessage(&freedom.Config{})}
	if _, ok := resources.DescribeInbound(in); ok {
		t.Fatal("non-socks inbound described")
	}
}

// TestDescribeAfterXrayRoundTrip adds the resources to the fake Xray and
// describes what List* returns, as runtime-state does.
func TestDescribeAfterXrayRoundTrip(t *testing.T) {
	_, c := xrayfake.Start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tn := tunnel("127.0.0.1")
	in, err := resources.Inbound(tn)
	if err != nil {
		t.Fatal(err)
	}
	out := linkProto(t, linkTCPTLS)
	out.Tag = "x-observe-t1"
	for _, err := range []error{
		c.AddOutbound(ctx, out),
		c.AddInbound(ctx, in),
		c.AddRule(ctx, resources.Rule(tn, out.Tag)),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}

	outs, err := c.ListOutbounds(ctx)
	if err != nil || len(outs) != 1 {
		t.Fatalf("outbounds %v %v", outs, err)
	}
	want := refRuntime(t, "redacted-default", "outbounds", 1)["parsed"]
	if got := asJSON(t, describe(t, outs[0], false)); !reflect.DeepEqual(got, want) {
		t.Errorf("outbound parsed %v", got)
	}

	ins, err := c.ListInbounds(ctx)
	if err != nil || len(ins) != 1 {
		t.Fatalf("inbounds %v %v", ins, err)
	}
	p, ok := resources.DescribeInbound(ins[0])
	if !ok {
		t.Fatal("inbound not described")
	}
	if got, want := asJSON(t, p), refRuntime(t, "redacted-default", "inbounds", 0)["parsed"]; !reflect.DeepEqual(got, want) {
		t.Errorf("inbound parsed\n got %v\nwant %v", got, want)
	}

	rules, err := c.ListRules(ctx)
	if err != nil || len(rules) != 1 {
		t.Fatalf("rules %v %v", rules, err)
	}
	// ListRule carries no inbound tags (02 §1.5): inboundTags is always [].
	wantRule := map[string]any{"ruleTag": "route-t1", "outboundTag": "x-observe-t1", "inboundTags": []any{}}
	if got := asJSON(t, resources.DescribeRule(rules[0])); !reflect.DeepEqual(got, wantRule) {
		t.Errorf("rule parsed %v", got)
	}
}

func TestDescribeRuleBalancer(t *testing.T) {
	got := resources.DescribeRule(xrayapi.Rule{RuleTag: "x-balancer"})
	want := map[string]any{"ruleTag": "x-balancer", "outboundTag": "", "inboundTags": []string{}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%#v", got)
	}
}

func TestRawBase64(t *testing.T) {
	h := linkProto(t, linkTCPTLS)
	raw := resources.RawBase64(h)
	if raw != resources.RawBase64(proto.Clone(h)) {
		t.Fatal("not deterministic")
	}
	b, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		t.Fatal(err)
	}
	var back core.OutboundHandlerConfig
	if err := proto.Unmarshal(b, &back); err != nil || !proto.Equal(&back, h) {
		t.Fatalf("round trip: %v", err)
	}
}
