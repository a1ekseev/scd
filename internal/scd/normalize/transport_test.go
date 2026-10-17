package normalize

import (
	"bytes"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/a1ekseev/scd/internal/common/inputsrc"
	"github.com/a1ekseev/scd/internal/common/subscription"
	"github.com/a1ekseev/scd/internal/common/vless"
)

const (
	wantHeartbeat = "ws heartbeatPeriod must be a non-negative integer."
	wantExtra     = "xhttp extra must be a JSON object."
	wantFM        = "fm (finalmask) must be a JSON object."
	tlsTCP        = "type=tcp&security=tls&sni=h.example.test"
	tlsWS         = "type=ws&security=tls&sni=h.example.test"
	tlsXHTTP      = "type=xhttp&security=tls&sni=h.example.test"
)

func TestTCPHeaderNoneKept(t *testing.T) {
	for _, tc := range []struct{ query, want string }{
		{tlsTCP + "&headerType=none", "none"},
		{tlsTCP + "&headerType=", ""},
	} {
		t.Run(tc.want, func(t *testing.T) {
			n, w := mustEntry(t, tc.query, Policy{})
			if w != nil || n.HeaderType == nil || *n.HeaderType != tc.want {
				t.Fatalf("headerType %v, warnings %v", n.HeaderType, w)
			}
		})
	}
	n, _ := mustEntry(t, tlsTCP, Policy{})
	if n.HeaderType != nil {
		t.Fatalf("absent headerType mapped to %q", *n.HeaderType)
	}
}

func TestTCPHeaderHTTP(t *testing.T) {
	n, w := mustEntry(t, tlsTCP+"&headerType=http&host=h1.example.test,h2.example.test&path=/a,/b", Policy{})
	if w != nil || n.HeaderType == nil || *n.HeaderType != "http" {
		t.Fatalf("headerType %v, warnings %v", n.HeaderType, w)
	}
	if n.Host != "h1.example.test,h2.example.test" || n.Path != "/a,/b" {
		t.Fatalf("host %q path %q", n.Host, n.Path)
	}
	// Without http the request host/path do not apply (O16).
	n, w = mustEntry(t, tlsTCP+"&headerType=none&host=h1.example.test&path=/a", Policy{})
	if w != nil || n.Host != "" || n.Path != "" {
		t.Fatalf("host %q path %q warnings %v", n.Host, n.Path, w)
	}
	// Other values are left to conf ("unknown config id").
	n, _ = mustEntry(t, tlsTCP+"&headerType=bogus&host=h1.example.test", Policy{})
	if *n.HeaderType != "bogus" || n.Host != "" {
		t.Fatalf("headerType %q host %q", *n.HeaderType, n.Host)
	}
}

func TestWSHostPathEd(t *testing.T) {
	n, w := mustEntry(t, tlsWS+"&host=cdn.example.test&path=%2Fws%3Fed%3D2048&headerType=none", Policy{})
	if w != nil || n.Host != "cdn.example.test" || n.Path != "/ws?ed=2048" || n.HeaderType != nil {
		t.Fatalf("host %q path %q headerType %v warnings %v", n.Host, n.Path, n.HeaderType, w)
	}
}

func TestWSWithoutPathAccepted(t *testing.T) {
	n, w := mustEntry(t, tlsWS, Policy{})
	if w != nil || n.Profile != "ws-tls" || n.Path != "" {
		t.Fatalf("profile %s path %q warnings %v", n.Profile, n.Path, w)
	}
}

