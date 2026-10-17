package outbound

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/xtls/xray-core/app/proxyman"
	"github.com/xtls/xray-core/core"
	"github.com/xtls/xray-core/proxy/vless"
	vout "github.com/xtls/xray-core/proxy/vless/outbound"
	"github.com/xtls/xray-core/transport/internet"
	"github.com/xtls/xray-core/transport/internet/reality"
	"github.com/xtls/xray-core/transport/internet/splithttp"
	"github.com/xtls/xray-core/transport/internet/tcp"
	"github.com/xtls/xray-core/transport/internet/tls"

	"github.com/a1ekseev/scd/internal/common/inputsrc"
	"github.com/a1ekseev/scd/internal/common/subscription"
	vlesslink "github.com/a1ekseev/scd/internal/common/vless"
	"github.com/a1ekseev/scd/internal/scd/normalize"
)

const (
	corpusDir     = "../../../testdata/reference/scd/corpus/"
	realShapesDir = "../../../docker/fixtures/real-shapes/"
)

// fileLines returns the lines of a corpus file (index 0 = line 1).
func fileLines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimRight(string(data), "\n"), "\n")
}

func parseRaw(t *testing.T, raw string, n int) vlesslink.Link {
	t.Helper()
	l, skip := vlesslink.Parse(subscription.Line{Number: n, Raw: raw, Trimmed: strings.TrimSpace(raw)})
	if skip != nil {
		t.Fatalf("line %d skipped by the parser: %+v", n, *skip)
	}
	return l
}

func normalizeLink(t *testing.T, l vlesslink.Link) normalize.Normalized {
	t.Helper()
	n, _, skip := normalize.Normalize(l, normalize.Policy{AllowInsecure: true})
	if skip != nil {
		t.Fatalf("line %d skipped by normalize: %+v", l.Line, *skip)
	}
	return n
}

// d26 returns the normalized d26-links.txt line n.
func d26(t *testing.T, n int) normalize.Normalized {
	t.Helper()
	return normalizeLink(t, parseRaw(t, fileLines(t, corpusDir+"d26-links.txt")[n-1], n))
}

// legacy returns the normalized vless-corpus.txt line n (09 §6 references).
func legacy(t *testing.T, n int) normalize.Normalized {
	t.Helper()
	return normalizeLink(t, parseRaw(t, fileLines(t, corpusDir+"vless-corpus.txt")[n-1], n))
}

// expectRow is one row of d26-expect.json.
type expectRow struct {
	Line           int      `json:"line"`
	MatrixRow      string   `json:"matrixRow"`
	Expect         string   `json:"expect"`
	Stage          string   `json:"stage"`
	ReasonCode     string   `json:"reasonCode"`
	ExpectContains string   `json:"expectContains"`
	Profile        string   `json:"profile"`
	Params         []string `json:"params"`
}

func expectRows(t *testing.T) []expectRow {
	t.Helper()
	data, err := os.ReadFile(corpusDir + "d26-expect.json")
	if err != nil {
		t.Fatal(err)
	}
	var rows []expectRow
	if err := json.Unmarshal(data, &rows); err != nil {
		t.Fatal(err)
	}
	return rows
}

