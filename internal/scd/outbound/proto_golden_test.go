package outbound

import (
	"encoding/base64"
	"encoding/hex"
	"regexp"
	"strings"
	"testing"

	"github.com/xtls/xray-core/app/proxyman"
	"github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/protocol"
	"github.com/xtls/xray-core/common/serial"
	"github.com/xtls/xray-core/core"
	"github.com/xtls/xray-core/proxy/vless"
	vout "github.com/xtls/xray-core/proxy/vless/outbound"
	"github.com/xtls/xray-core/transport/internet"
	"github.com/xtls/xray-core/transport/internet/reality"
	"github.com/xtls/xray-core/transport/internet/tls"
	"github.com/xtls/xray-core/transport/internet/websocket"
	"google.golang.org/protobuf/proto"

	"github.com/a1ekseev/scd/internal/scd/normalize"
	"github.com/a1ekseev/scd/internal/scd/outbound/paramcheck"
)

// vlessHandler is the hand-built reference handler of 09 §6.
func vlessHandler(tag, address string, port uint32, id, flow string, ss *internet.StreamConfig) *core.OutboundHandlerConfig {
	return &core.OutboundHandlerConfig{
		Tag:            tag,
		SenderSettings: serial.ToTypedMessage(&proxyman.SenderConfig{StreamSettings: ss}),
		ProxySettings: serial.ToTypedMessage(&vout.Config{Vnext: &protocol.ServerEndpoint{
			Address: net.NewIPOrDomain(net.ParseAddress(address)),
			Port:    port,
			User: &protocol.User{
				Email:   tag,
				Account: serial.ToTypedMessage(&vless.Account{Id: id, Flow: flow, Encryption: "none"}),
			},
		}}),
	}
}

func TestProtoGolden(t *testing.T) {
	pbk, err := base64.RawURLEncoding.DecodeString("GfXF1r493DSSqCtXMjqIvwiTpV_XPN5mCkj-GwcMjKI")
	if err != nil {
		t.Fatal(err)
	}
	sid, err := hex.DecodeString("abe4a59b9f2407e3")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		line int
		want func(tag string) *core.OutboundHandlerConfig
	}{
		{"tcp-tls 09 §6.1", 63, func(tag string) *core.OutboundHandlerConfig {
			tlsCfg := &tls.Config{NextProtocol: []string{"h2"}}
			return vlessHandler(tag, "de1.example.test", 8443, "00000000-0000-4000-8000-000000000002", "", &internet.StreamConfig{
				ProtocolName:     "tcp",
				SecurityType:     serial.GetMessageType(tlsCfg),
				SecuritySettings: []*serial.TypedMessage{serial.ToTypedMessage(tlsCfg)},
			})
		}},
		{"ws-tls 09 §6.2", 64, func(tag string) *core.OutboundHandlerConfig {
			wsCfg := &websocket.Config{Host: "cdn-1.example.test", Path: "/stream/updates/case-a", Ed: 2048}
			tlsCfg := &tls.Config{ServerName: "cdn.example.test", Fingerprint: "chrome"}
			return vlessHandler(tag, "198.51.100.10", 443, "00000000-0000-4000-8000-000000000003", "", &internet.StreamConfig{
				ProtocolName: "websocket",
				TransportSettings: []*internet.TransportConfig{{
					ProtocolName: "websocket",
					Settings:     serial.ToTypedMessage(wsCfg),
				}},
				SecurityType:     serial.GetMessageType(tlsCfg),
				SecuritySettings: []*serial.TypedMessage{serial.ToTypedMessage(tlsCfg)},
			})
		}},
		{"tcp-reality-vision 09 §6.3", 65, func(tag string) *core.OutboundHandlerConfig {
			rCfg := &reality.Config{
				Fingerprint: "chrome",
				ServerName:  "io.example.test",
				PublicKey:   pbk,
				ShortId:     sid,
				SpiderX:     "/",
				SpiderY:     make([]int64, 10),
			}
			return vlessHandler(tag, "203.0.113.20", 443, "00000000-0000-4000-8000-000000000004", "xtls-rprx-vision", &internet.StreamConfig{
				ProtocolName:     "tcp",
				SecurityType:     serial.GetMessageType(rCfg),
				SecuritySettings: []*serial.TypedMessage{serial.ToTypedMessage(rCfg)},
			})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			n := legacy(t, tc.line)
			got := mustProto(t, n)
			if want := tc.want(n.Tag); !proto.Equal(got, want) {
				t.Fatalf("conf proto differs from the 09 §6 reference:\n got %v\nwant %v", got, want)
			}
		})
	}
}

