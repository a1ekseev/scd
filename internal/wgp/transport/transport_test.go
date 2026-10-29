package transport

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	utls "github.com/refraction-networking/utls"

	"github.com/a1ekseev/scd/internal/wgp/provider"
)

// testSpec is a plain TLS 1.3/1.2 hello the Go test servers accept.
func testSpec() *utls.ClientHelloSpec {
	return &utls.ClientHelloSpec{
		TLSVersMin:         utls.VersionTLS12,
		TLSVersMax:         utls.VersionTLS13,
		CipherSuites:       []uint16{utls.TLS_AES_128_GCM_SHA256, utls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256},
		CompressionMethods: []uint8{0},
		Extensions: []utls.TLSExtension{
			&utls.SNIExtension{},
			&utls.SupportedCurvesExtension{Curves: []utls.CurveID{utls.X25519, utls.CurveP256}},
			&utls.SupportedPointsExtension{SupportedPoints: []byte{0}},
			&utls.SessionTicketExtension{},
			&utls.ALPNExtension{AlpnProtocols: []string{"http/1.1"}},
			&utls.SignatureAlgorithmsExtension{SupportedSignatureAlgorithms: []utls.SignatureScheme{utls.PSSWithSHA256, utls.PKCS1WithSHA256, utls.ECDSAWithP256AndSHA256}},
			&utls.ExtendedMasterSecretExtension{},
			&utls.SupportedVersionsExtension{Versions: []uint16{utls.VersionTLS13, utls.VersionTLS12}},
			&utls.KeyShareExtension{KeyShares: []utls.KeyShare{{Group: utls.X25519}}},
			&utls.PSKKeyExchangeModesExtension{Modes: []uint8{utls.PskModeDHE}},
		},
	}
}

func testAPI(timeoutMs int) provider.API {
	var a provider.API
	a.TimeoutMs = timeoutMs
	a.Retry.Attempts = 3
	a.Retry.MinDelayMs = 10
	a.Retry.MaxDelayMs = 20
	return a
}

func newClient(t *testing.T, cfg Config) *Client {
	t.Helper()
	if cfg.Spec == nil {
		cfg.Spec = testSpec
	}
	if cfg.API.TimeoutMs == 0 {
		cfg.API = testAPI(5000)
	}
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// recordSleep replaces the pause and records each delay.
func recordSleep(c *Client) *[]time.Duration {
	var got []time.Duration
	c.sleep = func(_ context.Context, d time.Duration) error {
		got = append(got, d)
		return nil
	}
	return &got
}

func tlsServer(t *testing.T, h http.HandlerFunc) (*httptest.Server, *x509.CertPool) {
	t.Helper()
	srv := httptest.NewTLSServer(h)
	t.Cleanup(srv.Close)
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	return srv, pool
}

func TestNew(t *testing.T) {
	for _, base := range []string{"", "http://h", "https://", "https://h/?q=1", "https://h/#f", "https://u@h", "::"} {
		if _, err := New(Config{BaseURL: base, Spec: testSpec}); err == nil {
			t.Errorf("%q: no error", base)
		}
	}
	if _, err := New(Config{BaseURL: "https://h"}); err == nil {
		t.Error("no spec: no error")
	}
	if _, err := New(Config{BaseURL: "https://h", Spec: testSpec, Mode: 7}); err == nil {
		t.Error("bad mode: no error")
	}
	c, err := New(Config{BaseURL: "https://h/v1/", Spec: testSpec})
	if err != nil || c.addr != "h:443" || c.authority != "h" || c.basePath != "/v1" {
		t.Fatalf("%+v %v", c, err)
	}
}

func TestRetryAfter(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		v    string
		want time.Duration
		ok   bool
	}{
		{"0", 0, true},
		{" 7 ", 7 * time.Second, true},
		{"99999999999999999", maxRetryAfter, true},
		{now.Add(4 * time.Second).Format(http.TimeFormat), 4 * time.Second, true},
		{now.Add(-time.Minute).Format(http.TimeFormat), 0, true},
		{"", 0, false},
		{"-1", 0, false},
		{"soon", 0, false},
	} {
		got, ok := retryAfter(tc.v, now)
		if got != tc.want || ok != tc.ok {
			t.Errorf("%q: %v %v, want %v %v", tc.v, got, ok, tc.want, tc.ok)
		}
	}
}

