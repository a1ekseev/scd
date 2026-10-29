package candidate

import (
	"bytes"
	"encoding/base64"
	"errors"
	"net/netip"
	"reflect"
	"strings"
	"testing"
	"time"
)

// key returns a fake base64 key of 32 bytes b.
func key(b byte) string { return base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{b}, 32)) }

var (
	priv = key(1)
	pub  = key(2)
	psk  = key(3)
)

// line builds a candidate line with userinfo priv and endpoint 192.0.2.10:51820.
func line(query string) string { return "wireguard://" + priv + "@192.0.2.10:51820?" + query }

func staticLine(extra string) string {
	q := "provider=static&publickey=" + pub + "&address=10.2.0.2/32&mtu=1420&allowedips=0.0.0.0/0"
	if extra != "" {
		q += "&" + extra
	}
	return line(q)
}

func withProvider(p, extra string) string {
	return strings.Replace(staticLine(extra), "provider=static", "provider="+p, 1)
}

func TestParseWireGuardProviders(t *testing.T) {
	tests := []struct {
		name string
		line string
		err  *FieldError
	}{
		{"static minimal", staticLine(""), nil},
		{"static dns", staticLine("dns=192.0.2.53"), nil},
		{"proton", withProvider("proton", "dns=10.2.0.1"), nil},
		{"proton no dns", withProvider("proton", ""), &FieldError{"dns", "required"}},
		{"warp", withProvider("warp", "dns=192.0.2.53,2001:db8::53&reserved=1,2,3"), nil},
		{"warp no reserved", withProvider("warp", "dns=192.0.2.53"), &FieldError{"reserved", "required"}},
		{"warp no dns", withProvider("warp", "reserved=1,2,3"), &FieldError{"dns", "required"}},
		{"unknown provider", withProvider("other", ""), &FieldError{"provider", "unknown provider"}},
		{"no provider", strings.Replace(staticLine(""), "provider=static&", "", 1), &FieldError{"provider", "required"}},
		{"no query", "wireguard://" + priv + "@192.0.2.10:51820", &FieldError{"provider", "required"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseWireGuard(tt.line)
			checkErr(t, err, tt.err)
		})
	}
}

func checkErr(t *testing.T, err error, want *FieldError) {
	t.Helper()
	if want == nil {
		if err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		return
	}
	fe, ok := errors.AsType[*FieldError](err)
	if !ok || *fe != *want {
		t.Fatalf("err = %v, want %v", err, want)
	}
}