func marshal(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func mustProto(t *testing.T, n normalize.Normalized) *core.OutboundHandlerConfig {
	t.Helper()
	h, err := ToProto(Build(n))
	if err != nil {
		t.Fatalf("line %d: %v", n.Line, err)
	}
	return h
}

// parts unpacks the typed messages of a built outbound handler.
type parts struct {
	stream    *internet.StreamConfig
	account   *vless.Account
	tls       *tls.Config
	reality   *reality.Config
	transport map[string]any
}

func unpack(t *testing.T, h *core.OutboundHandlerConfig) parts {
	t.Helper()
	snd, err := h.SenderSettings.GetInstance()
	if err != nil {
		t.Fatal(err)
	}
	prx, err := h.ProxySettings.GetInstance()
	if err != nil {
		t.Fatal(err)
	}
	acc, err := prx.(*vout.Config).Vnext.User.Account.GetInstance()
	if err != nil {
		t.Fatal(err)
	}
	p := parts{stream: snd.(*proxyman.SenderConfig).StreamSettings, account: acc.(*vless.Account), transport: map[string]any{}}
	for _, s := range p.stream.SecuritySettings {
		m, err := s.GetInstance()
		if err != nil {
			t.Fatal(err)
		}
		switch c := m.(type) {
		case *tls.Config:
			p.tls = c
		case *reality.Config:
			p.reality = c
		}
	}
	for _, ts := range p.stream.TransportSettings {
		m, err := ts.Settings.GetInstance()
		if err != nil {
			t.Fatal(err)
		}
		p.transport[ts.ProtocolName] = m
	}
	return p
}

const (
	wantTCPTLS = `{"tag":"TAG","protocol":"vless","settings":{"vnext":[{"address":"de1.example.test","port":8443,` +
		`"users":[{"id":"00000000-0000-4000-8000-000000000002","encryption":"none","email":"TAG"}]}]},` +
		`"streamSettings":{"network":"tcp","security":"tls","tlsSettings":{"alpn":["h2"]}}}`
	wantWsTLS = `{"tag":"TAG","protocol":"vless","settings":{"vnext":[{"address":"198.51.100.10","port":443,` +
		`"users":[{"id":"00000000-0000-4000-8000-000000000003","encryption":"none","email":"TAG"}]}]},` +
		`"streamSettings":{"network":"ws","security":"tls",` +
		`"tlsSettings":{"serverName":"cdn.example.test","fingerprint":"chrome"},` +
		`"wsSettings":{"path":"/stream/updates/case-a?ed=2048","host":"cdn-1.example.test"}}}`
	wantReality = `{"tag":"TAG","protocol":"vless","settings":{"vnext":[{"address":"203.0.113.20","port":443,` +
		`"users":[{"id":"00000000-0000-4000-8000-000000000004","encryption":"none","flow":"xtls-rprx-vision","email":"TAG"}]}]},` +
		`"streamSettings":{"network":"tcp","security":"reality",` +
		`"realitySettings":{"serverName":"io.example.test","fingerprint":"chrome",` +
		`"password":"GfXF1r493DSSqCtXMjqIvwiTpV_XPN5mCkj-GwcMjKI","shortId":"abe4a59b9f2407e3","spiderX":"/"}}}`
)

// assertReferenceJSON compares json.Marshal(Build(n)) of a vless-corpus.txt
// line with a 09 §6 reference string (TAG = the outbound tag).
func assertReferenceJSON(t *testing.T, line int, want string) {
	t.Helper()
	n := legacy(t, line)
	got := marshal(t, Build(n))
	if want = strings.ReplaceAll(want, "TAG", n.Tag); got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
}

func TestBuildJSONTcpTLS(t *testing.T) { assertReferenceJSON(t, 63, wantTCPTLS) } // 09 §6.1

func TestBuildJSONWsTLS(t *testing.T) { // 09 §6.2
	assertReferenceJSON(t, 64, wantWsTLS)
	if s := Build(legacy(t, 64)).StreamSettings; s.TCPSettings != nil || s.RealitySettings != nil || s.XHTTPSettings != nil {
		t.Fatalf("foreign sections: %s", marshal(t, s))
	}
}

func TestBuildJSONReality(t *testing.T) { assertReferenceJSON(t, 65, wantReality) } // 09 §6.3

func TestBuildJSONOmitsAllowInsecure(t *testing.T) {
	for _, n := range []normalize.Normalized{legacy(t, 64), legacy(t, 69), d26(t, 43), d26(t, 45)} {
		if n.AllowInsecure == nil {
			t.Fatalf("line %d: fixture must carry allowInsecure", n.Line)
		}
		if got := marshal(t, Build(n)); strings.Contains(strings.ToLower(got), "allowinsecure") {
			t.Fatalf("line %d: allowInsecure reached the outbound: %s", n.Line, got)
		}
	}
}

func TestBuildJSONTLSSectionAlwaysPresent(t *testing.T) {
	// A tls link with nothing to put in tlsSettings still gets {} (RS2-like).
	n := normalizeLink(t, parseRaw(t, "vless://7c0e4b8a-2f3d-4e6a-9b1c-5d8f0a2e4c61@tls.example.test:443?type=tcp&security=tls#x", 1))
	got := marshal(t, Build(n).StreamSettings)
	if want := `{"network":"tcp","security":"tls","tlsSettings":{}}`; got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}

func TestBuildJSONTLSLists(t *testing.T) {
	// pcs/vcn are written as comma-joined strings (conf parses them), alpn as a list.
	got := Build(d26(t, 43)).StreamSettings.TLSSettings
	if got == nil || strings.Count(got.PinnedPeerCertSha256, ",") != 1 {
		t.Fatalf("pcs: %+v", got)
	}
	got = Build(d26(t, 45)).StreamSettings.TLSSettings
	if got == nil || got.VerifyPeerCertByName == "" || !strings.Contains(got.VerifyPeerCertByName, ",") {
		t.Fatalf("vcn: %+v", got)
	}
	got = Build(d26(t, 41)).StreamSettings.TLSSettings
	if !slices.Equal(got.ALPN, []string{"h2", "http/1.1"}) {
		t.Fatalf("alpn: %q", got.ALPN)
	}
	if cs := Build(d26(t, 48)).StreamSettings.TLSSettings.CipherSuites; cs == "" {
		t.Fatal("cs not written")
	}
}

func TestBuildJSONHeaderHTTP(t *testing.T) {
	var got struct {
		StreamSettings struct {
			TCPSettings struct {
				Header struct {
					Type    string `json:"type"`
					Request struct {
						Path    []string            `json:"path"`
						Headers map[string][]string `json:"headers"`
					} `json:"request"`
				} `json:"header"`
			} `json:"tcpSettings"`
		} `json:"streamSettings"`
	}
	if err := json.Unmarshal([]byte(marshal(t, Build(d26(t, 57)))), &got); err != nil {
		t.Fatal(err)
	}
	h := got.StreamSettings.TCPSettings.Header
	if h.Type != "http" || !slices.Equal(h.Request.Path, []string{"/a", "/b"}) ||
		!reflect.DeepEqual(h.Request.Headers, map[string][]string{"Host": {"h1.example.test", "h2.example.test"}}) {
		t.Fatalf("header: %+v", h)
	}
}

func TestBuildJSONHeaderHTTPDefaults(t *testing.T) {
	// Empty host/path: the keys are not written and Xray keeps its defaults.
	n := normalizeLink(t, parseRaw(t, "vless://7c0e4b8a-2f3d-4e6a-9b1c-5d8f0a2e4c61@tls.example.test:443?type=tcp&headerType=http&security=tls#x", 1))
	if got, want := marshal(t, Build(n).StreamSettings.TCPSettings), `{"header":{"type":"http","request":{}}}`; got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}

func TestBuildJSONHeaderTypes(t *testing.T) {
	cases := []struct {
		line int
		want string // "" = no tcpSettings
	}{
		{56, ""},                             // headerType=none
		{1, ""},                              // no headerType
		{107, `{"header":{"type":"bogus"}}`}, // any other value goes to conf, which rejects it
	}
	for _, tc := range cases {
		ts := Build(d26(t, tc.line)).StreamSettings.TCPSettings
		got := ""
		if ts != nil {
			got = marshal(t, ts)
		}
		if got != tc.want {
			t.Errorf("line %d: tcpSettings %s, want %s", tc.line, got, tc.want)
		}
	}
	empty := normalizeLink(t, parseRaw(t, "vless://7c0e4b8a-2f3d-4e6a-9b1c-5d8f0a2e4c61@tls.example.test:443?type=tcp&headerType=&security=tls#x", 1))
	if Build(empty).StreamSettings.TCPSettings != nil {
		t.Error("empty headerType must not write tcpSettings")
	}
}

func TestBuildJSONSectionsPerNetwork(t *testing.T) {
	ws := Build(d26(t, 17)).StreamSettings // ws without path/host
	if ws.WSSettings == nil || marshal(t, ws.WSSettings) != "{}" {
		t.Fatalf("ws: %s", marshal(t, ws))
	}
	hb := Build(d26(t, 62)).StreamSettings.WSSettings
	if hb == nil || hb.HeartbeatPeriod == 0 || !strings.Contains(marshal(t, hb), `"heartbeatPeriod":`) {
		t.Fatalf("heartbeatPeriod: %+v", hb)
	}
	xh := Build(d26(t, 64)).StreamSettings
	if xh.XHTTPSettings == nil || xh.XHTTPSettings.Mode != "stream-up" || xh.WSSettings != nil || xh.TCPSettings != nil {
		t.Fatalf("xhttp: %s", marshal(t, xh))
	}
}

func TestBuildJSONXHTTPExtraAndFinalMaskRaw(t *testing.T) {
	extra := `{"zz":1,"xPaddingBytes":"100-1000","a":{"b":[2,1]}}`
	fm := `{"tcp":[{"type":"fragment","settings":{"packets":"tlshello","length":"100-200"}}]}`
	raw := "vless://7c0e4b8a-2f3d-4e6a-9b1c-5d8f0a2e4c61@xh.example.test:443?type=xhttp&security=tls&path=%2Fxh" +
		"&extra=" + escape(extra) + "&fm=" + escape(fm) + "#x"
	got := marshal(t, Build(normalizeLink(t, parseRaw(t, raw, 1))))
	if !strings.Contains(got, `"extra":`+extra+`}`) {
		t.Fatalf("extra not embedded verbatim: %s", got)
	}
	if !strings.HasSuffix(got, `"finalmask":`+fm+`}}`) {
		t.Fatalf("finalmask not embedded verbatim: %s", got)
	}
}

// escape percent-encodes a query value.
func escape(s string) string {
	var b strings.Builder
	for _, c := range []byte(s) {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' {
			b.WriteByte(c)
			continue
		}
		b.WriteString("%" + strings.ToUpper(hex.EncodeToString([]byte{c})))
	}
	return b.String()
}

