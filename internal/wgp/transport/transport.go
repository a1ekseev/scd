// Package transport is the common wgp API client. It applies the api.*
// settings (a timeout per attempt, retries with Retry-After), returns errors
// without response bodies, and dials TLS with uTLS from the ClientHello spec
// a provider passes in, with full certificate verification.
//
// A request goes out in one of two modes: HTTP (net/http over that dialer:
// HTTP/1.1, keep-alive, one TLS session cache per Client) or Raw (HTTP/1.1
// written by hand with the headers in the given order and case, one
// connection per request, closed at the TCP level without a TLS alert).
// Headers, specs, JSON codecs, constants and base URLs belong to the
// providers.
package transport

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	utls "github.com/refraction-networking/utls"
	"golang.org/x/net/http/httpguts"

	"github.com/a1ekseev/scd/internal/common/neterr"
	"github.com/a1ekseev/scd/internal/wgp/provider"
)

// Mode selects how a request is sent.
type Mode int

const (
	// HTTP sends through net/http: HTTP/1.1, keep-alive, the net/http header
	// order and transparent gzip.
	HTTP Mode = iota
	// Raw writes HTTP/1.1 by hand on a new connection per request.
	Raw
)

// maxBody limits a decoded response body.
const maxBody = 32 << 20

// maxRetryAfter caps a Retry-After pause (the context still bounds it).
const maxRetryAfter = time.Hour

// maxMessage limits StatusError.Message in bytes.
const maxMessage = 200

// Header is one request header, written exactly as given.
type Header struct {
	Name, Value string
}

// Request is one API call.
type Request struct {
	// Method is the HTTP method.
	Method string
	// Path is an absolute path with an optional query, appended to the
	// base URL path.
	Path string
	// Header is written in this order and case in Raw mode (Host and
	// Content-Length included: Raw adds nothing). In HTTP mode "Host" sets
	// the request host and net/http orders the rest.
	Header []Header
	// Body is the request body, never logged or put in an error.
	Body []byte
}

// Response is an API answer with its body decoded (gzip, deflate).
type Response struct {
	Status int
	Header http.Header
	Body   []byte
}

// Config builds a Client.
type Config struct {
	// BaseURL is https://host[:port][/path], without query or fragment.
	BaseURL string
	// Mode is HTTP or Raw.
	Mode Mode
	// Spec returns a new ClientHello spec for each connection.
	Spec func() *utls.ClientHelloSpec
	// Prepare, if set, runs after the spec is applied and before the
	// handshake (for example to adjust the built hello).
	Prepare func(*utls.UConn) error
	// RootCAs verifies the server; nil uses the system roots.
	RootCAs *x509.CertPool
	// API is the timeout and retry policy.
	API provider.API
}

// StatusError is a response with a status of 400 or more. It holds only the
// status and the API's Code and Error fields, never the body.
type StatusError struct {
	// Status is the HTTP status.
	Status int
	// Code is the API error code, 0 if none.
	Code int
	// Message is the API error text, truncated.
	Message string
}

// Error implements error.
func (e *StatusError) Error() string {
	s := "status " + strconv.Itoa(e.Status)
	if e.Code != 0 {
		s += ", code " + strconv.Itoa(e.Code)
	}
	if e.Message != "" {
		s += ": " + strconv.Quote(e.Message)
	}
	return s
}

// Client sends requests to one base URL.
type Client struct {
	cfg        Config
	host, addr string
	authority  string // host[:port] as in the base URL
	basePath   string
	http       *http.Client
	sleep      func(context.Context, time.Duration) error
}

// New checks cfg and returns its Client.
func New(cfg Config) (*Client, error) {
	u, err := url.Parse(cfg.BaseURL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("transport: base URL must be https://host[:port][/path]")
	}
	if cfg.Spec == nil {
		return nil, errors.New("transport: no ClientHello spec")
	}
	if cfg.Mode != HTTP && cfg.Mode != Raw {
		return nil, fmt.Errorf("transport: unknown mode %d", cfg.Mode)
	}
	port := u.Port()
	if port == "" {
		port = "443"
	}
	c := &Client{
		cfg:       cfg,
		host:      u.Hostname(),
		addr:      net.JoinHostPort(u.Hostname(), port),
		authority: u.Host,
		basePath:  strings.TrimSuffix(u.EscapedPath(), "/"),
		sleep:     sleep,
	}
	if cfg.Mode == HTTP {
		sessions := utls.NewLRUClientSessionCache(0)
		c.http = &http.Client{
			Transport: &http.Transport{
				DialTLSContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					tc, _, err := c.dial(ctx, sessions)
					return tc, err
				},
				IdleConnTimeout: 90 * time.Second,
			},
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	}
	return c, nil
}

