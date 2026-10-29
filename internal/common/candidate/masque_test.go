package candidate

import (
	"bytes"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Fake keys generated per test run.
var (
	mqPriv, mqPriv2 = genKey(elliptic.P256()), genKey(elliptic.P256())
	mqPeer          = genKey(elliptic.P256())
	mqPrivB64       = pkcs8(mqPriv)
	mqPeerB64       = mustPKIX(&mqPeer.PublicKey)
)

func genKey(c elliptic.Curve) *ecdsa.PrivateKey {
	k, err := ecdsa.GenerateKey(c, rand.Reader)
	if err != nil {
		panic(err)
	}
	return k
}

func pkcs8(k any) string {
	b, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		panic(err)
	}
	return keyEncoding.EncodeToString(b)
}

func pkix(k any) string {
	b, err := x509.MarshalPKIXPublicKey(k)
	if err != nil {
		panic(err)
	}
	return keyEncoding.EncodeToString(b)
}

// mqLine builds a valid masque line with endpoint 192.0.2.30:443 and extra
// query parameters.
func mqLine(extra string) string {
	q := "provider=warp&peerkey=" + mqPeerB64 + "&address=172.16.0.2/32,2001:db8::2/128" +
		"&sni=masque.example.com&transport=h3&dns=192.0.2.53,2001:db8::53&priority=0"
	if extra != "" {
		q += "&" + extra
	}
	return "masque://" + mqPrivB64 + "@192.0.2.30:443?" + q
}

// mqDrop removes parameter k from a mqLine.
func mqDrop(k string) string {
	l := mqLine("")
	i := strings.Index(l, "&"+k+"=")
	if i < 0 {
		i = strings.Index(l, "?"+k+"=")
	}
	j := strings.IndexByte(l[i+1:], '&')
	if j < 0 {
		return l[:i]
	}
	return l[:i+1] + l[i+j+2:]
}

func mqSet(k, v string) string {
	l := mqDrop(k)
	return l + "&" + k + "=" + v
}

func TestParseMasqueProviders(t *testing.T) {
	tests := []struct {
		name string
		line string
		err  *FieldError
	}{
		{"warp", mqLine(""), nil},
		{"static", mqSet("provider", "static"), &FieldError{"provider", "unknown provider"}},
		{"proton", mqSet("provider", "proton"), &FieldError{"provider", "unknown provider"}},
		{"no provider", strings.Replace(mqLine(""), "provider=warp&", "", 1), &FieldError{"provider", "required"}},
		{"no query", "masque://" + mqPrivB64 + "@192.0.2.30:443", &FieldError{"provider", "required"}},
	}
	for _, k := range masqueRequired {
		tests = append(tests, struct {
			name string
			line string
			err  *FieldError
		}{"no " + k, mqDrop(k), &FieldError{k, "required"}})
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseMasque(tt.line)
			checkErr(t, err, tt.err)
		})
	}
}