func TestRetry(t *testing.T) {
	random := time.Duration(-1) // any pause in minDelayMs..maxDelayMs
	for _, tc := range []struct {
		name     string
		statuses []int
		header   string
		calls    int
		sleeps   []time.Duration
		status   int // final StatusError status, 0 = success
	}{
		{"429 seconds", []int{429, 200}, "2", 2, []time.Duration{2 * time.Second}, 0},
		{"503 seconds", []int{503, 200}, "1", 2, []time.Duration{time.Second}, 0},
		{"429 random", []int{429, 200}, "", 2, []time.Duration{random}, 0},
		{"503 bad header", []int{503, 200}, "soon", 2, []time.Duration{random}, 0},
		{"408 at once", []int{408, 200}, "", 2, []time.Duration{0}, 0},
		{"502 at once", []int{502, 200}, "", 2, []time.Duration{0}, 0},
		{"attempts", []int{503, 502, 429, 200}, "", 3, []time.Duration{random, 0}, 429},
		{"500 not retried", []int{500, 200}, "", 1, nil, 500},
		{"401 not retried", []int{401, 200}, "", 1, nil, 401},
		{"304 returned", []int{304}, "", 1, nil, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var n atomic.Int32
			srv, pool := tlsServer(t, func(w http.ResponseWriter, _ *http.Request) {
				i := int(n.Add(1)) - 1
				if tc.header != "" {
					w.Header().Set("Retry-After", tc.header)
				}
				w.WriteHeader(tc.statuses[i])
			})
			c := newClient(t, Config{BaseURL: srv.URL, RootCAs: pool})
			sleeps := recordSleep(c)
			resp, err := c.Do(t.Context(), Request{Method: "GET", Path: "/x"})
			if int(n.Load()) != tc.calls {
				t.Errorf("calls %d, want %d", n.Load(), tc.calls)
			}
			if len(*sleeps) != len(tc.sleeps) {
				t.Fatalf("sleeps %v, want %v", *sleeps, tc.sleeps)
			}
			for i, d := range *sleeps {
				if w := tc.sleeps[i]; w == random && (d < 10*time.Millisecond || d > 20*time.Millisecond) || w != random && d != w {
					t.Errorf("sleep %d = %v, want %v", i, d, w)
				}
			}
			var se *StatusError
			switch {
			case tc.status == 0 && err != nil:
				t.Fatal(err)
			case tc.status != 0 && (!errors.As(err, &se) || se.Status != tc.status || resp.Status != tc.status):
				t.Fatalf("err %v, resp %d, want status %d", err, resp.Status, tc.status)
			}
		})
	}
}

func TestRetryAfterDate(t *testing.T) {
	var n atomic.Int32
	srv, pool := tlsServer(t, func(w http.ResponseWriter, _ *http.Request) {
		if n.Add(1) == 1 {
			w.Header().Set("Retry-After", time.Now().Add(3*time.Second).UTC().Format(http.TimeFormat))
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	})
	c := newClient(t, Config{BaseURL: srv.URL, RootCAs: pool})
	sleeps := recordSleep(c)
	if _, err := c.Do(t.Context(), Request{Method: "GET", Path: "/"}); err != nil {
		t.Fatal(err)
	}
	if len(*sleeps) != 1 || (*sleeps)[0] <= time.Second || (*sleeps)[0] > 3*time.Second {
		t.Fatalf("sleeps %v", *sleeps)
	}
}

func TestRetrySleepCancelled(t *testing.T) {
	srv, pool := tlsServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "3600")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	c := newClient(t, Config{BaseURL: srv.URL, RootCAs: pool})
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if _, err := c.Do(ctx, Request{Method: "GET", Path: "/"}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err %v", err)
	}
}

