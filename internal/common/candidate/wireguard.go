// Package candidate parses and formats the tunnel candidate lines that scd
// reads from local candidate files ("wireguard://key@host:port?query#label"
// and "masque://key@ip:port?query#label").
// Parsing is strict and error texts never contain field values, because the
// lines carry private keys.
package candidate

import (
	"crypto/ecdh"
	"crypto/sha256"
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
	"time"
	"unicode"
	"unicode/utf8"
)

// Kind is the tunnel kind of a candidate line.
type Kind string

// Candidate kinds.
const (
	// KindWireGuard is a "wireguard://" line.
	KindWireGuard Kind = "wireguard"
	// KindMasque is a "masque://" line.
	KindMasque Kind = "masque"
)

// providerFields lists, per kind and provider, the query parameters the
// provider requires on top of the ones every provider of the kind requires.
// It is the only place in scd that knows provider names: a new provider is a
// new row here.
var providerFields = map[Kind]map[string][]string{
	KindWireGuard: {
		"proton": {"dns"},
		"warp":   {"dns", "reserved"},
		"static": {},
	},
	KindMasque: {
		"warp": {},
	},
}

// wgRequired are the query parameters every provider requires.
var wgRequired = []string{"publickey", "address", "mtu", "allowedips"}

// metaParams are the metadata query parameters in format order.
var metaParams = []string{"server", "entry", "exit", "load", "score", "expires"}

// wgParams are the known query parameters in format order.
var wgParams = slices.Concat([]string{
	"provider", "publickey", "address", "dns", "mtu", "allowedips", "reserved", "keepalive", "presharedkey",
}, metaParams)

// FieldError is a candidate line error. It names the field and the broken
// rule, never the value.
type FieldError struct {
	// Field is a parameter name or one of "uri", "privatekey", "endpoint",
	// "label".
	Field string
	// Rule is the broken rule.
	Rule string
}

// Error implements error.
func (e *FieldError) Error() string {
	return "candidate field " + e.Field + ": " + e.Rule
}

// Meta is the public candidate metadata. It is not part of PublicHash.
type Meta struct {
	// Server is the server name ("" without one).
	Server string
	// Entry and Exit are the ISO2 entry and exit countries ("" without one).
	Entry, Exit string
	// Load is the server load 0..100 (nil without one).
	Load *int
	// Score is the provider score, lower is better (nil without one).
	Score *float64
	// Expires is the certificate expiry in UTC (zero without one).
	Expires time.Time
}

// WireGuard is a parsed "wireguard://" candidate line.
type WireGuard struct {
	// Provider is a known provider name.
	Provider string
	// PrivateKey is the interface private key (secret).
	PrivateKey [32]byte
	// Endpoint is the peer "host:port", IPv6 in brackets.
	Endpoint string
	// PublicKey is the peer public key.
	PublicKey [32]byte
	// Address are the interface addresses; a bare IP is a full-length prefix.
	Address []netip.Prefix
	// DNS are the remote DNS servers (nil without them).
	DNS []netip.Addr
	// MTU is in 576..1500.
	MTU int
	// AllowedIPs are the peer allowed IPs.
	AllowedIPs []netip.Prefix
	// Reserved are the 3 reserved bytes (nil without them).
	Reserved *[3]byte
	// Keepalive is the peer keepalive in seconds, 0 for none.
	Keepalive int
	// PresharedKey is the peer preshared key (secret, nil without one).
	PresharedKey *[32]byte
	// Label is the percent-decoded fragment ("" without one).
	Label string
	// Meta is the public metadata.
	Meta Meta
}

func fieldErr(field, rule string) error { return &FieldError{Field: field, Rule: rule} }