func TestWSHeartbeatPeriod(t *testing.T) {
	n, w := mustEntry(t, tlsWS+"&path=%2Fws&heartbeatPeriod=30", Policy{})
	if w != nil || n.HeartbeatPeriod != 30 {
		t.Fatalf("heartbeatPeriod %d warnings %v", n.HeartbeatPeriod, w)
	}
	n, _ = mustEntry(t, tlsWS+"&heartbeatPeriod=4294967295", Policy{})
	if n.HeartbeatPeriod != 4294967295 {
		t.Fatalf("heartbeatPeriod %d", n.HeartbeatPeriod)
	}
	// An empty value is an unset parameter, like any other empty one.
	n, w = mustEntry(t, tlsWS+"&heartbeatPeriod=", Policy{})
	if w != nil || n.HeartbeatPeriod != 0 {
		t.Fatalf("heartbeatPeriod %d warnings %v", n.HeartbeatPeriod, w)
	}
	for _, v := range []string{"abc", "-1", "4294967296", "1.5", "+1", " 1"} {
		t.Run(v, func(t *testing.T) {
			mustSkip(t, tlsWS+"&heartbeatPeriod="+url.QueryEscape(v), Policy{},
				vless.CodeUnsupportedValue, wantHeartbeat, "heartbeatPeriod="+v)
		})
	}
	// Inapplicable on tcp and xhttp: dropped silently (O16).
	for _, q := range []string{tlsXHTTP, tlsTCP} {
		n, w := mustEntry(t, q+"&heartbeatPeriod=abc", Policy{})
		if w != nil || n.HeartbeatPeriod != 0 {
			t.Fatalf("%s: heartbeatPeriod %d warnings %v", q, n.HeartbeatPeriod, w)
		}
	}
}

func TestXHTTPFields(t *testing.T) {
	extra := `{"xPaddingBytes":"100-1000","xmux":{"maxConcurrency":"16-32"},"headers":{},` +
		`"downloadSettings":{"address":"dl.example.test","port":443,"network":"xhttp"}}`
	n, w := mustEntry(t, tlsXHTTP+"&host=xh.example.test&path=%2Fxh&mode=stream-up&headerType=http&extra="+url.QueryEscape(extra), Policy{})
	if w != nil || n.Profile != "xhttp-tls" {
		t.Fatalf("profile %s warnings %v", n.Profile, w)
	}
	if n.Host != "xh.example.test" || n.Path != "/xh" || n.XHTTPMode != "stream-up" || n.HeaderType != nil {
		t.Fatalf("host %q path %q mode %q headerType %v", n.Host, n.Path, n.XHTTPMode, n.HeaderType)
	}
	if string(n.XHTTPExtra) != extra {
		t.Fatalf("extra %s", n.XHTTPExtra)
	}
	// mode is left to conf; leading whitespace before the object is fine.
	n, _ = mustEntry(t, tlsXHTTP+"&mode=bogus&extra="+url.QueryEscape(" \n{}"), Policy{})
	if n.XHTTPMode != "bogus" || string(n.XHTTPExtra) != " \n{}" {
		t.Fatalf("mode %q extra %q", n.XHTTPMode, n.XHTTPExtra)
	}
	// An empty extra is unset; extra/mode do not apply to ws (O16).
	for _, q := range []string{tlsXHTTP + "&extra=", tlsWS + "&mode=auto&extra=%5B1%5D"} {
		n, w := mustEntry(t, q, Policy{})
		if w != nil || n.XHTTPExtra != nil || n.XHTTPMode != "" {
			t.Fatalf("%s: extra %q mode %q warnings %v", q, n.XHTTPExtra, n.XHTTPMode, w)
		}
	}
}

func TestXHTTPBadExtra(t *testing.T) {
	long := `["` + strings.Repeat("x", 60) + `"]`
	for _, tc := range []struct{ name, value, detail string }{
		{"array", "[1]", "extra=[1]"},
		{"broken", "{x", "extra={x"},
		{"string", `"{}"`, `extra="{}"`},
		{"trailing", "{} {}", "extra={} {}"},
		{"long", long, "extra=" + truncate(long, 40)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mustSkip(t, tlsXHTTP+"&path=%2Fxh&extra="+url.QueryEscape(tc.value), Policy{},
				vless.CodeUnsupportedValue, wantExtra, tc.detail)
		})
	}
}

