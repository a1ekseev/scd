package warp

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/a1ekseev/scd/internal/wgp/provider"
	"github.com/a1ekseev/scd/internal/wgp/transport"
)

var testID = Identification{
	UserAgent:     "1.1.1.1/6.38.9-5641 (Android 16.0.0)",
	ClientVersion: "a-6.38.9-5641",
	APIVersion:    "v0a5641",
	Model:         "PC",
	Locale:        "en_US",
	OSVersion:     "16.0.0",
}

// hello is a field view of one ClientHello record, the format of
// testdata/warp-clienthello.json.
type hello struct {
	About         string      `json:"about"`
	RecordLen     int         `json:"record_len"`
	LegacyVersion string      `json:"legacy_version"`
	SessionIDLen  int         `json:"session_id_len"`
	Ciphers       []string    `json:"ciphers"`
	Compression   []int       `json:"compression"`
	Extensions    []extension `json:"extensions"`
	JA3           string      `json:"ja3"`
	JA3Hash       string      `json:"ja3_hash"`
	JA4           string      `json:"ja4"`
	Normalized    string      `json:"normalized_hex"` // random and session_id zeroed
}

type extension struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
	Len  int    `json:"len"`
	Data string `json:"data_hex"`
}

var extNames = map[uint16]string{
	0: "server_name", 5: "status_request", 10: "supported_groups", 11: "ec_point_formats",
	13: "signature_algorithms", 16: "alpn", 21: "padding", 23: "extended_master_secret",
	35: "session_ticket", 65281: "renegotiation_info",
}

// parseHello parses a ClientHello record and computes JA3 and JA4.
// shortcut: no GREASE filtering and the version is legacy_version (TLS 1.2
// hellos only); extend it when a spec with GREASE or TLS 1.3 is tested.
func parseHello(t *testing.T, rec []byte) hello {
	t.Helper()
	h := hello{RecordLen: int(binary.BigEndian.Uint16(rec[3:]))}
	norm := slices.Clone(rec)
	b := rec[9:]
	legacy := binary.BigEndian.Uint16(b)
	h.LegacyVersion = fmt.Sprintf("0x%04x", legacy)
	clear(norm[11:43])
	h.SessionIDLen = int(b[34])
	clear(norm[44 : 44+h.SessionIDLen])
	o := 35 + h.SessionIDLen
	n := int(binary.BigEndian.Uint16(b[o:]))
	var ciphers, exts, groups, sigs []uint16
	for i := o + 2; i < o+2+n; i += 2 {
		ciphers = append(ciphers, binary.BigEndian.Uint16(b[i:]))
		h.Ciphers = append(h.Ciphers, fmt.Sprintf("0x%04x", ciphers[len(ciphers)-1]))
	}
	o += 2 + n
	for _, c := range b[o+1 : o+1+int(b[o])] {
		h.Compression = append(h.Compression, int(c))
	}
	o += 1 + int(b[o])
	end := o + 2 + int(binary.BigEndian.Uint16(b[o:]))
	var points []string
	alpn := "00"
	for o += 2; o < end; {
		id, l := binary.BigEndian.Uint16(b[o:]), int(binary.BigEndian.Uint16(b[o+2:]))
		d := b[o+4 : o+4+l]
		h.Extensions = append(h.Extensions, extension{ID: int(id), Name: extNames[id], Len: l, Data: hex.EncodeToString(d)})
		exts = append(exts, id)
		switch id {
		case 10:
			for i := 2; i < l; i += 2 {
				groups = append(groups, binary.BigEndian.Uint16(d[i:]))
			}
		case 11:
			for _, p := range d[1:] {
				points = append(points, strconv.Itoa(int(p)))
			}
		case 13:
			for i := 2; i < l; i += 2 {
				sigs = append(sigs, binary.BigEndian.Uint16(d[i:]))
			}
		case 16:
			p := string(d[3 : 3+int(d[2])])
			alpn = p[:1] + p[len(p)-1:]
		}
		o += 4 + l
	}
	h.Normalized = hex.EncodeToString(norm)

	join := func(v []uint16, sep, f string) string {
		s := make([]string, len(v))
		for i, x := range v {
			s[i] = fmt.Sprintf(f, x)
		}
		return strings.Join(s, sep)
	}
	h.JA3 = fmt.Sprintf("%d,%s,%s,%s,%s", legacy, join(ciphers, "-", "%d"), join(exts, "-", "%d"), join(groups, "-", "%d"), strings.Join(points, "-"))
	sum := md5.Sum([]byte(h.JA3))
	h.JA3Hash = hex.EncodeToString(sum[:])
	sha12 := func(s string) string { s2 := sha256.Sum256([]byte(s)); return hex.EncodeToString(s2[:6]) }
	sorted := slices.Sorted(slices.Values(ciphers))
	rest := slices.Sorted(slices.Values(slices.DeleteFunc(slices.Clone(exts), func(e uint16) bool { return e == 0 || e == 16 })))
	ver := map[uint16]string{0x0303: "12", 0x0302: "11", 0x0301: "10"}[legacy]
	h.JA4 = fmt.Sprintf("t%sd%02d%02d%s_%s_%s", ver, len(ciphers), len(exts), alpn,
		sha12(join(sorted, ",", "%04x")), sha12(join(rest, ",", "%04x")+"_"+join(sigs, ",", "%04x")))
	return h
}

// exchange is one connection seen by the test server.
type exchange struct {
	hello    []byte // the ClientHello record
	resumed  bool
	request  []byte // the HTTP request bytes
	hostname string // SNI
}

// prefixConn replays the already read hello before the rest of the stream.
type prefixConn struct {
	net.Conn
	r io.Reader
}

