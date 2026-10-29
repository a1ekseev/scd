package candidate

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"
)

// masqueRequired are the query parameters every MASQUE provider requires.
var masqueRequired = []string{"peerkey", "address", "sni", "transport", "dns", "priority"}

// masqueParams are the known MASQUE query parameters in format order.
var masqueParams = slices.Concat([]string{
	"provider", "peerkey", "address", "sni", "transport", "dns", "priority",
}, metaParams)

// MASQUE transports.
const (
	// TransportH3 is HTTP/3 over UDP.
	TransportH3 = "h3"
	// TransportH2 is HTTP/2 over TCP.
	TransportH2 = "h2"
)

// keyEncoding is the encoding of the keys in a "masque://" line.
var keyEncoding = base64.RawURLEncoding.Strict()

// Masque is a parsed "masque://" candidate line.
type Masque struct {
	// Provider is a known provider name.
	Provider string
	// PrivateKey is the ECDSA P-256 device private key (secret).
	PrivateKey *ecdsa.PrivateKey
	// Endpoint is the peer IP and port.
	Endpoint netip.AddrPort
	// PeerKey is the ECDSA P-256 endpoint public key.
	PeerKey *ecdsa.PublicKey
	// Address are the device addresses, at most one IPv4 and one IPv6; a
	// bare IP is a full-length prefix.
	Address []netip.Prefix
	// SNI is the TLS server name: a hostname, or the endpoint IP when no SNI
	// is sent.
	SNI string
	// Transport is TransportH3 or TransportH2.
	Transport string
	// DNS are the remote DNS servers.
	DNS []netip.Addr
	// Priority is the transport priority, 0 is the highest. It is metadata.
	Priority int
	// Label is the percent-decoded fragment ("" without one).
	Label string
	// Meta is the public metadata.
	Meta Meta
}

// ParseMasque parses a "masque://" candidate line. Any error is a
// *FieldError.
func ParseMasque(line string) (Masque, error) {
	var m Masque
	rest, frag, _ := strings.Cut(line, "#")
	u, err := url.Parse(rest) // its error text holds the input, so it is dropped
	if err != nil || u.Scheme != string(KindMasque) || u.Opaque != "" || u.Path != "" || u.User == nil {
		return m, fieldErr("uri", "not a masque:// URI")
	}
	label, err := url.PathUnescape(frag)
	if err != nil {
		return m, fieldErr("label", "invalid percent-encoding")
	}
	m.Label = strings.ToValidUTF8(label, "�")
	if _, set := u.User.Password(); set {
		return m, fieldErr("privatekey", "must not have a password")
	}
	if m.PrivateKey, err = parsePrivateKey(u.User.Username()); err != nil {
		return m, fieldErr("privatekey", err.Error())
	}
	if m.Endpoint, err = parseAddrPort(u.Host); err != nil {
		return m, fieldErr("endpoint", err.Error())
	}
	q, err := parseQuery(u.RawQuery, masqueParams)
	if err != nil {
		return m, err
	}
	if m.Provider, err = checkProvider(KindMasque, q, masqueRequired); err != nil {
		return m, err
	}
	for _, k := range masqueParams[1:] {
		if v, ok := q[k]; ok {
			if err := m.set(k, v); err != nil {
				return Masque{}, fieldErr(k, err.Error())
			}
		}
	}
	return m, nil
}

func (m *Masque) set(k, v string) error {
	var err error
	switch k {
	case "peerkey":
		m.PeerKey, err = parsePublicKey(v)
	case "address":
		m.Address, err = parseList(v, func(s string) (netip.Prefix, error) {
			if strings.Contains(s, "/") {
				return netip.ParsePrefix(s)
			}
			a, err := parseIP(s)
			return netip.PrefixFrom(a, a.BitLen()), err
		}, "each must be an IP or CIDR")
		v4 := 0
		for _, p := range m.Address {
			if p.Addr().Unmap().Is4() { // Xray counts an IPv4-mapped IPv6 as IPv4
				v4++
			}
		}
		if err == nil && (v4 > 1 || len(m.Address)-v4 > 1) {
			err = rule("at most one IPv4 and one IPv6")
		}
	case "sni":
		if a, err := parseIP(v); err == nil {
			if a != m.Endpoint.Addr() {
				return rule("an IP must be the endpoint IP")
			}
			m.SNI = a.String()
		} else if isHostname(v) {
			m.SNI = v
		} else {
			return rule("must be a hostname or the endpoint IP")
		}
	case "transport":
		if v != TransportH3 && v != TransportH2 {
			return rule("must be h3 or h2")
		}
		m.Transport = v
	case "dns":
		m.DNS, err = parseList(v, parseIP, "each must be an IP")
	case "priority":
		if m.Priority, err = parseUint(v, 0, math.MaxInt32); err != nil {
			return rule("must be an integer >= 0")
		}
	default:
		return m.Meta.set(k, v)
	}
	return err
}