func TestFinalMaskKept(t *testing.T) {
	fm := `{"tcp":[{"type":"fragment","settings":{"packets":"tlshello","length":"100-200","interval":"10-20"}}]}`
	for _, q := range []string{tlsTCP, tlsWS, tlsXHTTP, realityQuery} {
		n, w := mustEntry(t, q+"&fm="+url.QueryEscape(fm), Policy{})
		if w != nil || string(n.FinalMask) != fm {
			t.Fatalf("%s: finalmask %s warnings %v", q, n.FinalMask, w)
		}
	}
	n, _ := mustEntry(t, tlsTCP+"&fm=", Policy{})
	if n.FinalMask != nil {
		t.Fatalf("empty fm mapped to %q", n.FinalMask)
	}
}

func TestFinalMaskInvalid(t *testing.T) {
	for _, tc := range []struct{ query, value string }{
		{tlsTCP, "{x"},
		{tlsWS, "[]"},
		{tlsXHTTP, "null"},
		{realityQuery, "1"},
	} {
		t.Run(tc.value, func(t *testing.T) {
			mustSkip(t, tc.query+"&fm="+url.QueryEscape(tc.value), Policy{},
				vless.CodeUnsupportedValue, wantFM, "fm="+tc.value)
		})
	}
}

// realShapeLinks parses every line of a docker/fixtures/real-shapes file
// (base64 files through inputsrc.DecodeAuto).
func realShapeLinks(t *testing.T, name string) []vless.Link {
	t.Helper()
	data, err := os.ReadFile("../../../docker/fixtures/real-shapes/" + name)
	if err != nil {
		t.Fatal(err)
	}
	content, decoded := inputsrc.DecodeAuto(string(data))
	if strings.HasSuffix(name, ".b64") != decoded {
		t.Fatalf("%s: decoded=%v", name, decoded)
	}
	var links []vless.Link
	for _, sl := range subscription.ScanLines(content) {
		l, skip := vless.Parse(sl)
		if skip != nil {
			t.Fatalf("%s line %d: %+v", name, sl.Number, *skip)
		}
		links = append(links, l)
	}
	return links
}

// rawParam returns the percent-decoded value of key in the raw query of a link.
func rawParam(t *testing.T, raw, key string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		t.Fatal(err)
	}
	return q.Get(key)
}

func TestNormalizeRealShapes(t *testing.T) {
	links := realShapeLinks(t, "remnawave.txt")
	if len(links) != 4 {
		t.Fatalf("remnawave.txt: %d links", len(links))
	}
	for _, l := range links {
		n, w, skip := Normalize(l, Policy{})
		if skip != nil || w != nil {
			t.Fatalf("remnawave line %d: skip %+v warnings %v", l.Line, skip, w)
		}
		extra := rawParam(t, l.Raw, "extra")
		if n.Profile != "xhttp-tls" || n.XHTTPMode != "stream-up" || extra == "" || !bytes.Equal(n.XHTTPExtra, []byte(extra)) ||
			len(n.ALPN) != 1 || n.ALPN[0] != "h2" || n.Fingerprint != "chrome" ||
			!strings.HasPrefix(n.Encryption, "mlkem768x25519plus.native.0rtt.") {
			t.Fatalf("remnawave line %d: profile %s mode %q alpn %q fp %q extra equal %v",
				l.Line, n.Profile, n.XHTTPMode, n.ALPN, n.Fingerprint, bytes.Equal(n.XHTTPExtra, []byte(extra)))
		}
	}
	for _, name := range []string{"provider2.txt", "provider2.b64"} {
		links := realShapeLinks(t, name)
		if len(links) != 50 {
			t.Fatalf("%s: %d links", name, len(links))
		}
		for _, l := range links {
			n, w, skip := Normalize(l, Policy{})
			if skip != nil || w != nil {
				t.Fatalf("%s line %d: skip %+v warnings %v", name, l.Line, skip, w)
			}
			if n.Profile != "tcp-tls" || n.SNI != "" || n.Encryption != "none" || len(n.ALPN) != 1 || n.ALPN[0] != "h2" ||
				n.HeaderType != nil || n.Path != "" {
				t.Fatalf("%s line %d: profile %s sni %q enc %q alpn %q headerType %v path %q",
					name, l.Line, n.Profile, n.SNI, n.Encryption, n.ALPN, n.HeaderType, n.Path)
			}
		}
	}
}
