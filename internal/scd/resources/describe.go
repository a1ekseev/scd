package resources

import (
	"encoding/base64"
	"encoding/hex"
	"net"
	"strconv"
	"strings"

	"github.com/xtls/xray-core/app/proxyman"
	xnet "github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/serial"
	"github.com/xtls/xray-core/core"
	pmasque "github.com/xtls/xray-core/proxy/masque"
	"github.com/xtls/xray-core/proxy/socks"
	"github.com/xtls/xray-core/proxy/vless"
	vout "github.com/xtls/xray-core/proxy/vless/outbound"
	"github.com/xtls/xray-core/proxy/wireguard"
	"github.com/xtls/xray-core/transport/internet"
	"github.com/xtls/xray-core/transport/internet/headers/http"
	"github.com/xtls/xray-core/transport/internet/masque"
	"github.com/xtls/xray-core/transport/internet/reality"
	"github.com/xtls/xray-core/transport/internet/splithttp"
	"github.com/xtls/xray-core/transport/internet/tcp"
	"github.com/xtls/xray-core/transport/internet/tls"
	"github.com/xtls/xray-core/transport/internet/websocket"
	"google.golang.org/protobuf/proto"

	"github.com/a1ekseev/scd/internal/scd/xrayapi"
)

const (
	// redacted replaces a secret-like value when secrets are hidden (D33).
	redacted = "<redacted>"
	// mlkemEncryption replaces an ML-KEM key when secrets are hidden.
	mlkemEncryption = "mlkem768x25519plus"
)

// instance decodes tm into T; false when tm is nil, unknown or another type.
func instance[T proto.Message](tm *serial.TypedMessage) (T, bool) {
	var zero T
	if tm == nil {
		return zero, false
	}
	m, err := tm.GetInstance()
	if err != nil {
		return zero, false
	}
	v, ok := m.(T)
	return v, ok
}

// setString sets p[key] = v unless v is empty.
func setString(p map[string]any, key, v string) {
	if v != "" {
		p[key] = v
	}
}

// setSecret sets p[key] like setString, but to "<redacted>" unless
// includeSecrets.
func setSecret(p map[string]any, key, v string, includeSecrets bool) {
	if v != "" && !includeSecrets {
		v = redacted
	}
	setString(p, key, v)
}

// address formats an IPOrDomain: an IP in Go notation (IPv6 unbracketed),
// otherwise the domain; "" when unset.
func address(a *xnet.IPOrDomain) string {
	if ip := a.GetIp(); len(ip) > 0 {
		return net.IP(ip).String()
	}
	return a.GetDomain()
}

// DescribeOutbound returns the runtime-state "parsed" object of a VLESS
// outbound (SenderConfig + vless outbound Config), a WireGuard outbound
// (describeWireGuard) or a MASQUE outbound (describeMasque), false for any other outbound. Keys are those of the TS runtime-state (protocol, address, port,
// uuid, flow, encryption, network, security, sni, alpn, fingerprint, host,
// path, publicKey, shortId) plus xhttpMode; empty values are omitted, alpn is
// always present ([] when unset) unless security is none. Without
// includeSecrets uuid, publicKey and shortId are dropped, an ML-KEM key is
// shown as "mlkem768x25519plus", and the transport path (ws, xhttp, raw
// http header) and the xhttp host become "<redacted>" (D33).
func DescribeOutbound(h *core.OutboundHandlerConfig, includeSecrets bool) (map[string]any, bool) {
	if h == nil {
		return nil, false
	}
	sender, ok := instance[*proxyman.SenderConfig](h.SenderSettings)
	if !ok {
		return nil, false
	}
	if wg, ok := instance[*wireguard.DeviceConfig](h.ProxySettings); ok {
		return describeWireGuard(wg, sender), true
	}
	if mq, ok := instance[*pmasque.ClientConfig](h.ProxySettings); ok {
		return describeMasque(mq, sender), true
	}
	cfg, ok := instance[*vout.Config](h.ProxySettings)
	if !ok {
		return nil, false
	}
	stream, server := sender.GetStreamSettings(), cfg.GetVnext()
	if stream == nil || server == nil {
		return nil, false
	}

	p := map[string]any{"protocol": "vless"}
	setString(p, "address", address(server.Address))
	if server.Port != 0 {
		p["port"] = int(server.Port)
	}
	if acc, ok := instance[*vless.Account](server.GetUser().GetAccount()); ok {
		if includeSecrets {
			setString(p, "uuid", acc.Id)
		}
		setString(p, "flow", acc.Flow)
		enc := acc.Encryption
		if !includeSecrets && enc != "" && enc != "none" {
			enc = mlkemEncryption
		}
		setString(p, "encryption", enc)
	}
	setString(p, "network", stream.ProtocolName)
	describeTransport(p, stream, includeSecrets)
	describeSecurity(p, stream, includeSecrets)
	return p, true
}