// parsePrivateKey decodes a base64url PKCS#8 ECDSA P-256 private key.
func parsePrivateKey(s string) (*ecdsa.PrivateKey, error) {
	b, err := keyEncoding.DecodeString(s)
	if err != nil {
		return nil, rule("must be base64url")
	}
	k, err := x509.ParsePKCS8PrivateKey(b)
	if ec, ok := k.(*ecdsa.PrivateKey); err == nil && ok && ec.Curve == elliptic.P256() {
		return ec, nil
	}
	return nil, rule("must be a PKCS#8 ECDSA P-256 key")
}

// parsePublicKey decodes a base64url PKIX ECDSA P-256 public key.
func parsePublicKey(s string) (*ecdsa.PublicKey, error) {
	b, err := keyEncoding.DecodeString(s)
	if err != nil {
		return nil, rule("must be base64url")
	}
	k, err := x509.ParsePKIXPublicKey(b)
	if ec, ok := k.(*ecdsa.PublicKey); err == nil && ok && ec.Curve == elliptic.P256() {
		return ec, nil
	}
	return nil, rule("must be a PKIX ECDSA P-256 key")
}

// parseAddrPort parses "ip:port" with the IPv6 in brackets and without a
// zone.
func parseAddrPort(hostport string) (netip.AddrPort, error) {
	host, port, err := net.SplitHostPort(hostport)
	if err != nil {
		return netip.AddrPort{}, rule("must be IP:port, IPv6 in brackets")
	}
	p, err := parseUint(port, 1, 65535)
	if err != nil {
		return netip.AddrPort{}, rule("port must be 1..65535")
	}
	a, err := parseIP(host)
	if err != nil {
		return netip.AddrPort{}, rule("host must be an IP")
	}
	return netip.AddrPortFrom(a, uint16(p)), nil
}

// mustPKIX returns the base64url PKIX of k, a P-256 key.
func mustPKIX(k *ecdsa.PublicKey) string {
	b, err := x509.MarshalPKIXPublicKey(k)
	if err != nil {
		panic(err) // unreachable: a P-256 key always marshals
	}
	return keyEncoding.EncodeToString(b)
}

// tunnelParams are the public tunnel parameters in format order.
func (m Masque) tunnelParams() []kv {
	address := make([]string, len(m.Address))
	for i, p := range m.Address {
		address[i] = p.String()
	}
	dns := make([]string, len(m.DNS))
	for i, a := range m.DNS {
		dns[i] = a.String()
	}
	return []kv{
		{"peerkey", mustPKIX(m.PeerKey)},
		{"address", strings.Join(address, ",")},
		{"sni", m.SNI},
		{"transport", m.Transport},
		{"dns", strings.Join(dns, ",")},
	}
}

// FormatMasque formats m as a candidate line with parameters in a fixed
// order; empty metadata is not written. m needs both keys.
func FormatMasque(m Masque) string {
	b, err := x509.MarshalPKCS8PrivateKey(m.PrivateKey)
	if err != nil {
		panic(err) // unreachable: a P-256 key always marshals
	}
	return m.format(keyEncoding.EncodeToString(b))
}

// Redacted returns m as FormatMasque does, with "redacted" in place of the
// private key: the line as scd shows it.
func (m Masque) Redacted() string { return m.format("redacted") }

func (m Masque) format(privateKey string) string {
	params := slices.Concat([]kv{{"provider", m.Provider}}, m.tunnelParams(),
		[]kv{{"priority", strconv.Itoa(m.Priority)}}, m.Meta.params())
	return formatLine(KindMasque, privateKey, m.Endpoint.String(), params, m.Label)
}

// Format implements fmt.Formatter for every verb: the Redacted line, so
// that no fmt verb prints the key.
func (m Masque) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, m.Redacted()) }

// LogValue implements slog.LogValuer: the Redacted line.
func (m Masque) LogValue() slog.Value { return slog.StringValue(m.Redacted()) }

// PublicHash returns the sha8 (first 8 hex characters of SHA-256) of the
// public tunnel fields: the line with the PKIX of the device public key as
// userinfo, without provider, priority, metadata and label. Any tunnel
// change gives a new hash.
func (m Masque) PublicHash() string {
	line := formatLine(KindMasque, mustPKIX(&m.PrivateKey.PublicKey), m.Endpoint.String(), m.tunnelParams(), "")
	sum := sha256.Sum256([]byte(line))
	return hex.EncodeToString(sum[:])[:8]
}
