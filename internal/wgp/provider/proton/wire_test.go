package proton

import (
	"bufio"
	"bytes"
	"compress/gzip"
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
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/a1ekseev/scd/internal/wgp/provider"
	"github.com/a1ekseev/scd/internal/wgp/transport"
)

var testID = Identification{
	AppVersion: "linux-vpn-gui@5.8.11+aarch64",
	UserAgent:  "ProtonVPN/5.8.11 (Linux; ubuntu/26.04)",
	Timezone:   "Europe/Istanbul",
}

// exchange is one connection seen by the test server.
type exchange struct {
	hello   []byte // the ClientHello record
	request []byte // the HTTP request bytes
	state   tls.ConnectionState
	after   int   // bytes the client sent after the response (a TLS alert)
	err     error // a server-side failure
}

// prefixConn replays the already read hello before the rest of the stream.
type prefixConn struct {
	net.Conn
	r io.Reader
}

func (c prefixConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// respBody is the decoded answer of the test server; it goes out gzipped.
const respBody = `{"Code": 1000}`

// testCert returns a pool with a new test CA and a certificate for apiHost
// issued by it.
func testCert(t *testing.T) (*x509.CertPool, tls.Certificate) {
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
	return pool, tls.Certificate{Certificate: [][]byte{leafDER}, PrivateKey: key}
}

// newServer serves TLS on 127.0.0.1 with a certificate for apiHost from a
// new test CA. Each connection answers one request with a gzipped 200 and
// then waits for the client to close. It returns a transport.Config that
// reaches it and the channel of exchanges.
func newServer(t *testing.T) (transport.Config, <-chan exchange) {
	t.Helper()
	pool, cert := testCert(t)
	cfg := &tls.Config{Certificates: []tls.Certificate{cert}}

	var gz bytes.Buffer
	w := gzip.NewWriter(&gz)
	_, _ = io.WriteString(w, respBody)
	_ = w.Close()
	resp := "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Encoding: gzip\r\nContent-Length: " +
		strconv.Itoa(gz.Len()) + "\r\n\r\n" + gz.String()

	ln, err := new(net.ListenConfig).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	ch := make(chan exchange, 32)
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
				defer func() { ch <- e }()
				e.hello = make([]byte, 5)
				if _, e.err = io.ReadFull(raw, e.hello); e.err != nil {
					return
				}
				e.hello = append(e.hello, make([]byte, binary.BigEndian.Uint16(e.hello[3:]))...)
				if _, e.err = io.ReadFull(raw, e.hello[5:]); e.err != nil {
					return
				}
				tc := tls.Server(prefixConn{raw, io.MultiReader(bytes.NewReader(e.hello), raw)}, cfg)
				if e.err = tc.HandshakeContext(context.Background()); e.err != nil {
					return
				}
				e.state = tc.ConnectionState()
				if e.request, e.err = readRequest(bufio.NewReader(tc)); e.err != nil {
					return
				}
				if _, e.err = io.WriteString(tc, resp); e.err != nil {
					return
				}
				n, err := io.Copy(io.Discard, raw) // until the client closes TCP
				e.after, e.err = int(n), err
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

// readRequest reads one request head and its Content-Length body as bytes.
func readRequest(r *bufio.Reader) ([]byte, error) {
	var b bytes.Buffer
	n := 0
	for {
		line, err := r.ReadString('\n')
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
	_, err := io.CopyN(&b, r, int64(n))
	return b.Bytes(), err
}

// send makes one call and returns what the server saw. The response must
// be the decoded gzip body, and the client must close without an alert.
func send(t *testing.T, a *api, ch <-chan exchange, c call) exchange {
	t.Helper()
	resp, err := a.do(t.Context(), c)
	if err != nil || resp.Status != http.StatusOK || string(resp.Body) != respBody {
		t.Fatalf("%s %s: %d %q %v", c.method, c.path, resp.Status, resp.Body, err)
	}
	e := <-ch
	if e.err != nil {
		t.Fatalf("%s %s: server: %v", c.method, c.path, e.err)
	}
	if e.after != 0 {
		t.Errorf("%s %s: client sent %d bytes after the response (TLS alert)", c.method, c.path, e.after)
	}
	return e
}

func newTestAPI(t *testing.T) (*api, <-chan exchange) {
	t.Helper()
	cfg, ch := newServer(t)
	a, err := newAPI(testID, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return a, ch
}

// helloView is the field view of a ClientHello, a subset of the format of
// testdata/proton-clienthello.json.
type helloView struct {
	Record struct {
		ContentType int `json:"content_type"`
		Version     int `json:"version"`
		Length      int `json:"length"`
	} `json:"record"`
	Handshake struct {
		Type          int `json:"type"`
		Length        int `json:"length"`
		LegacyVersion int `json:"legacy_version"`
	} `json:"handshake"`
	SessionID struct {
		Length int `json:"length"`
	} `json:"session_id"`
	CipherSuites       []uint16  `json:"cipher_suites"`
	CompressionMethods []int     `json:"compression_methods"`
	Extensions         []helloEx `json:"extensions"`
	JA3String          string    `json:"ja3_string"`
	JA3                string    `json:"ja3"`
	JA4                string    `json:"ja4"`
}

type helloEx struct {
	Type       uint16     `json:"type"`
	Name       string     `json:"name"`
	Length     int        `json:"length"`
	Data       string     `json:"data_hex,omitempty"`
	ServerName string     `json:"server_name,omitempty"`
	Formats    []int      `json:"formats,omitempty"`
	Groups     []uint16   `json:"groups,omitempty"`
	Protocols  []string   `json:"protocols,omitempty"`
	Algorithms []uint16   `json:"algorithms,omitempty"`
	Versions   []uint16   `json:"versions,omitempty"`
	Modes      []int      `json:"modes,omitempty"`
	Entries    []keyShare `json:"entries,omitempty"`
}

type keyShare struct {
	Group     uint16 `json:"group"`
	KeyLength int    `json:"key_length"`
	key       []byte
}

// golden is testdata/proton-clienthello.json.
type golden struct {
	helloView
	RandomRanges []struct {
		Offset int    `json:"offset"`
		Length int    `json:"length"`
		Field  string `json:"field"`
	} `json:"random_ranges"`
	Template string `json:"template_hex"`
}

var extNames = map[uint16]string{
	0: "server_name", 10: "supported_groups", 11: "ec_point_formats", 13: "signature_algorithms",
	16: "alpn", 22: "encrypt_then_mac", 23: "extended_master_secret", 27: "compress_certificate",
	35: "session_ticket", 43: "supported_versions", 45: "psk_key_exchange_modes", 51: "key_share",
	65281: "renegotiation_info",
}

// u16s reads a list of big-endian uint16.
func u16s(b []byte) []uint16 {
	var v []uint16
	for i := 0; i+1 < len(b); i += 2 {
		v = append(v, binary.BigEndian.Uint16(b[i:]))
	}
	return v
}

func ints(b []byte) []int {
	v := make([]int, len(b))
	for i, x := range b {
		v[i] = int(x)
	}
	return v
}

// parseHello parses a ClientHello record and computes JA3 and JA4.
// shortcut: no GREASE filtering (the spec has none); add it with a spec
// that sends GREASE.
func parseHello(rec []byte) helloView {
	var h helloView
	h.Record.ContentType, h.Record.Version, h.Record.Length = int(rec[0]), int(binary.BigEndian.Uint16(rec[1:])), int(binary.BigEndian.Uint16(rec[3:]))
	b := rec[5:]
	h.Handshake.Type, h.Handshake.Length = int(b[0]), int(b[1])<<16|int(b[2])<<8|int(b[3])
	b = b[4:]
	h.Handshake.LegacyVersion = int(binary.BigEndian.Uint16(b))
	h.SessionID.Length = int(b[34])
	o := 35 + h.SessionID.Length
	n := int(binary.BigEndian.Uint16(b[o:]))
	h.CipherSuites = u16s(b[o+2 : o+2+n])
	o += 2 + n
	h.CompressionMethods = ints(b[o+1 : o+1+int(b[o])])
	o += 1 + int(b[o])
	end := o + 2 + int(binary.BigEndian.Uint16(b[o:]))
	var exts, groups, sigs, versions []uint16
	var points []string
	alpn := "00"
	for o += 2; o < end; {
		id, l := binary.BigEndian.Uint16(b[o:]), int(binary.BigEndian.Uint16(b[o+2:]))
		d := b[o+4 : o+4+l]
		e := helloEx{Type: id, Name: extNames[id], Length: l}
		switch id {
		case 0:
			e.ServerName = string(d[5:])
		case 10:
			e.Groups = u16s(d[2:])
			groups = e.Groups
		case 11:
			e.Formats = ints(d[1:])
			for _, p := range e.Formats {
				points = append(points, strconv.Itoa(p))
			}
		case 13:
			e.Algorithms = u16s(d[2:])
			sigs = e.Algorithms
		case 16:
			for i := 2; i < len(d); i += 1 + int(d[i]) {
				e.Protocols = append(e.Protocols, string(d[i+1:i+1+int(d[i])]))
			}
			p := e.Protocols[0]
			alpn = p[:1] + p[len(p)-1:]
		case 27:
			e.Algorithms = u16s(d[1:])
		case 43:
			e.Versions = u16s(d[1:])
			versions = e.Versions
		case 45:
			e.Modes = ints(d[1:])
		case 51:
			for i := 2; i < len(d); {
				k := int(binary.BigEndian.Uint16(d[i+2:]))
				e.Entries = append(e.Entries, keyShare{Group: binary.BigEndian.Uint16(d[i:]), KeyLength: k, key: d[i+4 : i+4+k]})
				i += 4 + k
			}
		default:
			e.Data = hex.EncodeToString(d)
		}
		h.Extensions = append(h.Extensions, e)
		exts = append(exts, id)
		o += 4 + l
	}

	join := func(v []uint16, sep, f string) string {
		s := make([]string, len(v))
		for i, x := range v {
			s[i] = fmt.Sprintf(f, x)
		}
		return strings.Join(s, sep)
	}
	h.JA3String = fmt.Sprintf("%d,%s,%s,%s,%s", h.Handshake.LegacyVersion, join(h.CipherSuites, "-", "%d"),
		join(exts, "-", "%d"), join(groups, "-", "%d"), strings.Join(points, "-"))
	sum := md5.Sum([]byte(h.JA3String))
	h.JA3 = hex.EncodeToString(sum[:])
	sha12 := func(s string) string { s2 := sha256.Sum256([]byte(s)); return hex.EncodeToString(s2[:6]) }
	ver := map[uint16]string{0x0304: "13", 0x0303: "12"}[slices.Max(versions)]
	sorted := slices.Sorted(slices.Values(h.CipherSuites))
	rest := slices.Sorted(slices.Values(slices.DeleteFunc(slices.Clone(exts), func(e uint16) bool { return e == 0 || e == 16 })))
	h.JA4 = fmt.Sprintf("t%sd%02d%02d%s_%s_%s", ver, len(h.CipherSuites), len(exts), alpn,
		sha12(join(sorted, ",", "%04x")), sha12(join(rest, ",", "%04x")+"_"+join(sigs, ",", "%04x")))
	return h
}

func TestClientHello(t *testing.T) {
	// testdata/proton-clienthello.json: the ClientHello of the official
	// Proton Linux client.
	b, err := os.ReadFile("testdata/proton-clienthello.json")
	if err != nil {
		t.Fatal(err)
	}
	var want golden
	if err := json.Unmarshal(b, &want); err != nil {
		t.Fatal(err)
	}
	wj, _ := json.MarshalIndent(want.helloView, "", "  ")
	tmpl, err := hex.DecodeString(want.Template)
	if err != nil {
		t.Fatal(err)
	}

	a, ch := newTestAPI(t)
	var hellos [][]byte
	for range 2 { // every connection sends a fresh, identical hello
		e := send(t, a, ch, call{method: http.MethodGet, path: "/tests/ping"})
		if e.state.Version != tls.VersionTLS13 || e.state.CurveID != tls.X25519MLKEM768 || e.state.DidResume || e.state.ServerName != apiHost {
			t.Errorf("TLS %x, group %v, resumed %v, SNI %q", e.state.Version, e.state.CurveID, e.state.DidResume, e.state.ServerName)
		}
		hellos = append(hellos, e.hello)
	}

	for i, rec := range hellos {
		got := parseHello(rec)
		gj, _ := json.MarshalIndent(got, "", "  ")
		if !bytes.Equal(gj, wj) {
			t.Errorf("hello %d differs from the reference:\n%s\nwant:\n%s", i, gj, wj)
		}
		masked := slices.Clone(rec)
		for _, r := range want.RandomRanges {
			if bytes.Equal(rec[r.Offset:r.Offset+r.Length], make([]byte, r.Length)) {
				t.Errorf("hello %d: %s is all zero", i, r.Field)
			}
			clear(masked[r.Offset : r.Offset+r.Length])
		}
		if !bytes.Equal(masked, tmpl) {
			t.Errorf("hello %d with the random fields masked differs from template_hex:\n%x", i, masked)
		}
		// The hybrid share is ML-KEM-768 then x25519; the x25519 share has
		// its own key.
		ks := got.Extensions[slices.IndexFunc(got.Extensions, func(e helloEx) bool { return e.Type == 51 })].Entries
		if len(ks) != 2 || bytes.Equal(ks[0].key[len(ks[0].key)-32:], ks[1].key) {
			t.Errorf("hello %d: the two x25519 keys are equal", i)
		}
		if len(got.CipherSuites) != 30 || len(got.Extensions[slices.IndexFunc(got.Extensions, func(e helloEx) bool { return e.Type == 13 })].Algorithms) != 26 {
			t.Errorf("hello %d: %d ciphers", i, len(got.CipherSuites))
		}
		if got.JA3 != "8ecf4858a704c8936042f20a8bedee04" || got.JA4 != "t13d3013h1_1d37bd780c83_ecd0401ec68b" {
			t.Errorf("hello %d: JA3 %s, JA4 %s", i, got.JA3, got.JA4)
		}
	}
	if bytes.Equal(hellos[0][11:43], hellos[1][11:43]) {
		t.Error("two hellos share the random")
	}
}

func TestRequests(t *testing.T) {
	const pem = "-----BEGIN PUBLIC KEY-----\nMCowBQYDK2VwAyEAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=\n-----END PUBLIC KEY-----\n"
	vals := map[string]string{
		"arch":                  "aarch64",
		"username":              "test-user",
		"client_ephemeral":      "Q0xJRU5ULUVQSEVNRVJBTA==",
		"client_proof":          "Q0xJRU5ULVBST09G",
		"srp_session":           "00112233445566778899aabbccddeeff",
		"uid":                   "fake-uid-0001",
		"access_token":          "fake-access-token",
		"new_access_token":      "fake-access-token-2",
		"refresh_token":         "fake-refresh-token",
		"totp":                  "123456",
		"client_public_key_pem": strings.ReplaceAll(pem, "\n", `\n`),
		"netzone":               "203.0.113.0",
	}
	uid, tok, tok2 := vals["uid"], vals["access_token"], vals["new_access_token"]
	cert := obj{{"ClientPublicKey", pem}, {"Duration", "10080 min"}}
	get := func(path, token string) call { return call{method: http.MethodGet, path: path, uid: uid, token: token} }
	calls := []struct {
		file string
		c    call
	}{
		{"01-ping.http", call{method: http.MethodGet, path: "/tests/ping"}},
		{"02-auth-info.http", call{method: http.MethodPost, path: "/auth/info", body: obj{{"Username", vals["username"]}}}},
		{"03-auth.http", call{method: http.MethodPost, path: "/auth", body: obj{
			{"Username", vals["username"]},
			{"ClientEphemeral", vals["client_ephemeral"]},
			{"ClientProof", vals["client_proof"]},
			{"SRPSession", vals["srp_session"]},
		}}},
		{"04-auth-2fa.http", call{method: http.MethodPost, path: "/auth/2fa", uid: uid, token: tok, body: obj{{"TwoFactorCode", vals["totp"]}}}},
		{"05-vpn-v2.http", get("/vpn/v2", tok)},
		{"06-certificate.http", call{method: http.MethodPost, path: "/vpn/v1/certificate", uid: uid, token: tok, body: cert}},
		{"07-location.http", get("/vpn/v1/location", tok)},
		{"08-clientconfig.http", get("/vpn/v2/clientconfig", tok)},
		{"09-feature-frontend.http", get("/feature/v2/frontend", tok)},
		{"10-logicals.http", call{
			method: http.MethodGet, path: "/vpn/v1/logicals?SecureCoreFilter=all&WithState=true", uid: uid, token: tok,
			header: []transport.Header{{Name: "X-PM-netzone", Value: vals["netzone"]}, {Name: "If-Modified-Since", Value: "Thu, 01 Jan 1970 00:00:00 GMT"}},
		}},
		{"11-notifications.http", get("/core/v4/notifications", tok)},
		{"12-vpn-v2-expired-401.http", get("/vpn/v2", tok)},
		{"13-refresh.http", call{method: http.MethodPost, path: "/auth/refresh", uid: uid, token: tok, body: obj{
			{"ResponseType", "token"},
			{"GrantType", "refresh_token"},
			{"RefreshToken", vals["refresh_token"]},
			{"RedirectURI", "http://protonmail.ch"},
		}}},
		{"14-vpn-v2-retry.http", get("/vpn/v2", tok2)},
		{"15-certificate-features.http", call{method: http.MethodPost, path: "/vpn/v1/certificate", uid: uid, token: tok2, body: append(slices.Clone(cert),
			kv{"Features", obj{{"RandomNAT", false}, {"SplitTCP", false}, {"PortForwarding", true}, {"NetShieldLevel", 2}}})}},
		{"16-logout.http", call{method: http.MethodDelete, path: "/auth", uid: uid, token: tok2}},
	}
	files, _ := filepath.Glob("testdata/proton-requests/*.http")
	if len(files) != len(calls) {
		t.Fatalf("%d reference files, %d calls", len(files), len(calls))
	}

	a, ch := newTestAPI(t)
	for _, tc := range calls {
		t.Run(tc.file, func(t *testing.T) {
			b, err := os.ReadFile("testdata/proton-requests/" + tc.file)
			if err != nil {
				t.Fatal(err)
			}
			s := string(b)
			for k, v := range vals {
				s = strings.ReplaceAll(s, "{{"+k+"}}", v)
			}
			_, body, _ := strings.Cut(s, "\r\n\r\n")
			want := strings.ReplaceAll(s, "{{len}}", strconv.Itoa(len(body)))
			if strings.Contains(want, "{{") {
				t.Fatalf("unknown placeholder in %s", tc.file)
			}
			if got := send(t, a, ch, tc.c).request; string(got) != want {
				t.Errorf("request\n%q\nwant\n%q", got, want)
			}
		})
	}
}

func TestDumps(t *testing.T) {
	// want is what Python json.dumps(v) prints for the same value.
	for _, tc := range []struct {
		v    any
		want string
	}{
		{obj{}, `{}`},
		{obj{{"z", 1}, {"a", 2}}, `{"z": 1, "a": 2}`},
		{obj{{"Features", obj{{"Inner", obj{}}, {"X", "y"}}}, {"Last", nil}}, `{"Features": {"Inner": {}, "X": "y"}, "Last": null}`},
		{obj{{"t", true}, {"f", false}, {"n", nil}}, `{"t": true, "f": false, "n": null}`},
		{obj{{"a", 0}, {"b", -5}, {"c", 1234567890123}}, `{"a": 0, "b": -5, "c": 1234567890123}`},
		{"plain text", `"plain text"`},
		{`a"b\c`, `"a\"b\\c"`},
		{"http://x/y", `"http://x/y"`},
		{"<a href='x'>&amp;</a>", `"<a href='x'>&amp;</a>"`},
		{"line\nnext\r\t\b\f", `"line\nnext\r\t\b\f"`},
		{"\x00\x01\x1f\x7f", `"\u0000\u0001\u001f\u007f"`},
		{"é ü Ж 東京", `"\u00e9 \u00fc \u0416 \u6771\u4eac"`},
		{"\u2028\ufeff", `"\u2028\ufeff"`},
		{"😀𝄞", `"\ud83d\ude00\ud834\udd1e"`},
		{obj{{"ключ", "значение"}}, `{"\u043a\u043b\u044e\u0447": "\u0437\u043d\u0430\u0447\u0435\u043d\u0438\u0435"}`},
	} {
		got, err := dumps(tc.v)
		if err != nil || string(got) != tc.want {
			t.Errorf("dumps(%#v) = %s, %v; want %s", tc.v, got, err, tc.want)
		}
	}
	for _, v := range []any{"\xff", obj{{"\xc3", 1}}, 1.5, []any{1}, obj{{"a", int64(1)}}} {
		if _, err := dumps(v); err == nil {
			t.Errorf("dumps(%#v): no error", v)
		}
	}
}