func (c prefixConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// testTLS returns a server config with a certificate for apiHost from a
// new test CA and the pool that trusts it.
func testTLS(t *testing.T) (*tls.Config, *x509.CertPool) {
	t.Helper()
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	ca := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, _ = x509.ParseCertificate(caDER)
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	leafDER, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
		SerialNumber: big.NewInt(2), DNSNames: []string{apiHost},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	return &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{leafDER}, PrivateKey: key}}}, pool
}

// newServer serves TLS on 127.0.0.1 with a certificate for apiHost from a
// test CA. Each connection answers one request with 200 and closes, so the
// next request dials again and can resume. It returns a transport.Config
// that reaches it and the channel of exchanges.
func newServer(t *testing.T) (transport.Config, <-chan exchange) {
	t.Helper()
	cfg, pool := testTLS(t)
	ln, err := new(net.ListenConfig).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	ch := make(chan exchange, 8)
	go func() {
		for {
			raw, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = raw.Close() }()
				_ = raw.SetDeadline(time.Now().Add(10 * time.Second))
				var e exchange
				e.hello = make([]byte, 5)
				if _, err := io.ReadFull(raw, e.hello); err != nil {
					return
				}
				e.hello = append(e.hello, make([]byte, binary.BigEndian.Uint16(e.hello[3:]))...)
				if _, err := io.ReadFull(raw, e.hello[5:]); err != nil {
					return
				}
				tc := tls.Server(prefixConn{raw, io.MultiReader(bytes.NewReader(e.hello), raw)}, cfg)
				if tc.HandshakeContext(context.Background()) != nil {
					return
				}
				e.resumed, e.hostname = tc.ConnectionState().DidResume, tc.ConnectionState().ServerName
				var req bytes.Buffer
				r, err := http.ReadRequest(bufio.NewReader(io.TeeReader(tc, &req)))
				if err != nil {
					return
				}
				_, _ = io.Copy(io.Discard, r.Body)
				e.request = req.Bytes()
				_, _ = io.WriteString(tc, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\n{}")
				ch <- e
			}()
		}
	}()
	addr := ln.Addr().String()
	var api provider.API
	api.TimeoutMs = 5000
	api.Retry.Attempts = 1
	return transport.Config{
		RootCAs: pool,
		API:     api,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return new(net.Dialer).DialContext(ctx, network, addr)
		},
	}, ch
}

// run sends POST /reg, then PATCH /reg/{id} on a new connection, and
// returns both exchanges.
func run(t *testing.T) (reg, patch exchange) {
	t.Helper()
	cfg, ch := newServer(t)
	a, err := newAPI(testID, cfg)
	if err != nil {
		t.Fatal(err)
	}
	tos := time.Date(2026, 10, 29, 15, 0, 0, 123456789, time.FixedZone("", 3*3600))
	if _, err := a.do(t.Context(), http.MethodPost, "/reg", "", regBody(testID, strings.Repeat("A", 43)+"=", tos)); err != nil {
		t.Fatal(err)
	}
	reg = <-ch
	if _, err := a.do(t.Context(), http.MethodPatch, "/reg/00000000-0000-4000-8000-000000000001", "fake-token",
		keyBody(strings.Repeat("B", 43)+"=", "curve25519", "wireguard")); err != nil {
		t.Fatal(err)
	}
	return reg, <-ch
}

func TestClientHello(t *testing.T) {
	reg, patch := run(t)

	// testdata/warp-clienthello.json: the WARP API ClientHello (a fresh
	// hello: TLS 1.2, 161 bytes, no GREASE, no padding).
	b, err := os.ReadFile("testdata/warp-clienthello.json")
	if err != nil {
		t.Fatal(err)
	}
	var want hello
	if err := json.Unmarshal(b, &want); err != nil {
		t.Fatal(err)
	}
	got := parseHello(t, reg.hello)
	got.About = want.About
	gj, _ := json.MarshalIndent(got, "", "  ")
	wj, _ := json.MarshalIndent(want, "", "  ")
	if !bytes.Equal(gj, wj) {
		t.Errorf("fresh hello differs from the reference:\n%s\nwant:\n%s", gj, wj)
	}
	if reg.resumed || reg.hostname != apiHost {
		t.Errorf("fresh: resumed %v, SNI %q", reg.resumed, reg.hostname)
	}

	// The resumed hello offers the ticket with a random 32-byte session_id,
	// and BoringSSL padding brings it to 512 bytes.
	r := parseHello(t, patch.hello)
	ids := func(h hello) []int {
		var v []int
		for _, e := range h.Extensions {
			v = append(v, e.ID)
		}
		return v
	}
	ticket := r.Extensions[slices.Index(ids(r), 35)].Len
	if !patch.resumed || r.SessionIDLen != 32 || ticket == 0 || r.RecordLen != 512 ||
		!slices.Equal(ids(r), append(ids(want), 21)) ||
		r.JA3Hash != "c60d1acd4167eb7d73d90211bd20f245" || r.JA4 != "t12d0210h1_8e6a03d994ba_f88f2b2eb673" {
		t.Errorf("resumed %v, session_id %d, ticket %d, record %d, extensions %v, JA3 %s, JA4 %s",
			patch.resumed, r.SessionIDLen, ticket, r.RecordLen, ids(r), r.JA3Hash, r.JA4)
	}
	if bytes.Equal(patch.hello[44:76], make([]byte, 32)) {
		t.Error("resumed session_id is all zero")
	}
}

func TestRequests(t *testing.T) {
	reg, patch := run(t)
	for name, got := range map[string][]byte{"reg.http": reg.request, "patch-reg.http": patch.request} {
		want, err := os.ReadFile("testdata/warp-requests/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s:\n%q\nwant:\n%q", name, got, want)
		}
	}
}
