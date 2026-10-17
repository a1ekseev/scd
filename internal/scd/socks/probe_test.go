package socks

import (
	"bytes"
	"context"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeProxy is a minimal SOCKS5 server on 127.0.0.1:0.
type fakeProxy struct {
	ln           net.Listener
	done         chan struct{}
	greetReply   []byte // default 05 00
	connectReply []byte // default 05 00 00 01 + 6 zero bytes
	connectDelay time.Duration
	skipGreeting bool // never reply to the greeting
	skipConnect  bool // never reply to CONNECT
	after        func(c net.Conn)

	mu       sync.Mutex
	greeting []byte
	connect  []byte
}

func newFakeProxy(t *testing.T, configure func(p *fakeProxy)) *fakeProxy {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &fakeProxy{ln: ln, done: make(chan struct{})}
	if configure != nil {
		configure(p)
	}
	var wg sync.WaitGroup
	t.Cleanup(func() {
		close(p.done)
		_ = ln.Close()
		wg.Wait()
	})
	wg.Go(func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			wg.Go(func() {
				defer c.Close()
				go func() { <-p.done; _ = c.Close() }()
				p.serve(c)
			})
		}
	})
	return p
}

func (p *fakeProxy) serve(c net.Conn) {
	g := make([]byte, 3)
	if _, err := io.ReadFull(c, g); err != nil {
		return
	}
	p.mu.Lock()
	p.greeting = g
	p.mu.Unlock()
	if p.skipGreeting {
		<-p.done
		return
	}
	reply := p.greetReply
	if reply == nil {
		reply = []byte{5, 0}
	}
	if _, err := c.Write(reply); err != nil {
		return
	}
	if p.greetReply != nil && !bytes.Equal(p.greetReply, []byte{5, 0}) {
		return // close after a non-standard greeting reply
	}
	head := make([]byte, 4)
	if _, err := io.ReadFull(c, head); err != nil {
		return
	}
	req := append([]byte{}, head...)
	var rest int
	switch head[3] {
	case 1:
		rest = 6
	case 3:
		l := make([]byte, 1)
		if _, err := io.ReadFull(c, l); err != nil {
			return
		}
		req = append(req, l...)
		rest = int(l[0]) + 2
	}
	tail := make([]byte, rest)
	if _, err := io.ReadFull(c, tail); err != nil {
		return
	}
	req = append(req, tail...)
	p.mu.Lock()
	p.connect = req
	p.mu.Unlock()
	if p.connectDelay > 0 {
		select {
		case <-time.After(p.connectDelay):
		case <-p.done:
			return
		}
	}
	if p.skipConnect {
		<-p.done
		return
	}
	cr := p.connectReply
	if cr == nil {
		cr = []byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0}
	}
	if _, err := c.Write(cr); err != nil {
		return
	}
	if p.after != nil {
		p.after(c)
	}
}

func (p *fakeProxy) port() int { return p.ln.Addr().(*net.TCPAddr).Port }

func (p *fakeProxy) connectBytes() []byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]byte{}, p.connect...)
}

func (p *fakeProxy) greetingBytes() []byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]byte{}, p.greeting...)
}

// readHead reads until CRLFCRLF.
func readHead(c net.Conn) []byte {
	var buf []byte
	tmp := make([]byte, 512)
	for !bytes.Contains(buf, []byte("\r\n\r\n")) {
		n, err := c.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if err != nil {
			break
		}
	}
	return buf
}

func respond(resp string) func(c net.Conn) {
	return func(c net.Conn) {
		readHead(c)
		_, _ = c.Write([]byte(resp))
	}
}

func req(p *fakeProxy, url, method string, timeout time.Duration) Request {
	return Request{ProxyHost: "127.0.0.1", ProxyPort: p.port(), URL: url, Method: method, Timeout: timeout}
}