func TestErrorWithoutBody(t *testing.T) {
	for _, tc := range []struct {
		body, want string
	}{
		{`{"Code":2001,"Error":"Invalid input","Details":{"Token":"s3cr3t-detail"},"AccessToken":"s3cr3t-token"}`, `status 422, code 2001: "Invalid input"`},
		{`{"Error":"` + strings.Repeat("é", 150) + `"}`, `status 422: "` + strings.Repeat("é", 100) + `"`},
		{`not json s3cr3t-body`, `status 422`},
		{``, `status 422`},
	} {
		srv, pool := tlsServer(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = io.WriteString(w, tc.body)
		})
		c := newClient(t, Config{BaseURL: srv.URL, RootCAs: pool})
		_, err := c.Do(t.Context(), Request{Method: "POST", Path: "/", Header: []Header{{"Authorization", "Bearer s3cr3t-auth"}}, Body: []byte(`{"Password":"s3cr3t-pw"}`)})
		if err == nil || err.Error() != tc.want || strings.Contains(err.Error(), "s3cr3t") {
			t.Errorf("err %v, want %s", err, tc.want)
		}
	}
}

func TestHTTPMode(t *testing.T) {
	var conns atomic.Int32
	var got http.Header
	var host string
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, host = r.Header, r.Host
		_, _ = io.WriteString(w, r.URL.RequestURI())
	}))
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			conns.Add(1)
		}
	}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	c := newClient(t, Config{BaseURL: srv.URL + "/v0/", RootCAs: pool})
	for range 2 {
		resp, err := c.Do(t.Context(), Request{Method: "GET", Path: "/reg?a=1", Header: []Header{{"Host", "api.test"}, {"x-lower", "1"}}})
		if err != nil || string(resp.Body) != "/v0/reg?a=1" {
			t.Fatalf("%q %v", resp.Body, err)
		}
	}
	if conns.Load() != 1 {
		t.Errorf("connections %d, want 1 (keep-alive)", conns.Load())
	}
	if host != "api.test" || got.Get("x-lower") != "1" {
		t.Errorf("host %q header %v", host, got)
	}
}

func TestDial(t *testing.T) {
	var host string
	srv, pool := tlsServer(t, func(_ http.ResponseWriter, r *http.Request) { host = r.Host })
	var addrs []string
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		addrs = append(addrs, network+" "+addr)
		return new(net.Dialer).DialContext(ctx, network, srv.Listener.Addr().String())
	}
	// The httptest certificate is valid for example.com, so this also
	// checks that SNI and verification use the base URL host.
	c := newClient(t, Config{BaseURL: "https://example.com:8443", RootCAs: pool, Dial: dial})
	if _, err := c.Do(t.Context(), Request{Method: "GET", Path: "/"}); err != nil {
		t.Fatal(err)
	}
	if len(addrs) != 1 || addrs[0] != "tcp example.com:8443" || host != "example.com:8443" {
		t.Fatalf("dialed %v, host %q", addrs, host)
	}
}

func TestVerifiesCertificate(t *testing.T) {
	srv, _ := tlsServer(t, func(http.ResponseWriter, *http.Request) {})
	for _, mode := range []Mode{HTTP, Raw} {
		c := newClient(t, Config{BaseURL: srv.URL, Mode: mode})
		var ue x509.UnknownAuthorityError
		if _, err := c.Do(t.Context(), Request{Method: "GET", Path: "/", Header: []Header{{"Host", "h"}}}); !errors.As(err, &ue) {
			t.Errorf("mode %d: err %v", mode, err)
		}
	}
}

func TestTimeout(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	srv, pool := tlsServer(t, func(http.ResponseWriter, *http.Request) { <-release })
	raw := newRawServer(t, func(*rawConn) { <-release })
	for _, tc := range []struct {
		mode Mode
		base string
		pool *x509.CertPool
	}{{HTTP, srv.URL, pool}, {Raw, raw.url, raw.pool}} {
		c := newClient(t, Config{BaseURL: tc.base, Mode: tc.mode, RootCAs: tc.pool, API: testAPI(100)})
		start := time.Now()
		_, err := c.Do(t.Context(), Request{Method: "GET", Path: "/", Header: []Header{{"Host", "h"}}})
		if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 3*time.Second {
			t.Errorf("mode %d: err %v after %v", tc.mode, err, time.Since(start))
		}
	}
}