// ParseWireGuard parses a "wireguard://" candidate line. Any error is a
// *FieldError.
func ParseWireGuard(line string) (WireGuard, error) {
	var w WireGuard
	rest, frag, _ := strings.Cut(line, "#")
	u, err := url.Parse(rest) // its error text holds the input, so it is dropped
	if err != nil || u.Scheme != string(KindWireGuard) || u.Opaque != "" || u.Path != "" || u.User == nil {
		return w, fieldErr("uri", "not a wireguard:// URI")
	}
	label, err := url.PathUnescape(frag)
	if err != nil {
		return w, fieldErr("label", "invalid percent-encoding")
	}
	w.Label = strings.ToValidUTF8(label, "�")
	if _, set := u.User.Password(); set {
		return w, fieldErr("privatekey", "must not have a password")
	}
	if err := parseKey(u.User.Username(), &w.PrivateKey); err != nil {
		return w, fieldErr("privatekey", err.Error())
	}
	if w.Endpoint, err = parseEndpoint(u.Host); err != nil {
		return w, fieldErr("endpoint", err.Error())
	}
	q, err := parseQuery(u.RawQuery, wgParams)
	if err != nil {
		return w, err
	}
	if w.Provider, err = checkProvider(KindWireGuard, q, wgRequired); err != nil {
		return w, err
	}
	for _, k := range wgParams[1:] {
		if v, ok := q[k]; ok {
			if err := w.set(k, v); err != nil {
				return WireGuard{}, fieldErr(k, err.Error())
			}
		}
	}
	return w, nil
}

// checkProvider returns the provider of q and checks that q has every
// parameter that kind and provider require.
func checkProvider(kind Kind, q map[string]string, required []string) (string, error) {
	p := q["provider"]
	extra, ok := providerFields[kind][p]
	if p == "" {
		return "", fieldErr("provider", "required")
	}
	if !ok {
		return "", fieldErr("provider", "unknown provider")
	}
	for _, k := range slices.Concat(required, extra) {
		if _, ok := q[k]; !ok {
			return "", fieldErr(k, "required")
		}
	}
	return p, nil
}

// rule is a rule violation; ParseWireGuard wraps it with the field name.
type rule string

func (r rule) Error() string { return string(r) }

func (w *WireGuard) set(k, v string) error {
	var err error
	switch k {
	case "publickey":
		return parseKey(v, &w.PublicKey)
	case "address":
		w.Address, err = parseList(v, func(s string) (netip.Prefix, error) {
			if strings.Contains(s, "/") {
				return netip.ParsePrefix(s)
			}
			a, err := parseIP(s)
			return netip.PrefixFrom(a, a.BitLen()), err
		}, "each must be an IP or CIDR")
	case "dns":
		w.DNS, err = parseList(v, parseIP, "each must be an IP")
	case "mtu":
		w.MTU, err = parseUint(v, 576, 1500)
	case "allowedips":
		w.AllowedIPs, err = parseList(v, netip.ParsePrefix, "each must be a CIDR")
	case "reserved":
		var r [3]byte
		parts := strings.Split(v, ",")
		if len(parts) != len(r) {
			return rule("must be 3 numbers 0..255")
		}
		for i, p := range parts {
			n, err := parseUint(p, 0, 255)
			if err != nil {
				return rule("must be 3 numbers 0..255")
			}
			r[i] = byte(n)
		}
		w.Reserved = &r
	case "keepalive":
		w.Keepalive, err = parseUint(v, 0, 65535)
	case "presharedkey":
		var psk [32]byte
		if err := parseKey(v, &psk); err != nil {
			return err
		}
		w.PresharedKey = &psk
	default:
		return w.Meta.set(k, v)
	}
	return err
}

// set parses metadata parameter k.
func (m *Meta) set(k, v string) error {
	switch k {
	case "server":
		if !utf8.ValidString(v) || strings.ContainsFunc(v, func(r rune) bool { return !unicode.IsPrint(r) }) {
			return rule("must be printable")
		}
		m.Server = v
	case "entry", "exit":
		if len(v) != 2 || v[0] < 'A' || v[0] > 'Z' || v[1] < 'A' || v[1] > 'Z' {
			return rule("must be an upper-case ISO2 code")
		}
		if k == "entry" {
			m.Entry = v
		} else {
			m.Exit = v
		}
	case "load":
		n, err := parseUint(v, 0, 100)
		if err != nil {
			return err
		}
		m.Load = &n
	case "score":
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || !(f >= 0) || math.IsInf(f, 0) { // !(f >= 0) also rejects NaN
			return rule("must be a number >= 0")
		}
		m.Score = &f
	case "expires":
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return rule("must be RFC 3339")
		}
		m.Expires = t.UTC()
	}
	return nil
}