func TestParseMasqueRules(t *testing.T) {
	ok := mqLine
	p384 := genKey(elliptic.P384())
	_, edPriv, _ := ed25519.GenerateKey(rand.Reader)
	x, _ := ecdh.P256().GenerateKey(rand.Reader)
	sec1, _ := x509.MarshalECPrivateKey(mqPriv)
	tests := []struct {
		name string
		line string
		err  *FieldError
	}{
		{"other scheme", strings.Replace(ok(""), "masque://", "wireguard://", 1), &FieldError{"uri", "not a masque:// URI"}},
		{"path", strings.Replace(ok(""), ":443?", ":443/x?", 1), &FieldError{"uri", "not a masque:// URI"}},
		{"no userinfo", strings.Replace(ok(""), mqPrivB64+"@", "", 1), &FieldError{"uri", "not a masque:// URI"}},
		{"password", strings.Replace(ok(""), mqPrivB64+"@", mqPrivB64+":x@", 1), &FieldError{"privatekey", "must not have a password"}},
		{"std base64 key", strings.Replace(ok(""), mqPrivB64, mqPrivB64+"==", 1), &FieldError{"privatekey", "must be base64url"}},
		{"garbage key", strings.Replace(ok(""), mqPrivB64, "AQEB", 1), &FieldError{"privatekey", "must be a PKCS#8 ECDSA P-256 key"}},
		{"sec1 key", strings.Replace(ok(""), mqPrivB64, keyEncoding.EncodeToString(sec1), 1), &FieldError{"privatekey", "must be a PKCS#8 ECDSA P-256 key"}},
		{"p384 key", strings.Replace(ok(""), mqPrivB64, pkcs8(p384), 1), &FieldError{"privatekey", "must be a PKCS#8 ECDSA P-256 key"}},
		{"ed25519 key", strings.Replace(ok(""), mqPrivB64, pkcs8(edPriv), 1), &FieldError{"privatekey", "must be a PKCS#8 ECDSA P-256 key"}},
		{"ecdh key", strings.Replace(ok(""), mqPrivB64, pkcs8(x), 1), nil}, // the same P-256 key in PKCS#8
		{"public as private", strings.Replace(ok(""), mqPrivB64, mqPeerB64, 1), &FieldError{"privatekey", "must be a PKCS#8 ECDSA P-256 key"}},
		{"no port", strings.Replace(ok(""), ":443", "", 1), &FieldError{"endpoint", "must be IP:port, IPv6 in brackets"}},
		{"port 0", strings.Replace(ok(""), ":443", ":0", 1), &FieldError{"endpoint", "port must be 1..65535"}},
		{"port high", strings.Replace(ok(""), ":443", ":65536", 1), &FieldError{"endpoint", "port must be 1..65535"}},
		{"domain", strings.Replace(ok(""), "192.0.2.30", "masque.example.com", 1), &FieldError{"endpoint", "host must be an IP"}},
		{"zone", strings.Replace(ok(""), "192.0.2.30", "[fe80::1%25eth0]", 1), &FieldError{"endpoint", "host must be an IP"}},
		{"ipv6", strings.Replace(ok(""), "192.0.2.30", "[2001:db8::30]", 1), nil},
		{"bad label escape", ok("") + "#%zz", &FieldError{"label", "invalid percent-encoding"}},
		{"unknown param", ok("mtu=1280"), &FieldError{"uri", "unknown or empty parameter"}},
		{"wg param", ok("publickey=x"), &FieldError{"uri", "unknown or empty parameter"}},
		{"repeated", ok("transport=h2"), &FieldError{"transport", "repeated"}},
		{"empty value", mqSet("sni", ""), &FieldError{"sni", "empty value"}},
		{"bad peerkey", mqSet("peerkey", "AQ"), &FieldError{"peerkey", "must be a PKIX ECDSA P-256 key"}},
		{"peerkey padded", mqSet("peerkey", mqPeerB64+"=="), &FieldError{"peerkey", "must be base64url"}},
		{"peerkey p384", mqSet("peerkey", pkix(&p384.PublicKey)), &FieldError{"peerkey", "must be a PKIX ECDSA P-256 key"}},
		{"peerkey ed25519", mqSet("peerkey", pkix(edPriv.Public())), &FieldError{"peerkey", "must be a PKIX ECDSA P-256 key"}},
		{"peerkey private", mqSet("peerkey", mqPrivB64), &FieldError{"peerkey", "must be a PKIX ECDSA P-256 key"}},
		{"bare address", mqSet("address", "172.16.0.2"), nil},
		{"ipv6 address only", mqSet("address", "2001:db8::2"), nil},
		{"two ipv4", mqSet("address", "172.16.0.2,172.16.0.3"), &FieldError{"address", "at most one IPv4 and one IPv6"}},
		{"two ipv6", mqSet("address", "2001:db8::2,2001:db8::3/128"), &FieldError{"address", "at most one IPv4 and one IPv6"}},
		{"mapped ipv4", mqSet("address", "172.16.0.2,::ffff:172.16.0.3"), &FieldError{"address", "at most one IPv4 and one IPv6"}},
		{"bad address", mqSet("address", "172.16.0.2,"), &FieldError{"address", "each must be an IP or CIDR"}},
		{"sni endpoint ip", mqSet("sni", "192.0.2.30"), nil},
		{"sni other ip", mqSet("sni", "192.0.2.31"), &FieldError{"sni", "an IP must be the endpoint IP"}},
		{"sni bad", mqSet("sni", "a_b"), &FieldError{"sni", "must be a hostname or the endpoint IP"}},
		{"sni port", mqSet("sni", "masque.example.com:443"), &FieldError{"sni", "must be a hostname or the endpoint IP"}},
		{"h2", mqSet("transport", "h2"), nil},
		{"h1", mqSet("transport", "h1"), &FieldError{"transport", "must be h3 or h2"}},
		{"upper H3", mqSet("transport", "H3"), &FieldError{"transport", "must be h3 or h2"}},
		{"dns domain", mqSet("dns", "dns.example.com"), &FieldError{"dns", "each must be an IP"}},
		{"dns cidr", mqSet("dns", "192.0.2.53/32"), &FieldError{"dns", "each must be an IP"}},
		{"priority 7", mqSet("priority", "7"), nil},
		{"priority negative", mqSet("priority", "-1"), &FieldError{"priority", "must be an integer >= 0"}},
		{"priority plus", mqSet("priority", "%2B1"), &FieldError{"priority", "must be an integer >= 0"}},
		{"priority float", mqSet("priority", "1.5"), &FieldError{"priority", "must be an integer >= 0"}},
		{"metadata", ok("entry=CH&exit=NL&expires=2026-11-01T10:00:00Z&server=CH%2312&load=3&score=1"), nil},
		{"entry lower", ok("entry=ch"), &FieldError{"entry", "must be an upper-case ISO2 code"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseMasque(tt.line)
			checkErr(t, err, tt.err)
		})
	}
}