// Do sends req and retries it: 429 and 503 after Retry-After (seconds or a
// date), else after a random pause of retry.minDelayMs..maxDelayMs; 408 and
// 502 at once; at most retry.attempts tries. Network errors and timeouts are
// not retried; their text has no local address (neterr.Stable). A status of 400 or more returns the last Response and a
// *StatusError.
func (c *Client) Do(ctx context.Context, req Request) (Response, error) {
	if err := c.check(req); err != nil {
		return Response{}, err
	}
	for attempt := 1; ; attempt++ {
		resp, err := c.once(ctx, req)
		if err != nil {
			return Response{}, fmt.Errorf("transport: %s: %w", req.Method, neterr.Stable(err))
		}
		if resp.Status < 400 {
			return resp, nil
		}
		delay, retry := c.backoff(resp, time.Now())
		if !retry || attempt >= c.cfg.API.Retry.Attempts {
			return resp, statusError(resp)
		}
		if err := c.sleep(ctx, delay); err != nil {
			return Response{}, fmt.Errorf("transport: %s: %w", req.Method, err)
		}
	}
}

// check rejects a request that would break the request line or headers.
func (c *Client) check(req Request) error {
	if !httpguts.ValidHeaderFieldName(req.Method) {
		return errors.New("transport: bad method")
	}
	if !strings.HasPrefix(req.Path, "/") || strings.ContainsFunc(req.Path, func(r rune) bool { return r <= ' ' || r >= 0x7f }) {
		return errors.New("transport: bad path")
	}
	for _, h := range req.Header {
		if !httpguts.ValidHeaderFieldName(h.Name) || !httpguts.ValidHeaderFieldValue(h.Value) {
			return fmt.Errorf("transport: bad header %q", h.Name)
		}
		if c.cfg.Mode == Raw && strings.EqualFold(h.Name, "Content-Length") && h.Value != strconv.Itoa(len(req.Body)) {
			return errors.New("transport: Content-Length does not match the body")
		}
	}
	return nil
}

// backoff says whether resp is retried and after which pause.
func (c *Client) backoff(resp Response, now time.Time) (time.Duration, bool) {
	switch resp.Status {
	case http.StatusRequestTimeout, http.StatusBadGateway:
		return 0, true
	case http.StatusTooManyRequests, http.StatusServiceUnavailable:
		if d, ok := retryAfter(resp.Header.Get("Retry-After"), now); ok {
			return d, true
		}
		r := c.cfg.API.Retry
		return time.Duration(r.MinDelayMs+rand.IntN(r.MaxDelayMs-r.MinDelayMs+1)) * time.Millisecond, true
	}
	return 0, false
}

// retryAfter parses a Retry-After value: delay seconds or an HTTP date.
func retryAfter(v string, now time.Time) (time.Duration, bool) {
	v = strings.TrimSpace(v)
	if s, err := strconv.ParseUint(v, 10, 64); err == nil {
		return time.Duration(min(s, uint64(maxRetryAfter/time.Second))) * time.Second, true
	}
	if t, err := http.ParseTime(v); err == nil {
		return min(max(t.Sub(now), 0), maxRetryAfter), true
	}
	return 0, false
}

// sleep waits d or until ctx ends.
func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// statusError keeps only the Code and Error fields of a JSON body.
func statusError(resp Response) *StatusError {
	e := &StatusError{Status: resp.Status}
	var v struct {
		Code  int    `json:"Code"`
		Error string `json:"Error"`
	}
	if json.Unmarshal(resp.Body, &v) == nil {
		e.Code = v.Code
		e.Message = strings.ToValidUTF8(v.Error, "")
		for len(e.Message) > maxMessage {
			_, n := utf8.DecodeLastRuneInString(e.Message)
			e.Message = e.Message[:len(e.Message)-n]
		}
	}
	return e
}