// parseQuery splits the raw query on "&" into unescaped values. An empty
// segment, an unknown or repeated key and an empty value are errors.
func parseQuery(raw string, known []string) (map[string]string, error) {
	q := map[string]string{}
	if raw == "" {
		return q, nil
	}
	for seg := range strings.SplitSeq(raw, "&") {
		k, v, _ := strings.Cut(seg, "=")
		key, err1 := url.QueryUnescape(k)
		value, err2 := url.QueryUnescape(v)
		switch {
		case err1 != nil || err2 != nil:
			return nil, fieldErr("uri", "invalid query escape")
		case !slices.Contains(known, key):
			// The key is not named: a malformed line may put a secret there.
			return nil, fieldErr("uri", "unknown or empty parameter")
		case q[key] != "":
			return nil, fieldErr(key, "repeated")
		case value == "":
			return nil, fieldErr(key, "empty value")
		}
		q[key] = value
	}
	return q, nil
}

// parseKey decodes a standard base64 key of exactly 32 bytes into dst.
func parseKey(s string, dst *[32]byte) error {
	b, err := base64.StdEncoding.Strict().DecodeString(s)
	if err != nil || len(b) != len(dst) {
		return rule("must be base64 of 32 bytes")
	}
	copy(dst[:], b)
	return nil
}

func parseEndpoint(hostport string) (string, error) {
	host, port, err := net.SplitHostPort(hostport)
	if err != nil {
		return "", rule("must be host:port, IPv6 in brackets")
	}
	p, err := parseUint(port, 1, 65535)
	if err != nil {
		return "", rule("port must be 1..65535")
	}
	if _, err := parseIP(host); err != nil && !isHostname(host) {
		return "", rule("host must be an IP or a domain")
	}
	return net.JoinHostPort(host, strconv.Itoa(p)), nil
}

