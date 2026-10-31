package transport

import (
	"io"
	"net"
	"net/http"
	"strconv"
	"testing"

	"github.com/a1ekseev/scd/internal/common/schema"
)

func TestParseProxy(t *testing.T) {
	for _, tc := range []struct {
		v    any
		want string
		ok   bool
	}{
		{nil, "", true},
		{"", "", true},
		{"socks5://xray:1082", "socks5://xray:1082", true},
		{"socks5://[::1]:1080", "socks5://[::1]:1080", true},
		{"http://xray:1082", "", false},
		{"socks5://xray", "", false},
		{"socks5://u:p@xray:1082", "", false},
		{"socks5://xray:1082/x", "", false},
		{"socks5://xray:1082?a=1", "", false},
		{"xray:1082", "", false},
		{1082, "", false},
	} {
		m := map[string]any{}
		if tc.v != nil {
			m["proxy"] = tc.v
		}
		var is schema.Issues
		got, dial := ParseProxy(schema.Root(m, &is))
		if got != tc.want || (len(is) == 0) != tc.ok || (dial != nil) != (tc.want != "") {
			t.Errorf("%v: got %q dial %v issues %v", tc.v, got, dial != nil, is)
		}
	}
}

// socks5 is a one-connection SOCKS5 server (no auth, CONNECT) that records
// the requested host and forwards to target.
func socks5(t *testing.T, target string) (addr string, host <-chan string) {
	t.Helper()
	ln, err := new(net.ListenConfig).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	hosts := make(chan string, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = c.Close() }()
		b := make([]byte, 262)
		if _, err := io.ReadFull(c, b[:2]); err != nil {
			return
		}
		if _, err := io.ReadFull(c, b[:b[1]]); err != nil {
			return
		}
		_, _ = c.Write([]byte{5, 0})
		if _, err := io.ReadFull(c, b[:5]); err != nil || b[3] != 3 { // ATYP 3: a domain name
			hosts <- "not a domain"
			return
		}
		n := int(b[4])
		if _, err := io.ReadFull(c, b[:n+2]); err != nil {
			return
		}
		hosts <- string(b[:n]) + ":" + strconv.Itoa(int(b[n])<<8|int(b[n+1]))
		up, err := new(net.Dialer).DialContext(t.Context(), "tcp", target)
		if err != nil {
			return
		}
		defer func() { _ = up.Close() }()
		_, _ = c.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0})
		go func() { _, _ = io.Copy(up, c) }()
		_, _ = io.Copy(c, up)
	}()
	return ln.Addr().String(), hosts
}

// The API goes through the proxy by name (not resolved here), and SNI and
// verification still use the base URL host.
func TestProxyDial(t *testing.T) {
	srv, pool := tlsServer(t, func(http.ResponseWriter, *http.Request) {})
	addr, hosts := socks5(t, srv.Listener.Addr().String())
	var is schema.Issues
	_, dial := ParseProxy(schema.Root(map[string]any{"proxy": "socks5://" + addr}, &is))
	if len(is) != 0 {
		t.Fatal(is)
	}
	c := newClient(t, Config{BaseURL: "https://example.com:8443", Mode: Raw, RootCAs: pool, Dial: dial})
	req := Request{Method: "GET", Path: "/", Header: []Header{{"Host", "example.com"}}}
	if _, err := c.Do(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	if h := <-hosts; h != "example.com:8443" {
		t.Fatalf("proxy got %q", h)
	}
}