// rawConn is one accepted connection of rawServer.
type rawConn struct {
	tls *tls.Conn
	r   *bufio.Reader
	raw net.Conn
}

// readRequest reads one request head and its Content-Length body as bytes.
func (c *rawConn) readRequest() ([]byte, error) {
	var b bytes.Buffer
	n := 0
	for {
		line, err := c.r.ReadString('\n')
		b.WriteString(line)
		if err != nil {
			return b.Bytes(), err
		}
		if line == "\r\n" {
			break
		}
		if v, ok := strings.CutPrefix(strings.ToLower(line), "content-length: "); ok {
			n, _ = strconv.Atoi(strings.TrimSpace(v))
		}
	}
	_, err := io.CopyN(&b, c.r, int64(n))
	return b.Bytes(), err
}

// clientSentAfter reports whether the client wrote any byte (a TLS alert
// record) before closing the TCP connection.
func (c *rawConn) clientSentAfter() (int, error) {
	_ = c.raw.SetReadDeadline(time.Now().Add(5 * time.Second))
	n, err := io.Copy(io.Discard, c.raw)
	return int(n), err
}

type rawServer struct {
	url  string
	pool *x509.CertPool
}

// newRawServer serves TLS on 127.0.0.1 with the httptest certificate and
// hands each connection to h.
func newRawServer(t *testing.T, h func(*rawConn)) rawServer {
	t.Helper()
	hs := httptest.NewUnstartedServer(nil)
	hs.StartTLS()
	cfg := &tls.Config{Certificates: hs.TLS.Certificates}
	pool := x509.NewCertPool()
	pool.AddCert(hs.Certificate())
	hs.Close()
	ln, err := new(net.ListenConfig).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	t.Cleanup(func() { _ = ln.Close(); wg.Wait() })
	wg.Go(func() {
		for {
			raw, err := ln.Accept()
			if err != nil {
				return
			}
			wg.Go(func() {
				defer func() { _ = raw.Close() }()
				tc := tls.Server(raw, cfg)
				if tc.HandshakeContext(context.Background()) == nil {
					h(&rawConn{tls: tc, r: bufio.NewReader(tc), raw: raw})
				}
			})
		}
	})
	return rawServer{url: "https://" + ln.Addr().String(), pool: pool}
}

func gz(s string) string {
	var b bytes.Buffer
	w := gzip.NewWriter(&b)
	_, _ = io.WriteString(w, s)
	_ = w.Close()
	return b.String()
}

func zl(s string) string {
	var b bytes.Buffer
	w := zlib.NewWriter(&b)
	_, _ = io.WriteString(w, s)
	_ = w.Close()
	return b.String()
}