func TestBuildJSONNoneSecurityMLKEM(t *testing.T) {
	n := d26(t, 14)
	o := Build(n)
	got := marshal(t, o)
	if strings.Contains(got, "tlsSettings") || strings.Contains(got, "realitySettings") {
		t.Fatalf("security sections with security=none: %s", got)
	}
	if o.StreamSettings.Security != "none" || o.Settings.Vnext[0].Users[0].Encryption != n.Encryption ||
		!strings.HasPrefix(n.Encryption, "mlkem768x25519plus.") {
		t.Fatalf("got %s", got)
	}
}

func TestHashStableForSameInput(t *testing.T) {
	list := []Outbound{Build(d26(t, 1)), Build(d26(t, 6)), Build(d26(t, 65))}
	h1, h2 := Hash(list), Hash(slices.Clone(list))
	if h1 != h2 || len(h1) != 64 {
		t.Fatalf("hash %q vs %q", h1, h2)
	}
	b, err := json.Marshal(list)
	if err != nil {
		t.Fatal(err)
	}
	if sum := sha256.Sum256(b); h1 != hex.EncodeToString(sum[:]) {
		t.Fatalf("hash %s is not sha256(json.Marshal(list))", h1)
	}
	other := slices.Clone(list)
	other[1].Tag += "x"
	if Hash(other) == h1 {
		t.Fatal("hash ignores a changed outbound")
	}
	if Hash([]Outbound{list[1], list[0]}) == Hash(list[:2]) {
		t.Fatal("hash ignores order")
	}
}