// HoldsPrivateKey reports whether h is a WireGuard or MASQUE outbound,
// whose proto holds a private key: runtime-state never shows its
// rawBase64.
func HoldsPrivateKey(h *core.OutboundHandlerConfig) bool {
	_, wg := instance[*wireguard.DeviceConfig](h.GetProxySettings())
	_, mq := instance[*pmasque.ClientConfig](h.GetProxySettings())
	return wg || mq
}

// describeMasque is the "parsed" object of a MASQUE outbound: protocol,
// endpoint, publicKey (the endpoint's, std base64 PKIX DER), address (the
// device addresses), remoteDNS, network, security, sni and alpn. The device
// private key is never shown, whatever includeSecrets.
func describeMasque(mq *pmasque.ClientConfig, sender *proxyman.SenderConfig) map[string]any {
	p := map[string]any{"protocol": "masque"}
	if s := mq.GetServer(); s != nil {
		p["endpoint"] = net.JoinHostPort(address(s.Address), strconv.Itoa(int(s.Port)))
	}
	if len(mq.RemoteDns) > 0 {
		p["remoteDNS"] = mq.RemoteDns
	}
	stream := sender.GetStreamSettings()
	setString(p, "network", stream.GetProtocolName())
	for _, tc := range stream.GetTransportSettings() {
		if c, ok := instance[*masque.Config](tc.Settings); ok {
			if d := c.GetWarp(); d != nil {
				p["publicKey"] = base64.StdEncoding.EncodeToString(d.PublicKey)
				p["address"] = d.Address
			}
		}
	}
	if stream != nil {
		describeSecurity(p, stream, false)
	}
	return p
}

// describeWireGuard is the "parsed" object of a WireGuard outbound:
// protocol, endpoint, publicKey (the peer's), address, mtu, remoteDNS,
// noKernelTun and domainStrategy. The private and preshared keys are never
// shown, whatever includeSecrets.
func describeWireGuard(wg *wireguard.DeviceConfig, sender *proxyman.SenderConfig) map[string]any {
	p := map[string]any{"protocol": "wireguard", "noKernelTun": wg.NoKernelTun}
	if peers := wg.GetPeers(); len(peers) > 0 {
		setString(p, "endpoint", peers[0].Endpoint)
		if k, err := hex.DecodeString(peers[0].PublicKey); err == nil {
			p["publicKey"] = base64.StdEncoding.EncodeToString(k)
		}
	}
	if len(wg.Endpoint) > 0 {
		p["address"] = wg.Endpoint
	}
	if wg.Mtu != 0 {
		p["mtu"] = int(wg.Mtu)
	}
	if len(wg.DNS) > 0 {
		p["remoteDNS"] = wg.DNS
	}
	if ds := sender.GetStreamSettings().GetSocketSettings().GetDomainStrategy(); ds != internet.DomainStrategy_AS_IS {
		p["domainStrategy"] = ds.String()
	}
	return p
}