func TestParseWireGuardRules(t *testing.T) {
	ok := staticLine
	tests := []struct {
		name string
		line string
		err  *FieldError
	}{
		{"other scheme", strings.Replace(ok(""), "wireguard://", "vless://", 1), &FieldError{"uri", "not a wireguard:// URI"}},
		{"path", strings.Replace(ok(""), ":51820?", ":51820/x?", 1), &FieldError{"uri", "not a wireguard:// URI"}},
		{"no userinfo", strings.Replace(ok(""), priv+"@", "", 1), &FieldError{"uri", "not a wireguard:// URI"}},
		{"password", strings.Replace(ok(""), priv+"@", priv+":x@", 1), &FieldError{"privatekey", "must not have a password"}},
		{"short key", strings.Replace(ok(""), priv, "AQEB", 1), &FieldError{"privatekey", "must be base64 of 32 bytes"}},
		{"no port", strings.Replace(ok(""), ":51820", "", 1), &FieldError{"endpoint", "must be host:port, IPv6 in brackets"}},
		{"port 0", strings.Replace(ok(""), ":51820", ":0", 1), &FieldError{"endpoint", "port must be 1..65535"}},
		{"port sign", strings.Replace(ok(""), ":51820", ":+1", 1), &FieldError{"uri", "not a wireguard:// URI"}},
		{"port high", strings.Replace(ok(""), ":51820", ":65536", 1), &FieldError{"endpoint", "port must be 1..65535"}},
		{"bad host", strings.Replace(ok(""), "192.0.2.10", "a_b", 1), &FieldError{"endpoint", "host must be an IP or a domain"}},
		{"domain", strings.Replace(ok(""), "192.0.2.10", "vpn.example.com", 1), nil},
		{"ipv6", strings.Replace(ok(""), "192.0.2.10", "[2001:db8::10]", 1), nil},
		{"bad label escape", ok("") + "#%zz", &FieldError{"label", "invalid percent-encoding"}},
		{"unknown param", ok("foo=1"), &FieldError{"uri", "unknown or empty parameter"}},
		{"upper-case param", ok("MTU=1420"), &FieldError{"uri", "unknown or empty parameter"}},
		{"empty segment", ok("") + "&", &FieldError{"uri", "unknown or empty parameter"}},
		{"bad escape", ok("server=%zz"), &FieldError{"uri", "invalid query escape"}},
		{"repeated", ok("mtu=1280"), &FieldError{"mtu", "repeated"}},
		{"empty value", ok("dns="), &FieldError{"dns", "empty value"}},
		{"no value", ok("dns"), &FieldError{"dns", "empty value"}},
		{"bad publickey", strings.Replace(ok(""), "publickey=", "publickey=AQ", 1), &FieldError{"publickey", "must be base64 of 32 bytes"}},
		{"bare address", strings.Replace(ok(""), "10.2.0.2/32", "10.2.0.2,2001:db8::2/128", 1), nil},
		{"bad address", strings.Replace(ok(""), "10.2.0.2/32", "10.2.0.2,", 1), &FieldError{"address", "each must be an IP or CIDR"}},
		{"zoned address", strings.Replace(ok(""), "10.2.0.2/32", "fe80::1%25eth0", 1), &FieldError{"address", "each must be an IP or CIDR"}},
		{"dns domain", ok("dns=dns.example.com"), &FieldError{"dns", "each must be an IP"}},
		{"dns cidr", ok("dns=192.0.2.53/32"), &FieldError{"dns", "each must be an IP"}},
		{"mtu low", strings.Replace(ok(""), "mtu=1420", "mtu=575", 1), &FieldError{"mtu", "must be an integer 576..1500"}},
		{"mtu high", strings.Replace(ok(""), "mtu=1420", "mtu=1501", 1), &FieldError{"mtu", "must be an integer 576..1500"}},
		{"allowedips ip", strings.Replace(ok(""), "0.0.0.0/0", "192.0.2.1", 1), &FieldError{"allowedips", "each must be a CIDR"}},
		{"reserved 2", ok("reserved=1,2"), &FieldError{"reserved", "must be 3 numbers 0..255"}},
		{"reserved 256", ok("reserved=1,2,256"), &FieldError{"reserved", "must be 3 numbers 0..255"}},
		{"keepalive", ok("keepalive=25"), nil},
		{"keepalive high", ok("keepalive=65536"), &FieldError{"keepalive", "must be an integer 0..65535"}},
		{"presharedkey", ok("presharedkey=" + strings.ReplaceAll(psk, "+", "%2B")), nil},
		{"bad presharedkey", ok("presharedkey=AQ"), &FieldError{"presharedkey", "must be base64 of 32 bytes"}},
		{"server", ok("server=NL%23123"), nil},
		{"server control", ok("server=a%0Ab"), &FieldError{"server", "must be printable"}},
		{"entry", ok("entry=CH&exit=NL"), nil},
		{"entry lower", ok("entry=ch"), &FieldError{"entry", "must be an upper-case ISO2 code"}},
		{"exit long", ok("exit=CHE"), &FieldError{"exit", "must be an upper-case ISO2 code"}},
		{"load", ok("load=0"), nil},
		{"load high", ok("load=101"), &FieldError{"load", "must be an integer 0..100"}},
		{"score", ok("score=12.5"), nil},
		{"score negative", ok("score=-1"), &FieldError{"score", "must be a number >= 0"}},
		{"score nan", ok("score=NaN"), &FieldError{"score", "must be a number >= 0"}},
		{"score inf", ok("score=Inf"), &FieldError{"score", "must be a number >= 0"}},
		{"expires", ok("expires=2026-11-01T10:00:00%2B03:00"), nil},
		{"expires date", ok("expires=2026-11-01"), &FieldError{"expires", "must be RFC 3339"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseWireGuard(tt.line)
			checkErr(t, err, tt.err)
		})
	}
}