func TestForTargetUdp443OnlyReality(t *testing.T) {
	realityVision := d26(t, 6) // tcp+reality+vision
	tlsVision := d26(t, 3)     // tcp+tls+vision
	if realityVision.Flow != "xtls-rprx-vision" || tlsVision.Flow != "xtls-rprx-vision" {
		t.Fatal("fixtures must carry vision")
	}
	for _, override := range []bool{false, true} {
		got := ForTarget(realityVision, "t1-"+realityVision.Tag, override)
		want := "xtls-rprx-vision"
		if override {
			want = "xtls-rprx-vision-udp443"
		}
		if got.Flow != want || got.Tag != "t1-"+realityVision.Tag {
			t.Errorf("reality override=%v: flow %q tag %q", override, got.Flow, got.Tag)
		}
		if u := Build(got).Settings.Vnext[0].Users[0]; u.Flow != want || u.Email != got.Tag {
			t.Errorf("reality override=%v: user %+v", override, u)
		}
		if g := ForTarget(tlsVision, "t1", override); g.Flow != "xtls-rprx-vision" || g.Tag != "t1" {
			t.Errorf("tls override=%v: flow %q", override, g.Flow)
		}
	}
	plain := d26(t, 5) // reality without flow
	if g := ForTarget(plain, "t", true); g.Flow != "" {
		t.Errorf("reality without flow got %q", g.Flow)
	}
	if realityVision.Flow != "xtls-rprx-vision" || realityVision.Tag == "t1-"+realityVision.Tag {
		t.Fatal("ForTarget mutated its input")
	}
}