// describeTransport adds host, path and xhttpMode of the stream's transport.
func describeTransport(p map[string]any, stream *internet.StreamConfig, includeSecrets bool) {
	for _, tc := range stream.TransportSettings {
		if tc.ProtocolName != stream.ProtocolName {
			continue
		}
		switch tc.ProtocolName {
		case "websocket":
			if ws, ok := instance[*websocket.Config](tc.Settings); ok {
				setString(p, "host", ws.Host)
				setSecret(p, "path", ws.Path, includeSecrets)
			}
		case "splithttp":
			if x, ok := instance[*splithttp.Config](tc.Settings); ok {
				setSecret(p, "host", x.Host, includeSecrets)
				setSecret(p, "path", x.Path, includeSecrets)
				setString(p, "xhttpMode", x.Mode)
			}
		case "tcp":
			if c, ok := instance[*tcp.Config](tc.Settings); ok {
				if hc, ok := instance[*http.Config](c.HeaderSettings); ok {
					setSecret(p, "path", strings.Join(hc.GetRequest().GetUri(), ","), includeSecrets)
				}
			}
		}
	}
}

// describeSecurity adds security, sni, alpn, fingerprint and the REALITY
// publicKey (std base64) and shortId (hex).
func describeSecurity(p map[string]any, stream *internet.StreamConfig, includeSecrets bool) {
	switch stream.SecurityType {
	case serial.GetMessageType(&tls.Config{}):
		p["security"] = "tls"
		alpn := []string{}
		if c, ok := findSecurity[*tls.Config](stream); ok {
			setString(p, "sni", c.ServerName)
			setString(p, "fingerprint", c.Fingerprint)
			alpn = append(alpn, c.NextProtocol...)
		}
		p["alpn"] = alpn
	case serial.GetMessageType(&reality.Config{}):
		p["security"] = "reality"
		p["alpn"] = []string{}
		if c, ok := findSecurity[*reality.Config](stream); ok {
			setString(p, "sni", c.ServerName)
			setString(p, "fingerprint", c.Fingerprint)
			if includeSecrets && len(c.PublicKey) > 0 {
				p["publicKey"] = base64.StdEncoding.EncodeToString(c.PublicKey)
			}
			if includeSecrets && len(c.ShortId) > 0 {
				p["shortId"] = hex.EncodeToString(c.ShortId)
			}
		}
	}
}

// findSecurity returns the first security setting of type T.
func findSecurity[T proto.Message](stream *internet.StreamConfig) (T, bool) {
	for _, tm := range stream.SecuritySettings {
		if v, ok := instance[T](tm); ok {
			return v, true
		}
	}
	var zero T
	return zero, false
}

// DescribeInbound returns the runtime-state "parsed" object of a SOCKS
// inbound (ReceiverConfig + socks ServerConfig): protocol, listen,
// portStart, portEnd (first port range) and udp; false for any other
// inbound.
func DescribeInbound(h *core.InboundHandlerConfig) (map[string]any, bool) {
	if h == nil {
		return nil, false
	}
	recv, ok := instance[*proxyman.ReceiverConfig](h.ReceiverSettings)
	if !ok {
		return nil, false
	}
	sc, ok := instance[*socks.ServerConfig](h.ProxySettings)
	if !ok {
		return nil, false
	}
	p := map[string]any{"protocol": "socks"}
	setString(p, "listen", address(recv.Listen))
	if r := recv.GetPortList().GetRange(); len(r) > 0 {
		p["portStart"] = int(r[0].From)
		p["portEnd"] = int(r[0].To)
	}
	p["udp"] = sc.UdpEnabled
	return p, true
}

// DescribeRule returns the runtime-state "parsed" object of a ListRule
// item. ListRule carries no inbound tags, so inboundTags is always [].
func DescribeRule(r xrayapi.Rule) map[string]any {
	return map[string]any{"ruleTag": r.RuleTag, "outboundTag": r.Tag, "inboundTags": []string{}}
}

// RawBase64 returns the deterministic protobuf encoding of m in standard
// base64 (runtime-state rawBase64), or "" if m cannot be marshalled.
func RawBase64(m proto.Message) string {
	b, err := proto.MarshalOptions{Deterministic: true}.Marshal(m)
	if err != nil {
		return ""
	}
	return base64.StdEncoding.EncodeToString(b)
}