// once makes one attempt within api.timeoutMs.
func (c *Client) once(ctx context.Context, req Request) (Response, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Duration(c.cfg.API.TimeoutMs)*time.Millisecond)
	defer cancel()
	if c.cfg.Mode == Raw {
		return c.raw(ctx, req)
	}
	return c.viaHTTP(ctx, req)
}

// dial connects and completes the uTLS handshake. raw is the TCP connection
// under tc.
func (c *Client) dial(ctx context.Context, sessions utls.ClientSessionCache) (tc *utls.UConn, raw net.Conn, err error) {
	var d net.Dialer
	raw, err = d.DialContext(ctx, "tcp", c.addr)
	if err != nil {
		return nil, nil, err
	}
	tc = utls.UClient(raw, &utls.Config{ServerName: c.host, RootCAs: c.cfg.RootCAs, ClientSessionCache: sessions}, utls.HelloCustom)
	if err = tc.ApplyPreset(c.cfg.Spec()); err == nil && c.cfg.Prepare != nil {
		err = c.cfg.Prepare(tc)
	}
	if err == nil {
		err = tc.HandshakeContext(ctx)
	}
	if err != nil {
		_ = raw.Close()
		return nil, nil, err
	}
	return tc, raw, nil
}

// viaHTTP sends req through net/http.
func (c *Client) viaHTTP(ctx context.Context, req Request) (Response, error) {
	var body io.Reader
	if req.Body != nil {
		body = bytes.NewReader(req.Body)
	}
	hr, err := http.NewRequestWithContext(ctx, req.Method, "https://"+c.authority+c.basePath+req.Path, body)
	if err != nil {
		return Response{}, errors.New("bad request")
	}
	for _, h := range req.Header {
		if strings.EqualFold(h.Name, "Host") {
			hr.Host = h.Value
			continue
		}
		hr.Header[h.Name] = append(hr.Header[h.Name], h.Value) // case kept as given
	}
	resp, err := c.http.Do(hr)
	if err != nil {
		if ue, ok := errors.AsType[*url.Error](err); ok {
			err = ue.Err // drop the URL
		}
		return Response{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	return read(resp)
}

// raw writes req by hand on a new connection and closes the TCP connection
// after the response, without a TLS close_notify.
func (c *Client) raw(ctx context.Context, req Request) (Response, error) {
	tc, conn, err := c.dial(ctx, nil)
	if err != nil {
		return Response{}, err
	}
	defer func() { _ = conn.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()

	var b bytes.Buffer
	b.WriteString(req.Method + " " + c.basePath + req.Path + " HTTP/1.1\r\n")
	for _, h := range req.Header {
		b.WriteString(h.Name + ": " + h.Value + "\r\n")
	}
	b.WriteString("\r\n")
	b.Write(req.Body)
	if _, err := tc.Write(b.Bytes()); err != nil {
		return Response{}, ctxErr(ctx, err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(tc), &http.Request{Method: req.Method})
	if err != nil {
		return Response{}, ctxErr(ctx, err)
	}
	defer func() { _ = resp.Body.Close() }()
	r, err := read(resp)
	return r, ctxErr(ctx, err)
}

// ctxErr prefers the context error to the closed-connection error it causes.
func ctxErr(ctx context.Context, err error) error {
	if err != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

// read reads and decodes the body of resp, at most maxBody bytes.
func read(resp *http.Response) (Response, error) {
	var body io.Reader = resp.Body
	var err error
	switch enc := strings.ToLower(resp.Header.Get("Content-Encoding")); enc {
	case "", "identity":
	case "gzip":
		body, err = gzip.NewReader(body)
	case "deflate":
		body, err = zlib.NewReader(body) // shortcut: zlib-wrapped only (RFC 9110), a raw deflate stream fails; add flate if an API sends one
	default:
		return Response{}, fmt.Errorf("unsupported Content-Encoding %q", enc)
	}
	if err != nil {
		return Response{}, fmt.Errorf("decode response: %w", err)
	}
	b, err := io.ReadAll(io.LimitReader(body, maxBody+1))
	if err != nil {
		return Response{}, fmt.Errorf("read response: %w", err)
	}
	if len(b) > maxBody {
		return Response{}, fmt.Errorf("response body exceeds %d bytes", maxBody)
	}
	return Response{Status: resp.StatusCode, Header: resp.Header, Body: b}, nil
}