func TestRawMode(t *testing.T) {
	const body = `{"Code":1000,"ok":"é"}`
	chunked := func(s string) string {
		return strconv.FormatInt(int64(len(s[:5])), 16) + "\r\n" + s[:5] + "\r\n" +
			strconv.FormatInt(int64(len(s[5:])), 16) + "\r\n" + s[5:] + "\r\n0\r\n\r\n"
	}
	responses := []struct {
		name, resp string
	}{
		{"content-length", "HTTP/1.1 200 OK\r\nContent-Length: " + strconv.Itoa(len(body)) + "\r\n\r\n" + body},
		{"chunked", "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n" + chunked(body)},
		{"gzip", "HTTP/1.1 200 OK\r\nContent-Encoding: gzip\r\nContent-Length: " + strconv.Itoa(len(gz(body))) + "\r\n\r\n" + gz(body)},
		{"gzip chunked", "HTTP/1.1 200 OK\r\nContent-Encoding: gzip\r\nTransfer-Encoding: chunked\r\n\r\n" + chunked(gz(body))},
		{"deflate", "HTTP/1.1 200 OK\r\nContent-Encoding: deflate\r\nContent-Length: " + strconv.Itoa(len(zl(body))) + "\r\n\r\n" + zl(body)},
	}
	header := []Header{
		{"Host", "api.example.test"},
		{"x-pm-appversion", "app@1.0+arm"},
		{"User-Agent", "Agent/1.0 (Linux)"},
		{"Authorization", "Bearer fake"},
		{"Accept", "*/*"},
		{"Accept-Encoding", "gzip, deflate"},
		{"Content-Length", "13"},
		{"Content-Type", "application/json"},
	}
	want := "POST /api/auth/info?x=1 HTTP/1.1\r\n" +
		"Host: api.example.test\r\n" +
		"x-pm-appversion: app@1.0+arm\r\n" +
		"User-Agent: Agent/1.0 (Linux)\r\n" +
		"Authorization: Bearer fake\r\n" +
		"Accept: */*\r\n" +
		"Accept-Encoding: gzip, deflate\r\n" +
		"Content-Length: 13\r\n" +
		"Content-Type: application/json\r\n" +
		"\r\n" +
		`{"a": "b c"}` + "\n"

	type seen struct {
		req   []byte
		sent  int
		error error
	}
	results := make(chan seen, len(responses))
	var next atomic.Int32
	srv := newRawServer(t, func(c *rawConn) {
		req, err := c.readRequest()
		if err == nil {
			_, err = io.WriteString(c.tls, responses[next.Add(1)-1].resp)
		}
		n, eofErr := c.clientSentAfter()
		results <- seen{req, n, errors.Join(err, eofErr)}
	})
	c := newClient(t, Config{BaseURL: srv.url + "/api", Mode: Raw, RootCAs: srv.pool})
	for _, r := range responses {
		t.Run(r.name, func(t *testing.T) {
			resp, err := c.Do(t.Context(), Request{Method: "POST", Path: "/auth/info?x=1", Header: header, Body: []byte(`{"a": "b c"}` + "\n")})
			if err != nil || resp.Status != 200 || string(resp.Body) != body {
				t.Fatalf("%d %q %v", resp.Status, resp.Body, err)
			}
			s := <-results
			if s.error != nil {
				t.Fatal(s.error)
			}
			if string(s.req) != want {
				t.Errorf("request\n%q\nwant\n%q", s.req, want)
			}
			if s.sent != 0 {
				t.Errorf("client sent %d bytes after the response (TLS alert)", s.sent)
			}
		})
	}
}

// The check of TestRawMode sees an alert: a client that closes the TLS
// connection sends close_notify before EOF.
func TestAlertDetector(t *testing.T) {
	results := make(chan int, 1)
	srv := newRawServer(t, func(c *rawConn) {
		_, _ = c.readRequest()
		_, _ = io.WriteString(c.tls, "HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n")
		n, _ := c.clientSentAfter()
		results <- n
	})
	conn, err := (&tls.Dialer{Config: &tls.Config{RootCAs: srv.pool}}).DialContext(t.Context(), "tcp", strings.TrimPrefix(srv.url, "https://"))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(conn, "GET / HTTP/1.1\r\nHost: h\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	_ = conn.Close() // a TLS Close: sends close_notify
	if n := <-results; n == 0 {
		t.Fatal("close_notify not seen")
	}
}

func TestRawRejects(t *testing.T) {
	c := newClient(t, Config{BaseURL: "https://127.0.0.1:1", Mode: Raw})
	for _, req := range []Request{
		{Method: "GET", Path: "/", Header: []Header{{"X", "a\r\nInjected: 1"}}},
		{Method: "GET", Path: "/", Header: []Header{{"Bad Name", "1"}}},
		{Method: "GET", Path: "/a b"},
		{Method: "GET", Path: "a"},
		{Method: "GET /x", Path: "/"},
		{Method: "POST", Path: "/", Header: []Header{{"content-length", "3"}}, Body: []byte("{}")},
	} {
		if _, err := c.Do(t.Context(), req); err == nil || !strings.HasPrefix(err.Error(), "transport: bad") && !strings.Contains(err.Error(), "Content-Length") {
			t.Errorf("%+v: err %v", req, err)
		}
	}
}