func full() WireGuard {
	load, score := 37, 0.25
	return WireGuard{
		Provider:     "warp",
		PrivateKey:   [32]byte{1, 2, 3},
		Endpoint:     "[2001:db8::10]:2408",
		PublicKey:    [32]byte{4, 5, 6},
		Address:      []netip.Prefix{netip.MustParsePrefix("192.0.2.2/32"), netip.MustParsePrefix("2001:db8::2/128")},
		DNS:          []netip.Addr{netip.MustParseAddr("192.0.2.53"), netip.MustParseAddr("2001:db8::53")},
		MTU:          1280,
		AllowedIPs:   []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0"), netip.MustParsePrefix("::/0")},
		Reserved:     &[3]byte{1, 2, 255},
		Keepalive:    25,
		PresharedKey: &[32]byte{7, 8, 9},
		Label:        "🇨🇭 Zürich, #1 / test",
		Meta: Meta{
			Server: "CH#12", Entry: "CH", Exit: "NL", Load: &load, Score: &score,
			Expires: time.Date(2026, 11, 1, 10, 0, 0, 500, time.UTC),
		},
	}
}

func TestFormatWireGuardRoundTrip(t *testing.T) {
	minimal := WireGuard{
		Provider: "static", PrivateKey: [32]byte{1}, Endpoint: "vpn.example.com:51820", PublicKey: [32]byte{2},
		Address: []netip.Prefix{netip.MustParsePrefix("10.2.0.2/32")}, MTU: 1420,
		AllowedIPs: []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0")},
	}
	for _, w := range []WireGuard{full(), minimal} {
		s := FormatWireGuard(w)
		got, err := ParseWireGuard(s)
		if err != nil {
			t.Fatalf("parse %q: %v", s, err)
		}
		if !reflect.DeepEqual(got, w) {
			t.Fatalf("round trip of %q:\n got %+v\nwant %+v", s, got, w)
		}
		if again := FormatWireGuard(got); again != s {
			t.Fatalf("format not stable: %q != %q", again, s)
		}
	}
	want := "wireguard://AQAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=@vpn.example.com:51820" +
		"?provider=static&publickey=AgAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=" +
		"&address=10.2.0.2/32&mtu=1420&allowedips=0.0.0.0/0"
	if got := FormatWireGuard(minimal); got != want {
		t.Fatalf("format = %q, want %q", got, want)
	}
}

func TestPublicHash(t *testing.T) {
	base := full().PublicHash()
	if len(base) != 8 || strings.Trim(base, "0123456789abcdef") != "" {
		t.Fatalf("hash = %q, want 8 hex characters", base)
	}
	tests := []struct {
		name    string
		mut     func(*WireGuard)
		changes bool
	}{
		{"load", func(w *WireGuard) { w.Meta.Load = nil }, false},
		{"score", func(w *WireGuard) { s := 3.0; w.Meta.Score = &s }, false},
		{"expires", func(w *WireGuard) { w.Meta.Expires = time.Time{} }, false},
		{"server entry exit", func(w *WireGuard) { w.Meta.Server, w.Meta.Entry, w.Meta.Exit = "x", "", "DE" }, false},
		{"label", func(w *WireGuard) { w.Label = "other" }, false},
		{"provider", func(w *WireGuard) { w.Provider = "static" }, false},
		{"endpoint", func(w *WireGuard) { w.Endpoint = "192.0.2.10:2408" }, true},
		{"private key", func(w *WireGuard) { w.PrivateKey[31] = 1 }, true},
		{"public key", func(w *WireGuard) { w.PublicKey[31] = 1 }, true},
		{"preshared key", func(w *WireGuard) { w.PresharedKey = &[32]byte{1} }, true},
		{"no preshared key", func(w *WireGuard) { w.PresharedKey = nil }, true},
		{"address", func(w *WireGuard) { w.Address = w.Address[:1] }, true},
		{"mtu", func(w *WireGuard) { w.MTU = 1420 }, true},
		{"reserved", func(w *WireGuard) { w.Reserved = nil }, true},
		{"keepalive", func(w *WireGuard) { w.Keepalive = 0 }, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := full()
			tt.mut(&w)
			if got := w.PublicHash(); (got != base) != tt.changes {
				t.Fatalf("hash %q vs base %q, want changed=%v", got, base, tt.changes)
			}
		})
	}
}