// captureOutput redirects os.Stdout and os.Stderr into a pipe while fn runs
// and for a grace period after it (Xray's logger writes asynchronously).
func captureOutput(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, stderr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = w, w
	fn()
	time.Sleep(300 * time.Millisecond)
	os.Stdout, os.Stderr = stdout, stderr
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func TestToProtoIsSilent(t *testing.T) {
	// ws makes conf.Build log a deprecation warning; Xray's default handler
	// prints it to stdout (review B1).
	o := Build(legacy(t, 64))
	out := captureOutput(t, func() {
		if _, err := ToProto(o); err != nil {
			t.Error(err)
		}
		// A rejected outbound must not print either.
		bad := Build(d26(t, 104))
		if _, err := ToProto(bad); err == nil {
			t.Error("line 104 must be rejected")
		}
	})
	if out != "" {
		t.Fatalf("ToProto wrote to stdout/stderr: %q", out)
	}
}

func TestToProtoErrorNamesTag(t *testing.T) {
	n := d26(t, 103)
	_, err := ToProto(Build(n))
	if err == nil || !strings.HasPrefix(err.Error(), "invalid outbound "+n.Tag+": ") {
		t.Fatalf("err %v", err)
	}
}

func TestXrayRejectsLikeMatrix(t *testing.T) {
	lines := fileLines(t, corpusDir+"d26-links.txt")
	checked := 0
	for _, r := range expectRows(t) {
		if r.Stage != "xray" || r.ReasonCode != "unsupported_value" {
			continue
		}
		n := normalizeLink(t, parseRaw(t, lines[r.Line-1], r.Line))
		_, err := ToProto(Build(n))
		if err == nil || !strings.Contains(err.Error(), r.ExpectContains) {
			t.Errorf("line %d: err %v, want it to contain %q", r.Line, err, r.ExpectContains)
		}
		checked++
	}
	if checked != 16 {
		t.Fatalf("checked %d xray-stage rows, want 16", checked)
	}
}

func TestMLKEMAccountTransform(t *testing.T) {
	cases := []struct {
		line           int
		xor, seconds   uint32
		padding, after string
	}{
		{28, 1, 0, "", "mlkem768x25519plus.xorpub.1rtt."},
		{29, 2, 1, "", "mlkem768x25519plus.random.0rtt."},
		{30, 0, 1, "100-111-1111.75-0-111.50-0-3333", "mlkem768x25519plus.native.0rtt.100-111-1111.75-0-111.50-0-3333."},
		{31, 0, 1, "", ""},
	}
	for _, tc := range cases {
		n := d26(t, tc.line)
		a := unpack(t, mustProto(t, n)).account
		if a.XorMode != tc.xor || a.Seconds != tc.seconds || a.Padding != tc.padding {
			t.Errorf("line %d: xor=%d seconds=%d padding=%q", tc.line, a.XorMode, a.Seconds, a.Padding)
		}
		if tc.after != "" && a.Encryption != strings.TrimPrefix(n.Encryption, tc.after) {
			t.Errorf("line %d: encryption %q", tc.line, a.Encryption)
		}
		if strings.HasPrefix(a.Encryption, "mlkem768x25519plus") {
			t.Errorf("line %d: prefix not stripped", tc.line)
		}
	}
}

func realShapeFirst(t *testing.T, name string) normalize.Normalized {
	t.Helper()
	data, err := os.ReadFile(realShapesDir + name)
	if err != nil {
		t.Fatal(err)
	}
	content, _ := inputsrc.DecodeAuto(string(data))
	sl := subscription.ScanLines(content)[0]
	l, skip := vlesslink.Parse(sl)
	if skip != nil {
		t.Fatalf("%s: %+v", name, *skip)
	}
	return normalizeLink(t, l)
}

func TestToProtoRealShapes(t *testing.T) {
	t.Run("remnawave", func(t *testing.T) {
		p := unpack(t, mustProto(t, realShapeFirst(t, "remnawave.txt")))
		if a := p.account; a.XorMode != 0 || a.Seconds != 1 || a.Padding != "" || len(a.Encryption) < 100 {
			t.Fatalf("account xor=%d seconds=%d padding=%q", a.XorMode, a.Seconds, a.Padding)
		}
		x, ok := p.transport["splithttp"].(*splithttp.Config)
		if !ok {
			t.Fatalf("no splithttp transport: %v", p.transport)
		}
		rng := func(r *splithttp.RangeConfig) [2]int32 {
			if r == nil {
				return [2]int32{}
			}
			return [2]int32{r.From, r.To}
		}
		m := x.Xmux
		if x.Mode != "stream-up" || m == nil ||
			rng(m.MaxConcurrency) != [2]int32{8, 16} || rng(m.MaxConnections) != [2]int32{} ||
			rng(m.CMaxReuseTimes) != [2]int32{} || rng(m.HMaxRequestTimes) != [2]int32{600, 900} ||
			rng(m.HMaxReusableSecs) != [2]int32{1800, 3000} || m.HKeepAlivePeriod != 0 ||
			rng(x.XPaddingBytes) != [2]int32{100, 1000} || x.NoGRPCHeader || len(x.Headers) != 0 {
			t.Fatalf("splithttp: mode=%q xmux=%v padding=%v noGRPC=%v", x.Mode, m, x.XPaddingBytes, x.NoGRPCHeader)
		}
		if p.tls == nil || !slices.Equal(p.tls.NextProtocol, []string{"h2"}) || p.tls.Fingerprint != "chrome" {
			t.Fatalf("tls: %v", p.tls)
		}
	})
	t.Run("provider2", func(t *testing.T) {
		p := unpack(t, mustProto(t, realShapeFirst(t, "provider2.txt")))
		if c, ok := p.transport["tcp"].(*tcp.Config); ok && c.HeaderSettings != nil {
			t.Fatalf("tcp header settings: %v", c.HeaderSettings)
		}
		if p.tls == nil || p.tls.ServerName != "" || !slices.Equal(p.tls.NextProtocol, []string{"h2"}) {
			t.Fatalf("tls: %v", p.tls)
		}
		if p.account.Encryption != "none" {
			t.Fatalf("encryption %q", p.account.Encryption)
		}
	})
}

func TestRealityAlwaysHasSpiderY(t *testing.T) {
	lines := fileLines(t, corpusDir+"d26-links.txt")
	checked := 0
	for _, r := range expectRows(t) {
		if r.Expect != "entry" {
			continue
		}
		n := normalizeLink(t, parseRaw(t, lines[r.Line-1], r.Line))
		if n.Security != "reality" {
			continue
		}
		rc := unpack(t, mustProto(t, n)).reality
		if rc == nil || len(rc.SpiderY) != 10 || rc.SpiderX == "" || len(rc.ShortId) != 8 {
			t.Errorf("line %d: reality %v", r.Line, rc)
		}
		if n.ShortID == "" && !slices.Equal(rc.ShortId, make([]byte, 8)) {
			t.Errorf("line %d: empty sid gave %x", r.Line, rc.ShortId)
		}
		checked++
	}
	if checked < 10 {
		t.Fatalf("only %d reality entries checked", checked)
	}
}