// marshalProto returns the wire bytes of a handler as a string.
func marshalProto(t *testing.T, h *core.OutboundHandlerConfig) string {
	t.Helper()
	b, err := proto.Marshal(h)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// P21 (01 §17.1): JSON and gRPC payload for ws+tls.
func TestP21WsTLSPayload(t *testing.T) {
	n := legacy(t, 64)
	o := Build(n)
	if o.StreamSettings.Network != "ws" || o.StreamSettings.Security != "tls" {
		t.Fatalf("json network/security %s/%s", o.StreamSettings.Network, o.StreamSettings.Security)
	}
	raw := marshalProto(t, mustProto(t, n))
	if raw == "" || !strings.Contains(raw, "websocket") || !strings.Contains(raw, "xray.transport.internet.tls.Config") {
		t.Fatalf("payload lacks websocket/tls: %q", raw)
	}
}

// P22 (01 §17.1): the REALITY payload uses reality type names and no ws.
func TestP22RealityTypeNames(t *testing.T) {
	raw := marshalProto(t, mustProto(t, legacy(t, 65)))
	if !strings.Contains(raw, "xray.transport.internet.reality.Config") {
		t.Fatalf("payload lacks the reality type: %q", raw)
	}
	if regexp.MustCompile(`\bws\b`).MatchString(raw) || strings.Contains(raw, "websocket") {
		t.Fatalf("reality payload mentions ws: %q", raw)
	}
}

// TestCorpusEveryParamReachesProto: every entry line of the D26 corpus builds
// and every listed parameter is found in the proto (D6, D19, D26).
func TestCorpusEveryParamReachesProto(t *testing.T) {
	lines := fileLines(t, corpusDir+"d26-links.txt")
	entries, checks := 0, 0
	for _, r := range expectRows(t) {
		if r.Expect != "entry" {
			continue
		}
		l := parseRaw(t, lines[r.Line-1], r.Line)
		n := normalizeLink(t, l)
		h, err := ToProto(Build(n))
		if err != nil {
			t.Errorf("line %d: %v", r.Line, err)
			continue
		}
		for _, param := range r.Params {
			value, _ := l.Get(param)
			if err := paramcheck.Check(param, value, h); err != nil {
				t.Errorf("line %d: %s=%q: %v", r.Line, param, value, err)
			}
			checks++
		}
		entries++
	}
	if entries != 73 {
		t.Fatalf("checked %d entries, want 73", entries)
	}
	t.Logf("%d entries, %d parameter checks", entries, checks)
}

// matrixRow is a ✓ row of 11 §3 as a predicate on a normalized link (and
// the flow parameter of its link, for the rows where flow is cleared).
type matrixRow struct {
	row   string
	match func(n normalize.Normalized, linkFlow string) bool
}

func isMLKEM(n normalize.Normalized) bool {
	return strings.HasPrefix(n.Encryption, "mlkem768x25519plus.")
}

var matrixRows = []matrixRow{
	{"3#1", func(n normalize.Normalized, f string) bool {
		return n.Network == "tcp" && n.Security == "tls" && !isMLKEM(n) && f == ""
	}},
	{"3#2", func(n normalize.Normalized, _ string) bool {
		return n.Network == "tcp" && n.Security == "tls" && !isMLKEM(n) && n.Flow != ""
	}},
	{"3#3", func(n normalize.Normalized, f string) bool {
		return n.Network == "tcp" && n.Security == "reality" && !isMLKEM(n) && f == ""
	}},
	{"3#4", func(n normalize.Normalized, _ string) bool {
		return n.Network == "tcp" && n.Security == "reality" && !isMLKEM(n) && n.Flow != ""
	}},
	{"3#5", func(n normalize.Normalized, f string) bool {
		return n.Network == "ws" && n.Security == "tls" && !isMLKEM(n) && f == ""
	}},
	{"3#6", func(n normalize.Normalized, f string) bool {
		return n.Network == "ws" && n.Security == "tls" && !isMLKEM(n) && f != "" && n.Flow == ""
	}},
	{"3#8", func(n normalize.Normalized, f string) bool {
		return n.Network == "xhttp" && n.Security == "tls" && !isMLKEM(n) && f == ""
	}},
	{"3#9", func(n normalize.Normalized, f string) bool {
		return n.Network == "xhttp" && n.Security == "reality" && !isMLKEM(n) && f == ""
	}},
	{"3#10", func(n normalize.Normalized, f string) bool {
		return n.Network == "xhttp" && n.Security != "none" && !isMLKEM(n) && f != "" && n.Flow == ""
	}},
	{"3#11", func(n normalize.Normalized, _ string) bool {
		return n.Security == "none" && isMLKEM(n) && n.Flow == ""
	}},
	{"3#12", func(n normalize.Normalized, _ string) bool {
		return n.Security == "none" && isMLKEM(n) && n.Flow != ""
	}},
	{"3#13", func(n normalize.Normalized, _ string) bool {
		return n.Network != "ws" && n.Security != "none" && isMLKEM(n)
	}},
	{"3#14", func(n normalize.Normalized, _ string) bool {
		return n.Network == "ws" && n.Security == "tls" && isMLKEM(n) && n.Flow != ""
	}},
}

func TestCorpusCoversEveryParam(t *testing.T) {
	covered := map[string]bool{}
	for _, p := range paramcheck.Covered {
		covered[p] = true
	}
	if len(paramcheck.Covered) != 22 || len(covered) != 22 {
		t.Fatalf("Covered has %d items (%d unique), want 22", len(paramcheck.Covered), len(covered))
	}
	lines := fileLines(t, corpusDir+"d26-links.txt")
	seen := map[string]bool{}
	rowsSeen := map[string]bool{}
	for _, r := range expectRows(t) {
		if r.Expect != "entry" {
			continue
		}
		for _, p := range r.Params {
			if !covered[p] {
				t.Errorf("line %d: parameter %q has no paramcheck", r.Line, p)
			}
			seen[p] = true
		}
		l := parseRaw(t, lines[r.Line-1], r.Line)
		n := normalizeLink(t, l)
		flow, _ := l.Get("flow")
		for _, m := range matrixRows {
			if m.match(n, flow) {
				rowsSeen[m.row] = true
			}
		}
	}
	for _, p := range paramcheck.Covered {
		if !seen[p] {
			t.Errorf("covered parameter %q is in no entry line of the corpus", p)
		}
	}
	for _, m := range matrixRows {
		if !rowsSeen[m.row] {
			t.Errorf("matrix row %s (11 §3) has no entry line in the corpus", m.row)
		}
	}
}