func TestErrorTextHasNoSecrets(t *testing.T) {
	short := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, 31))
	secrets := []string{priv, psk, short, "AQEBAQEB"}
	lines := []string{
		strings.Replace(staticLine(""), priv, short, 1),
		strings.Replace(staticLine(""), "mtu=1420", "mtu=9999", 1),
		staticLine(priv + "=1"),
		staticLine("presharedkey=" + short),
		staticLine("presharedkey=" + psk + "&presharedkey=" + psk),
		staticLine("dns=" + psk),
		strings.Replace(staticLine(""), "@192.0.2.10:51820", "@"+priv, 1),
		"wireguard://" + priv + "@192.0.2.10:51820?%zz" + priv,
		"wireguard://" + priv + "@[" + priv + "]:1",
	}
	for _, l := range lines {
		_, err := ParseWireGuard(l)
		if err == nil {
			t.Fatalf("no error for %q", l)
		}
		for _, s := range secrets {
			if strings.Contains(err.Error(), s) {
				t.Fatalf("error %q contains a secret", err)
			}
		}
	}
}

func FuzzParseWireGuard(f *testing.F) {
	f.Add(FormatWireGuard(full()))
	f.Add(staticLine(""))
	f.Add(withProvider("proton", "dns=10.2.0.1"))
	f.Add(withProvider("warp", "dns=192.0.2.53&reserved=1,2,3") + "#%F0%9F%87%A8%F0%9F%87%AD")
	f.Add(staticLine("keepalive=0&score=1e3&expires=2026-11-01T10:00:00Z"))
	f.Add("wireguard://" + priv + "@[2001:db8::1]:1?provider=x")
	f.Fuzz(func(t *testing.T, s string) {
		w, err := ParseWireGuard(s)
		if err != nil {
			if _, ok := errors.AsType[*FieldError](err); !ok {
				t.Fatalf("error %T is not a *FieldError", err)
			}
			return
		}
		out := FormatWireGuard(w)
		got, err := ParseWireGuard(out)
		if err != nil {
			t.Fatalf("reparse %q: %v", out, err)
		}
		if !reflect.DeepEqual(got, w) {
			t.Fatalf("round trip of %q:\n got %+v\nwant %+v", out, got, w)
		}
		if FormatWireGuard(got) != out {
			t.Fatalf("format not stable for %q", out)
		}
		_ = w.PublicHash()
	})
}

func TestRedacted(t *testing.T) {
	w := full()
	got := w.Redacted()
	priv, psk := base64.StdEncoding.EncodeToString(w.PrivateKey[:]), base64.StdEncoding.EncodeToString(w.PresharedKey[:])
	if !strings.HasPrefix(got, "wireguard://redacted@") || !strings.Contains(got, "&presharedkey=redacted&") ||
		strings.Contains(got, priv) || strings.Contains(got, psk) {
		t.Fatalf("Redacted() = %q", got)
	}
	_, want, _ := strings.Cut(FormatWireGuard(w), "@")
	want = strings.Replace(want, "presharedkey="+psk, "presharedkey=redacted", 1)
	if _, rest, _ := strings.Cut(got, "@"); rest != want {
		t.Fatalf("Redacted() = %q, want the rest %q", got, want)
	}
}