func fullMasque() Masque {
	return Masque{
		Provider:   "warp",
		PrivateKey: mqPriv,
		Endpoint:   netip.MustParseAddrPort("[2001:db8::30]:443"),
		PeerKey:    &mqPeer.PublicKey,
		Address:    []netip.Prefix{netip.MustParsePrefix("172.16.0.2/32"), netip.MustParsePrefix("2001:db8::2/128")},
		SNI:        "masque.example.com",
		Transport:  TransportH2,
		DNS:        []netip.Addr{netip.MustParseAddr("192.0.2.53"), netip.MustParseAddr("2001:db8::53")},
		Priority:   1,
		Label:      "🇨🇭 Zürich, #1 / test",
		Meta:       Meta{Entry: "CH", Exit: "NL", Expires: time.Date(2026, 11, 1, 10, 0, 0, 500, time.UTC)},
	}
}

// sameMasque reports whether a and b are equal, comparing keys by value.
func sameMasque(a, b Masque) bool {
	if !a.PrivateKey.Equal(b.PrivateKey) || !a.PeerKey.Equal(b.PeerKey) {
		return false
	}
	a.PrivateKey, b.PrivateKey, a.PeerKey, b.PeerKey = nil, nil, nil, nil
	return reflect.DeepEqual(a, b)
}

func TestFormatMasqueRoundTrip(t *testing.T) {
	minimal := fullMasque()
	minimal.Endpoint, minimal.SNI = netip.MustParseAddrPort("192.0.2.30:443"), "192.0.2.30"
	minimal.Address, minimal.DNS = minimal.Address[:1], minimal.DNS[:1]
	minimal.Transport, minimal.Priority, minimal.Label, minimal.Meta = TransportH3, 0, "", Meta{}
	for _, m := range []Masque{fullMasque(), minimal} {
		s := FormatMasque(m)
		got, err := ParseMasque(s)
		if err != nil {
			t.Fatalf("parse %q: %v", s, err)
		}
		if !sameMasque(got, m) {
			t.Fatalf("round trip of %q:\n got %v\nwant %v", s, got, m)
		}
		if again := FormatMasque(got); again != s {
			t.Fatalf("format not stable: %q != %q", again, s)
		}
	}
	want := "masque://" + mqPrivB64 + "@192.0.2.30:443?provider=warp&peerkey=" + mqPeerB64 +
		"&address=172.16.0.2/32&sni=192.0.2.30&transport=h3&dns=192.0.2.53&priority=0"
	if got := FormatMasque(minimal); got != want {
		t.Fatalf("format = %q, want %q", got, want)
	}
}