func isHostname(s string) bool {
	if s == "" || len(s) > 253 {
		return false
	}
	for l := range strings.SplitSeq(s, ".") {
		if l == "" || len(l) > 63 || l[0] == '-' || l[len(l)-1] == '-' ||
			strings.ContainsFunc(l, func(r rune) bool {
				return (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '-'
			}) {
			return false
		}
	}
	return true
}

// parseIP parses an IP address without a zone.
func parseIP(s string) (netip.Addr, error) {
	a, err := netip.ParseAddr(s)
	if err == nil && a.Zone() != "" {
		err = rule("zone")
	}
	return a, err
}

// parseList parses a non-empty comma-separated list; any bad item fails it
// with msg.
func parseList[T any](s string, parse func(string) (T, error), msg string) ([]T, error) {
	var out []T
	for item := range strings.SplitSeq(s, ",") {
		v, err := parse(item)
		if err != nil {
			return nil, rule(msg)
		}
		out = append(out, v)
	}
	return out, nil
}

// parseUint parses a decimal number in lo..hi without a sign.
func parseUint(s string, lo, hi int) (int, error) {
	n, err := strconv.Atoi(s)
	if err != nil || s[0] == '+' || s[0] == '-' || n < lo || n > hi {
		return 0, rule("must be an integer " + strconv.Itoa(lo) + ".." + strconv.Itoa(hi))
	}
	return n, nil
}

// queryEscape is url.QueryEscape that keeps ",", "/", ":" and "=", which are
// literal in a query value, for readable lines.
var queryEscape = strings.NewReplacer("%2C", ",", "%2F", "/", "%3A", ":", "%3D", "=")

type kv struct{ k, v string }

// tunnelParams are the public tunnel parameters in format order.
func (w WireGuard) tunnelParams() []kv {
	join := func(n int, f func(int) string) string {
		s := make([]string, n)
		for i := range s {
			s[i] = f(i)
		}
		return strings.Join(s, ",")
	}
	p := []kv{
		{"publickey", base64.StdEncoding.EncodeToString(w.PublicKey[:])},
		{"address", join(len(w.Address), func(i int) string { return w.Address[i].String() })},
		{"dns", join(len(w.DNS), func(i int) string { return w.DNS[i].String() })},
		{"mtu", strconv.Itoa(w.MTU)},
		{"allowedips", join(len(w.AllowedIPs), func(i int) string { return w.AllowedIPs[i].String() })},
	}
	if r := w.Reserved; r != nil {
		p = append(p, kv{"reserved", join(len(r), func(i int) string { return strconv.Itoa(int(r[i])) })})
	}
	if w.Keepalive != 0 {
		p = append(p, kv{"keepalive", strconv.Itoa(w.Keepalive)})
	}
	return p
}

func formatLine(kind Kind, userinfo, endpoint string, params []kv, label string) string {
	var b strings.Builder
	b.WriteString(string(kind) + "://" + url.User(userinfo).String() + "@" + endpoint)
	sep := "?"
	for _, p := range params {
		if p.v != "" {
			b.WriteString(sep + p.k + "=" + queryEscape.Replace(url.QueryEscape(p.v)))
			sep = "&"
		}
	}
	if label != "" {
		b.WriteString("#" + url.PathEscape(label))
	}
	return b.String()
}

// FormatWireGuard formats w as a candidate line with parameters in a fixed
// order; empty optional fields and metadata are not written.
func FormatWireGuard(w WireGuard) string {
	psk := ""
	if w.PresharedKey != nil {
		psk = base64.StdEncoding.EncodeToString(w.PresharedKey[:])
	}
	return w.format(base64.StdEncoding.EncodeToString(w.PrivateKey[:]), psk)
}

// Redacted returns w as FormatWireGuard does, with "redacted" in place of
// the private key and of a preshared key: the line as scd shows it.
func (w WireGuard) Redacted() string {
	psk := ""
	if w.PresharedKey != nil {
		psk = "redacted"
	}
	return w.format("redacted", psk)
}

func (w WireGuard) format(privateKey, psk string) string {
	params := slices.Concat([]kv{{"provider", w.Provider}}, w.tunnelParams(), []kv{{"presharedkey", psk}}, w.Meta.params())
	return formatLine(KindWireGuard, privateKey, w.Endpoint, params, w.Label)
}

// params are the metadata parameters in format order.
func (m Meta) params() []kv {
	params := []kv{{"server", m.Server}, {"entry", m.Entry}, {"exit", m.Exit}}
	if m.Load != nil {
		params = append(params, kv{"load", strconv.Itoa(*m.Load)})
	}
	if m.Score != nil {
		params = append(params, kv{"score", strconv.FormatFloat(*m.Score, 'g', -1, 64)})
	}
	if !m.Expires.IsZero() {
		params = append(params, kv{"expires", m.Expires.Format(time.RFC3339Nano)})
	}
	return params
}

// InterfacePublicKey returns the interface public key (X25519 of the
// private key): a key identity that is not secret.
func (w WireGuard) InterfacePublicKey() [32]byte {
	priv, err := ecdh.X25519().NewPrivateKey(w.PrivateKey[:])
	if err != nil {
		panic(err) // unreachable: the key is 32 bytes
	}
	return [32]byte(priv.PublicKey().Bytes())
}

// Format implements fmt.Formatter for every verb: the Redacted line, so
// that no fmt verb prints a key.
func (w WireGuard) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, w.Redacted()) }

// LogValue implements slog.LogValuer: the Redacted line.
func (w WireGuard) LogValue() slog.Value { return slog.StringValue(w.Redacted()) }

// PublicHash returns the sha8 (first 8 hex characters of SHA-256) of the
// public tunnel fields: the line with the interface public key (X25519 of
// the private key) as userinfo and the hex SHA-256 of presharedkey, without
// provider, metadata and label. Any tunnel change gives a new hash.
func (w WireGuard) PublicHash() string {
	pub := w.InterfacePublicKey()
	params := w.tunnelParams()
	if w.PresharedKey != nil {
		sum := sha256.Sum256(w.PresharedKey[:])
		params = append(params, kv{"presharedkey", hex.EncodeToString(sum[:])})
	}
	line := formatLine(KindWireGuard, base64.StdEncoding.EncodeToString(pub[:]), w.Endpoint, params, "")
	sum := sha256.Sum256([]byte(line))
	return hex.EncodeToString(sum[:])[:8]
}
