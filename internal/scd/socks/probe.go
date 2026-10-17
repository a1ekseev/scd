// Package socks implements the SOCKS5 + HTTP(S) probe used by the tunnel
// monitor to check health through Xray SOCKS inbounds.
//
// The whole probe (TCP connect, SOCKS5 handshake, optional TLS handshake and
// the HTTP request) shares a single deadline of ctx + Request.Timeout (D23).
// When it expires the error names the phase that was in progress.
package socks

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Request describes one probe.
type Request struct {
	ProxyHost string
	ProxyPort int
	URL       string
	Method    string
	Timeout   time.Duration
	// RootCAs is a test hook; nil in production (system roots).
	RootCAs *x509.CertPool
	// Dial is a test hook; nil means (&net.Dialer{}).DialContext.
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
}

// Response is the result of a successful probe.
type Response struct {
	StatusCode int
	LatencyMs  int
	BodyBytes  int
}

var statusLineRe = regexp.MustCompile(`^HTTP/1\.[01]\s+(\d{3})`)

// verbatim exists so TS-contract texts (capitalised, trailing period) bypass error-string lint.
// It builds an error whose text is part of the TS-compatible contract
// (capitalised, with a trailing period; it reaches lastError and Remote Push).
func verbatim(format string, args ...any) error {
	return fmt.Errorf(format, args...)
}

type phase int

const (
	phaseConnect phase = iota
	phaseHandshake
	phaseTLS
	phaseRequest
)

func (p phase) timeoutMessage(ms int64) string {
	switch p {
	case phaseConnect:
		return fmt.Sprintf("SOCKS5 proxy connection timed out after %dms.", ms)
	case phaseHandshake:
		return fmt.Sprintf("SOCKS5 handshake timed out after %dms.", ms)
	case phaseTLS:
		return fmt.Sprintf("SOCKS HTTPS TLS handshake timed out after %dms.", ms)
	default:
		return fmt.Sprintf("SOCKS HTTP request timed out after %dms.", ms)
	}
}

type target struct {
	host    string // domain for ATYP 3 / Host header (IPv6 keeps brackets for the header only)
	port    int
	hostHdr string
	path    string
	useTLS  bool
	sniHost string
	isIPv4  bool
	ipv4    [4]byte
	isIPv6  bool
	ipv6    [16]byte
}

func invalidURL(raw string) error {
	return fmt.Errorf("invalid URL: %s", raw)
}

func parseTarget(raw string) (target, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return target{}, invalidURL(raw)
	}
	var t target
	t.useTLS = u.Scheme == "https"
	defPort := 80
	if t.useTLS {
		defPort = 443
	}
	t.port = defPort
	if ps := u.Port(); ps != "" {
		n, err := strconv.Atoi(ps)
		if err != nil || n < 0 || n > 65535 {
			return target{}, invalidURL(raw)
		}
		t.port = n
	}
	name := strings.ToLower(u.Hostname())
	t.sniHost = name
	t.host = name
	if strings.Contains(name, ":") {
		t.host = "[" + name + "]"
	}
	t.hostHdr = t.host
	if t.port != defPort {
		t.hostHdr += ":" + strconv.Itoa(t.port)
	}
	if a, err := netip.ParseAddr(name); err == nil {
		switch {
		case a.Is4():
			t.isIPv4 = true
			t.ipv4 = a.As4()
		case a.Is6():
			t.isIPv6 = true
			t.ipv6 = a.As16()
		}
	}
	if !t.isIPv4 && !t.isIPv6 && len(name) > 255 {
		return target{}, invalidURL(raw)
	}
	path := u.EscapedPath()
	if path == "" {
		path = "/"
	}
	if u.RawQuery != "" {
		path += "?" + u.RawQuery
	}
	t.path = path
	return t, nil
}

func connectRequest(t target) []byte {
	b := []byte{5, 1, 0}
	switch {
	case t.isIPv4:
		b = append(b, 1)
		b = append(b, t.ipv4[:]...)
	case t.isIPv6:
		b = append(b, 4)
		b = append(b, t.ipv6[:]...)
	default:
		b = append(b, 3, byte(len(t.host)))
		b = append(b, t.host...)
	}
	return append(b, byte(t.port>>8), byte(t.port))
}

// probe carries the state shared by the phases.
type probe struct {
	ctx   context.Context
	ms    int64
	phase phase
	conn  net.Conn
}

// fail maps an I/O error in the current phase to the probe error.
func (p *probe) fail(err error) error {
	var ne net.Error
	if errors.Is(p.ctx.Err(), context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()) {
		return verbatim("%s", p.phase.timeoutMessage(p.ms))
	}
	if cerr := p.ctx.Err(); cerr != nil {
		return cerr
	}
	return err
}