func TestMasquePublicHash(t *testing.T) {
	base := fullMasque().PublicHash()
	if len(base) != 8 || strings.Trim(base, "0123456789abcdef") != "" {
		t.Fatalf("hash = %q, want 8 hex characters", base)
	}
	other := genKey(elliptic.P256())
	tests := []struct {
		name    string
		mut     func(*Masque)
		changes bool
	}{
		{"priority", func(m *Masque) { m.Priority = 0 }, false},
		{"metadata", func(m *Masque) { m.Meta = Meta{Server: "x"} }, false},
		{"label", func(m *Masque) { m.Label = "other" }, false},
		{"provider", func(m *Masque) { m.Provider = "other" }, false},
		{"same key copy", func(m *Masque) { k := *m.PrivateKey; m.PrivateKey = &k }, false},
		{"private key", func(m *Masque) { m.PrivateKey = mqPriv2 }, true},
		{"peer key", func(m *Masque) { m.PeerKey = &other.PublicKey }, true},
		{"endpoint", func(m *Masque) { m.Endpoint = netip.MustParseAddrPort("[2001:db8::30]:8443") }, true},
		{"address", func(m *Masque) { m.Address = m.Address[:1] }, true},
		{"sni", func(m *Masque) { m.SNI = "other.example.com" }, true},
		{"transport", func(m *Masque) { m.Transport = TransportH3 }, true},
		{"dns", func(m *Masque) { m.DNS = m.DNS[1:] }, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := fullMasque()
			tt.mut(&m)
			if got := m.PublicHash(); (got != base) != tt.changes {
				t.Fatalf("hash %q vs base %q, want changed=%v", got, base, tt.changes)
			}
		})
	}
}

// TestMasqueKeyNotPrinted checks that no fmt verb, slog and no parse error
// shows the private key.
func TestMasqueKeyNotPrinted(t *testing.T) {
	m := fullMasque()
	raw, err := m.PrivateKey.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	secrets := []string{mqPrivB64, hex.EncodeToString(raw), base64.StdEncoding.EncodeToString(raw), keyEncoding.EncodeToString(raw)}
	var outs []string
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x"} {
		outs = append(outs, fmt.Sprintf(verb, m), fmt.Sprintf(verb, &m), fmt.Sprintf(verb, []Masque{m}))
	}
	var buf bytes.Buffer
	for _, h := range []slog.Handler{slog.NewTextHandler(&buf, nil), slog.NewJSONHandler(&buf, nil)} {
		slog.New(h).Info("x", "candidate", m, "ptr", &m)
	}
	outs = append(outs, buf.String(), m.Redacted())
	for _, l := range []string{
		strings.Replace(mqLine(""), "@192.0.2.30:443", "@"+mqPrivB64, 1),
		mqLine(mqPrivB64 + "=1"),
		mqSet("dns", mqPrivB64),
		mqSet("peerkey", mqPrivB64),
		mqSet("sni", mqPrivB64),
		strings.Replace(mqLine(""), mqPrivB64, mqPrivB64[:40], 1),
		"masque://" + mqPrivB64 + "@192.0.2.30:443?%zz" + mqPrivB64,
	} {
		_, err := ParseMasque(l)
		if err == nil {
			t.Fatalf("no error for %q", l)
		}
		outs = append(outs, err.Error())
	}
	for _, o := range outs {
		for _, s := range secrets {
			if strings.Contains(o, s) {
				t.Fatalf("output %q contains the private key", o)
			}
		}
	}
	if !strings.HasPrefix(m.Redacted(), "masque://redacted@") {
		t.Fatalf("Redacted() = %q", m.Redacted())
	}
	_, want, _ := strings.Cut(FormatMasque(m), "@")
	if _, rest, _ := strings.Cut(m.Redacted(), "@"); rest != want {
		t.Fatalf("Redacted() = %q, want the rest %q", m.Redacted(), want)
	}
}

func FuzzParseMasque(f *testing.F) {
	f.Add(FormatMasque(fullMasque()))
	f.Add(mqLine(""))
	f.Add(mqSet("sni", "192.0.2.30"))
	f.Add(mqLine("entry=CH&score=1e3&expires=2026-11-01T10:00:00Z") + "#%F0%9F%87%A8%F0%9F%87%AD")
	f.Add("masque://" + mqPrivB64 + "@[2001:db8::1]:1?provider=x")
	f.Fuzz(func(t *testing.T, s string) {
		m, err := ParseMasque(s)
		if err != nil {
			if _, ok := errors.AsType[*FieldError](err); !ok {
				t.Fatalf("error %T is not a *FieldError", err)
			}
			return
		}
		out := FormatMasque(m)
		got, err := ParseMasque(out)
		if err != nil {
			t.Fatalf("reparse %q: %v", out, err)
		}
		if !sameMasque(got, m) {
			t.Fatalf("round trip of %q:\n got %v\nwant %v", out, got, m)
		}
		if FormatMasque(got) != out {
			t.Fatalf("format not stable for %q", out)
		}
		_ = m.PublicHash()
	})
}