func TestResolvesOnHeadersNotClose(t *testing.T) {
	p := newFakeProxy(t, func(p *fakeProxy) {
		p.after = func(c net.Conn) {
			readHead(c)
			_, _ = c.Write([]byte("HTTP/1.1 204 No Content\r\n\r\n"))
			<-p.done // keep the connection open
		}
	})
	start := time.Now()
	resp, err := Do(context.Background(), req(p, "http://example.test/health", "GET", 2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusNoContent || resp.BodyBytes != 0 {
		t.Fatalf("resp = %+v", resp)
	}
	if el := time.Since(start); el > 500*time.Millisecond {
		t.Fatalf("took %v", el)
	}
}

func TestBodyBytesCountsAlreadyReceived(t *testing.T) {
	p := newFakeProxy(t, func(p *fakeProxy) {
		p.after = respond("HTTP/1.0 200 OK\r\nContent-Length: 5\r\n\r\nhello")
	})
	resp, err := Do(context.Background(), req(p, "http://example.test/", "GET", time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || resp.BodyBytes < 0 || resp.BodyBytes > 5 {
		t.Fatalf("resp = %+v", resp)
	}
}

func TestHandshakeTimeout(t *testing.T) {
	for name, cfg := range map[string]func(p *fakeProxy){
		"no greeting reply": func(p *fakeProxy) { p.skipGreeting = true },
		"no connect reply":  func(p *fakeProxy) { p.skipConnect = true },
	} {
		t.Run(name, func(t *testing.T) {
			p := newFakeProxy(t, cfg)
			_, err := Do(context.Background(), req(p, "http://example.test/", "GET", 50*time.Millisecond))
			if err == nil || err.Error() != "SOCKS5 handshake timed out after 50ms." {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func TestConnectTimeout(t *testing.T) {
	r := Request{
		ProxyHost: "127.0.0.1", ProxyPort: 1, URL: "http://example.test/", Method: "GET",
		Timeout: 50 * time.Millisecond,
		Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}
	_, err := Do(context.Background(), r)
	if err == nil || err.Error() != "SOCKS5 proxy connection timed out after 50ms." {
		t.Fatalf("err = %v", err)
	}
}

func TestConnectRefusedKeepsNetworkError(t *testing.T) {
	ln, _ := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	_, err := Do(context.Background(), Request{ProxyHost: "127.0.0.1", ProxyPort: port, URL: "http://example.test/", Method: "GET", Timeout: time.Second})
	if err == nil || !strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("err = %v", err)
	}
}

func TestParentCancel(t *testing.T) {
	p := newFakeProxy(t, func(p *fakeProxy) { p.skipGreeting = true })
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(30 * time.Millisecond); cancel() }()
	start := time.Now()
	_, err := Do(ctx, req(p, "http://example.test/", "GET", 5*time.Second))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("cancel not prompt")
	}
}

func TestRequestBytes(t *testing.T) {
	got := make(chan []byte, 1)
	p := newFakeProxy(t, func(p *fakeProxy) {
		p.after = func(c net.Conn) {
			got <- readHead(c)
			_, _ = c.Write([]byte("HTTP/1.1 200 OK\r\n\r\n"))
		}
	})
	if _, err := Do(context.Background(), req(p, "http://example.test/health?x=1#frag", "GET", time.Second)); err != nil {
		t.Fatal(err)
	}
	want := "GET /health?x=1 HTTP/1.1\r\nHost: example.test\r\nConnection: close\r\n\r\n"
	if s := string(<-got); s != want {
		t.Fatalf("request = %q want %q", s, want)
	}
	if !bytes.Equal(p.greetingBytes(), []byte{5, 1, 0}) {
		t.Fatalf("greeting = %v", p.greetingBytes())
	}
}

func TestEmptyPathBecomesSlash(t *testing.T) {
	got := make(chan []byte, 1)
	p := newFakeProxy(t, func(p *fakeProxy) {
		p.after = func(c net.Conn) {
			got <- readHead(c)
			_, _ = c.Write([]byte("HTTP/1.1 200 OK\r\n\r\n"))
		}
	})
	if _, err := Do(context.Background(), req(p, "http://example.test", "HEAD", time.Second)); err != nil {
		t.Fatal(err)
	}
	if s := string(<-got); !strings.HasPrefix(s, "HEAD / HTTP/1.1\r\n") {
		t.Fatalf("request = %q", s)
	}
}

func TestPostHasContentLengthZero(t *testing.T) {
	got := make(chan []byte, 1)
	p := newFakeProxy(t, func(p *fakeProxy) {
		p.after = func(c net.Conn) {
			got <- readHead(c)
			_, _ = c.Write([]byte("HTTP/1.1 200 OK\r\n\r\n"))
		}
	})
	if _, err := Do(context.Background(), req(p, "http://example.test/p", "POST", time.Second)); err != nil {
		t.Fatal(err)
	}
	want := "POST /p HTTP/1.1\r\nHost: example.test\r\nConnection: close\r\nContent-Length: 0\r\n\r\n"
	if s := string(<-got); s != want {
		t.Fatalf("request = %q want %q", s, want)
	}
}

func TestHostHeaderNonDefaultPort(t *testing.T) {
	cases := map[string]string{
		"http://example.test:8080/x": "Host: example.test:8080\r\n",
		"http://example.test:80/x":   "Host: example.test\r\n",
	}
	for u, want := range cases {
		got := make(chan []byte, 1)
		p := newFakeProxy(t, func(p *fakeProxy) {
			p.after = func(c net.Conn) {
				got <- readHead(c)
				_, _ = c.Write([]byte("HTTP/1.1 200 OK\r\n\r\n"))
			}
		})
		if _, err := Do(context.Background(), req(p, u, "GET", time.Second)); err != nil {
			t.Fatal(err)
		}
		if s := string(<-got); !strings.Contains(s, want) {
			t.Fatalf("%s: request = %q", u, s)
		}
	}
}

func TestIPv4UsesATYP1DomainUsesATYP3(t *testing.T) {
	ok := func(p *fakeProxy) { p.after = respond("HTTP/1.1 200 OK\r\n\r\n") }
	p := newFakeProxy(t, ok)
	if _, err := Do(context.Background(), req(p, "http://10.1.2.3:8080/", "GET", time.Second)); err != nil {
		t.Fatal(err)
	}
	if want := []byte{5, 1, 0, 1, 10, 1, 2, 3, 0x1f, 0x90}; !bytes.Equal(p.connectBytes(), want) {
		t.Fatalf("connect = %v want %v", p.connectBytes(), want)
	}
	p = newFakeProxy(t, ok)
	if _, err := Do(context.Background(), req(p, "http://example.test/", "GET", time.Second)); err != nil {
		t.Fatal(err)
	}
	want := append([]byte{5, 1, 0, 3, 12}, []byte("example.test")...)
	want = append(want, 0, 80)
	if !bytes.Equal(p.connectBytes(), want) {
		t.Fatalf("connect = %v want %v", p.connectBytes(), want)
	}
	// IPv6 is sent as a domain including brackets (TS behaviour, 03 §5.1 item 4).
	p = newFakeProxy(t, ok)
	if _, err := Do(context.Background(), req(p, "http://[::1]:81/", "GET", time.Second)); err != nil {
		t.Fatal(err)
	}
	want = append([]byte{5, 1, 0, 3, 5}, []byte("[::1]")...)
	want = append(want, 0, 81)
	if !bytes.Equal(p.connectBytes(), want) {
		t.Fatalf("connect = %v want %v", p.connectBytes(), want)
	}
}

func TestGreetingRejected(t *testing.T) {
	for _, reply := range [][]byte{{5, 0xff}, {4, 0}} {
		p := newFakeProxy(t, func(p *fakeProxy) { p.greetReply = reply })
		_, err := Do(context.Background(), req(p, "http://example.test/", "GET", time.Second))
		if err == nil || err.Error() != "SOCKS5 proxy does not accept no-auth authentication." {
			t.Fatalf("reply %v: err = %v", reply, err)
		}
	}
}

func TestConnectFailedCode5(t *testing.T) {
	p := newFakeProxy(t, func(p *fakeProxy) { p.connectReply = []byte{5, 5, 0, 1, 0, 0, 0, 0, 0, 0} })
	_, err := Do(context.Background(), req(p, "http://example.test/", "GET", time.Second))
	if err == nil || err.Error() != "SOCKS5 connect failed with code 5." {
		t.Fatalf("err = %v", err)
	}
}

func TestBindATYPDomainAndIPv6Skipped(t *testing.T) {
	replies := map[string][]byte{
		"domain": append([]byte{5, 0, 0, 3, 3, 'a', 'b', 'c'}, 0, 80),
		"ipv6":   append(append([]byte{5, 0, 0, 4}, make([]byte, 16)...), 0, 80),
		"ipv4":   {5, 0, 0, 1, 1, 2, 3, 4, 0, 80},
	}
	for name, reply := range replies {
		t.Run(name, func(t *testing.T) {
			p := newFakeProxy(t, func(p *fakeProxy) {
				// HTTP response is sent together with the SOCKS reply; no bytes may be lost.
				p.connectReply = append(append([]byte{}, reply...), []byte("HTTP/1.1 200 OK\r\n\r\n")...)
				p.after = func(c net.Conn) { readHead(c) }
			})
			resp, err := Do(context.Background(), req(p, "http://example.test/", "GET", time.Second))
			if err != nil || resp.StatusCode != http.StatusOK {
				t.Fatalf("resp = %+v err = %v", resp, err)
			}
		})
	}
}

func TestEOFDuringSocks(t *testing.T) {
	cases := map[string]struct {
		cfg  func(p *fakeProxy)
		want string
	}{
		"before greeting reply": {func(p *fakeProxy) { p.greetReply = []byte{} }, "Unexpected EOF from SOCKS proxy."},
		"mid greeting reply":    {func(p *fakeProxy) { p.greetReply = []byte{5} }, "Unexpected EOF while reading 2 bytes from SOCKS proxy."},
		"mid connect reply": {func(p *fakeProxy) {
			p.connectReply = []byte{5, 0, 0}
		}, "Unexpected EOF while reading 4 bytes from SOCKS proxy."},
		"mid bind": {func(p *fakeProxy) {
			p.connectReply = []byte{5, 0, 0, 1, 1, 2}
		}, "Unexpected EOF while reading 6 bytes from SOCKS proxy."},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			p := newFakeProxy(t, func(p *fakeProxy) {
				tc.cfg(p)
				p.after = func(net.Conn) {} // handler returns, closing the connection
			})
			_, err := Do(context.Background(), req(p, "http://example.test/", "GET", time.Second))
			if err == nil || err.Error() != tc.want {
				t.Fatalf("err = %v want %q", err, tc.want)
			}
		})
	}
}

func TestInvalidStatusLine(t *testing.T) {
	p := newFakeProxy(t, func(p *fakeProxy) { p.after = respond("garbage\r\n\r\n") })
	_, err := Do(context.Background(), req(p, "http://example.test/", "GET", time.Second))
	if err == nil || err.Error() != "Unable to parse HTTP status line: garbage" {
		t.Fatalf("err = %v", err)
	}
}

func TestEOFWithoutHeaders(t *testing.T) {
	p := newFakeProxy(t, func(p *fakeProxy) {
		p.after = func(c net.Conn) {
			readHead(c)
			_, _ = c.Write([]byte("HTTP/1.1 200 OK\r\nfoo"))
		}
	})
	_, err := Do(context.Background(), req(p, "http://example.test/", "GET", time.Second))
	if err == nil || err.Error() != "Invalid HTTP response from monitored endpoint." {
		t.Fatalf("err = %v", err)
	}
}

func TestHTTPRequestTimeout(t *testing.T) {
	p := newFakeProxy(t, func(p *fakeProxy) {
		p.after = func(c net.Conn) { readHead(c); <-p.done }
	})
	_, err := Do(context.Background(), req(p, "http://example.test/", "GET", 50*time.Millisecond))
	if err == nil || err.Error() != "SOCKS HTTP request timed out after 50ms." {
		t.Fatalf("err = %v", err)
	}
}

func TestLatencyExcludesHandshake(t *testing.T) {
	p := newFakeProxy(t, func(p *fakeProxy) {
		p.connectDelay = 300 * time.Millisecond
		p.after = func(c net.Conn) {
			readHead(c)
			time.Sleep(20 * time.Millisecond)
			_, _ = c.Write([]byte("HTTP/1.1 200 OK\r\n\r\n"))
		}
	})
	resp, err := Do(context.Background(), req(p, "http://example.test/", "GET", 3*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if resp.LatencyMs < 15 || resp.LatencyMs >= 150 {
		t.Fatalf("latency = %d", resp.LatencyMs)
	}
}

func tunnelTo(addr string) func(c net.Conn) {
	return func(c net.Conn) {
		up, err := (&net.Dialer{}).DialContext(context.Background(), "tcp", addr)
		if err != nil {
			return
		}
		defer up.Close()
		go func() { _, _ = io.Copy(up, c); _ = up.Close() }()
		_, _ = io.Copy(c, up)
	}
}

func TestHTTPSViaSocks(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	p := newFakeProxy(t, func(p *fakeProxy) { p.after = tunnelTo(srv.Listener.Addr().String()) })
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	r := req(p, "https://example.com:"+port+"/health", "GET", 2*time.Second)
	r.RootCAs = pool
	resp, err := Do(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("resp = %+v", resp)
	}
	// Without the CA the certificate must be rejected (verification is on).
	r.RootCAs = nil
	if _, err := Do(context.Background(), r); err == nil {
		t.Fatal("expected certificate error")
	}
}

func TestTLSHandshakeTimeout(t *testing.T) {
	p := newFakeProxy(t, func(p *fakeProxy) {
		p.after = func(c net.Conn) { _, _ = io.Copy(io.Discard, c) }
	})
	_, err := Do(context.Background(), req(p, "https://example.test/", "GET", 100*time.Millisecond))
	if err == nil || err.Error() != "SOCKS HTTPS TLS handshake timed out after 100ms." {
		t.Fatalf("err = %v", err)
	}
}