// readFull reads exactly len(buf) bytes of a SOCKS reply. first marks the
// first read of the handshake, where an EOF before any byte is the plain
// "Unexpected EOF from SOCKS proxy.".
func (p *probe) readFull(buf []byte, first bool) error {
	_, err := io.ReadFull(p.conn, buf)
	if err == nil {
		return nil
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		if err == io.EOF && first { //nolint:errorlint // ReadFull returns bare io.EOF only for zero bytes
			return verbatim("Unexpected EOF from SOCKS proxy.")
		}
		return verbatim("Unexpected EOF while reading %d bytes from SOCKS proxy.", len(buf))
	}
	return p.fail(err)
}

func (p *probe) handshake(t target) error {
	if _, err := p.conn.Write([]byte{5, 1, 0}); err != nil {
		return p.fail(err)
	}
	g := make([]byte, 2)
	if err := p.readFull(g, true); err != nil {
		return err
	}
	if g[0] != 5 || g[1] != 0 {
		return verbatim("SOCKS5 proxy does not accept no-auth authentication.")
	}
	if _, err := p.conn.Write(connectRequest(t)); err != nil {
		return p.fail(err)
	}
	head := make([]byte, 4)
	if err := p.readFull(head, false); err != nil {
		return err
	}
	if head[0] != 5 || head[1] != 0 {
		return verbatim("SOCKS5 connect failed with code %d.", head[1])
	}
	skip := 0
	switch head[3] {
	case 1:
		skip = 6
	case 4:
		skip = 18
	case 3:
		l := make([]byte, 1)
		if err := p.readFull(l, false); err != nil {
			return err
		}
		skip = int(l[0]) + 2
	}
	if skip > 0 {
		return p.readFull(make([]byte, skip), false)
	}
	return nil
}

// readHead reads until CRLFCRLF or EOF and returns everything received.
func (p *probe) readHead(c net.Conn) ([]byte, error) {
	var buf []byte
	tmp := make([]byte, 4096)
	for !bytes.Contains(buf, []byte("\r\n\r\n")) {
		n, err := c.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				break
			}
			return nil, p.fail(err)
		}
	}
	return buf, nil
}

func parseHead(resp []byte) (status, bodyBytes int, err error) {
	sep := bytes.Index(resp, []byte("\r\n\r\n"))
	if sep < 0 {
		return 0, 0, verbatim("Invalid HTTP response from monitored endpoint.")
	}
	line, _, _ := strings.Cut(string(resp[:sep]), "\r\n")
	m := statusLineRe.FindStringSubmatch(line)
	if m == nil {
		return 0, 0, verbatim("Unable to parse HTTP status line: %s", line)
	}
	status, _ = strconv.Atoi(m[1])
	return status, len(resp) - sep - 4, nil
}

// Do runs the probe under one deadline of ctx + r.Timeout.
func Do(ctx context.Context, r Request) (Response, error) {
	t, err := parseTarget(r.URL)
	if err != nil {
		return Response{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, r.Timeout)
	defer cancel()
	p := &probe{ctx: ctx, ms: r.Timeout.Milliseconds(), phase: phaseConnect}

	dial := r.Dial
	if dial == nil {
		dial = (&net.Dialer{}).DialContext
	}
	raw, err := dial(ctx, "tcp", net.JoinHostPort(r.ProxyHost, strconv.Itoa(r.ProxyPort)))
	if err != nil {
		return Response{}, p.fail(err)
	}
	defer raw.Close()
	// Parent cancellation closes the connection, which unblocks any pending I/O.
	defer context.AfterFunc(ctx, func() { _ = raw.Close() })()
	if dl, ok := ctx.Deadline(); ok {
		_ = raw.SetDeadline(dl)
	}
	p.conn = raw

	p.phase = phaseHandshake
	if err := p.handshake(t); err != nil {
		return Response{}, err
	}

	stream := raw
	if t.useTLS {
		p.phase = phaseTLS
		tc := tls.Client(raw, &tls.Config{ServerName: t.sniHost, RootCAs: r.RootCAs, MinVersion: tls.VersionTLS12})
		if err := tc.HandshakeContext(ctx); err != nil {
			return Response{}, p.fail(err)
		}
		stream = tc
	}

	p.phase = phaseRequest
	var sb strings.Builder
	sb.WriteString(r.Method + " " + t.path + " HTTP/1.1\r\nHost: " + t.hostHdr + "\r\nConnection: close\r\n")
	if r.Method == http.MethodPost {
		sb.WriteString("Content-Length: 0\r\n")
	}
	sb.WriteString("\r\n")
	started := time.Now()
	if _, err := stream.Write([]byte(sb.String())); err != nil {
		return Response{}, p.fail(err)
	}
	head, err := p.readHead(stream)
	if err != nil {
		return Response{}, err
	}
	latency := int(math.Round(float64(time.Since(started)) / float64(time.Millisecond)))
	status, body, err := parseHead(head)
	if err != nil {
		return Response{}, err
	}
	return Response{StatusCode: status, LatencyMs: latency, BodyBytes: body}, nil
}
