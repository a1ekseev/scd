package normalize

import (
	"encoding/base64"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/a1ekseev/scd/internal/common/vless"
)

const wantPbk = `REALITY VLESS requires parameter "pbk".`

// corpusParam returns a parameter of a d26-links.txt line.
func corpusParam(t *testing.T, line int, key string) string {
	t.Helper()
	v, ok := parseLine(t, corpusLines(t)[line-1], line).Get(key)
	if !ok || v == "" {
		t.Fatalf("line %d has no %s", line, key)
	}
	return v
}

const realityQuery = "type=tcp&security=reality&sni=www.example.test&fp=chrome&pbk=" + testPbk + "&sid=ab"

func TestTLSFields(t *testing.T) {
	n, w := mustEntry(t, "type=tcp&security=tls&sni=front.example.test&fp=Firefox&alpn=h2,%20http/1.1,,", Policy{})
	if n.SNI != "front.example.test" || n.Fingerprint != "firefox" || !slices.Equal(n.ALPN, []string{"h2", "http/1.1"}) {
		t.Fatalf("got sni %q fp %q alpn %q", n.SNI, n.Fingerprint, n.ALPN)
	}
	if w != nil {
		t.Fatalf("warnings %v", w)
	}
	// TLS-only fields stay empty without their parameters.
	n, _ = mustEntry(t, "type=tcp&security=tls", Policy{})
	if n.ALPN == nil || len(n.ALPN) != 0 || n.PinnedPeerCertSha256 != nil || n.VerifyPeerCertByName != nil ||
		n.ECHConfigList != "" || n.CipherSuites != "" || n.PublicKey != "" || n.SpiderX != "" {
		t.Fatalf("unexpected fields %+v", n)
	}
}

func TestTLSPinsAndNames(t *testing.T) {
	const colon = "2D:AA:13:89:E3:E6:0F:82:11:59:DE:ED:A7:5E:55:BD:ED:FA:90:B7:31:A4:02:82:36:98:36:A7:ED:40:D3:04"
	const hex = "3dfdf0330d3e9e2bbd2e1b4e8c0e7d4b54e1f1b0d0c6a5f8f3b2e2d1c0b9a8f7"
	n, _ := mustEntry(t, "type=tcp&security=tls&pcs="+url.QueryEscape(colon+", "+hex+",")+
		"&vcn="+url.QueryEscape("a.example.test, b.example.test"), Policy{})
	if !slices.Equal(n.PinnedPeerCertSha256, []string{colon, hex}) {
		t.Fatalf("pcs %q", n.PinnedPeerCertSha256)
	}
	if !slices.Equal(n.VerifyPeerCertByName, []string{"a.example.test", "b.example.test"}) {
		t.Fatalf("vcn %q", n.VerifyPeerCertByName)
	}
}

func TestTLSECHKeptVerbatim(t *testing.T) {
	for _, ech := range []string{corpusParam(t, 46, "ech"), "ech.example.test+https://1.1.1.1/dns-query"} {
		n, _ := mustEntry(t, "type=tcp&security=tls&sni=ech.example.test&ech="+url.QueryEscape(ech), Policy{})
		if n.ECHConfigList != ech {
			t.Fatalf("ech %q, want %q", n.ECHConfigList, ech)
		}
	}
}

func TestTLSCipherSuitesKept(t *testing.T) {
	const cs = "cs=TLS_AES_128_GCM_SHA256%3ATLS_CHACHA20_POLY1305_SHA256"
	n, w := mustEntry(t, "type=tcp&security=tls&sni=tls.example.test&"+cs, Policy{})
	if n.CipherSuites != "TLS_AES_128_GCM_SHA256:TLS_CHACHA20_POLY1305_SHA256" || w != nil {
		t.Fatalf("cipherSuites %q warnings %v", n.CipherSuites, w)
	}
	n, w = mustEntry(t, realityQuery+"&"+cs, Policy{})
	if n.CipherSuites != "" || w != nil {
		t.Fatalf("reality: cipherSuites %q warnings %v", n.CipherSuites, w)
	}
}

func TestRealityRequiresOnlyPbk(t *testing.T) {
	for _, q := range []string{
		"type=tcp&security=reality&sni=www.example.test&fp=chrome&sid=ab",
		"type=tcp&security=reality&pbk=&sid=ab",
		"type=xhttp&security=reality&sni=www.example.test",
	} {
		mustSkip(t, q, Policy{}, vless.CodeMissingRequiredField, wantPbk)
	}
	n, w := mustEntry(t, "type=tcp&security=reality&pbk="+testPbk, Policy{})
	if n.Profile != "tcp-reality" || n.SNI != "" || n.Fingerprint != "" || n.ShortID != "" || w != nil {
		t.Fatalf("got %+v %v", n, w)
	}
}

func TestRealityFields(t *testing.T) {
	n, _ := mustEntry(t, "type=tcp&security=reality&sni=www.example.test&fp=Chrome&pbk="+testPbk+
		"&sid=ab&alpn=h2&pcs=ab&vcn=a.example.test&ech=x", Policy{})
	if n.SNI != "www.example.test" || n.Fingerprint != "chrome" || n.PublicKey != testPbk || n.ShortID != "ab" {
		t.Fatalf("got %+v", n)
	}
	// TLS-only parameters are inapplicable to REALITY and dropped (O16).
	if len(n.ALPN) != 0 || n.ALPN == nil || n.PinnedPeerCertSha256 != nil || n.VerifyPeerCertByName != nil ||
		n.ECHConfigList != "" {
		t.Fatalf("TLS fields on reality: %+v", n)
	}
}

func TestRealityPbkNormalized(t *testing.T) {
	raw, err := base64.RawURLEncoding.DecodeString(testPbk)
	if err != nil {
		t.Fatal(err)
	}
	std := base64.StdEncoding.EncodeToString(raw)
	if !strings.HasSuffix(std, "=") || !strings.ContainsAny(std, "+/") {
		t.Fatalf("test key %q must use the std alphabet and padding", std)
	}
	n, _ := mustEntry(t, "type=tcp&security=reality&pbk="+url.QueryEscape(std), Policy{})
	if n.PublicKey != testPbk {
		t.Fatalf("publicKey %q, want %q", n.PublicKey, testPbk)
	}
}

func TestRealityDefaultSpiderX(t *testing.T) {
	for _, q := range []string{realityQuery, realityQuery + "&spx="} {
		n, _ := mustEntry(t, q, Policy{})
		if n.SpiderX != "/" {
			t.Fatalf("%s: spiderX %q", q, n.SpiderX)
		}
	}
}

func TestRealitySpxKept(t *testing.T) {
	n, _ := mustEntry(t, realityQuery+"&spx="+url.QueryEscape("/abc?p=10-20&c=2"), Policy{})
	if n.SpiderX != "/abc?p=10-20&c=2" {
		t.Fatalf("spiderX %q", n.SpiderX)
	}
}

func TestRealityPqvKept(t *testing.T) {
	pqv := corpusParam(t, 53, "pqv")
	n, _ := mustEntry(t, realityQuery+"&pqv="+pqv, Policy{})
	if n.Mldsa65Verify != pqv {
		t.Fatalf("mldsa65Verify %.20q…", n.Mldsa65Verify)
	}
	// pqv is a REALITY parameter: ignored on TLS (O16).
	n, _ = mustEntry(t, "type=tcp&security=tls&pqv="+pqv, Policy{})
	if n.Mldsa65Verify != "" {
		t.Fatal("pqv on tls")
	}
}

func TestNoneSecurityLeavesFieldsEmpty(t *testing.T) {
	n, w := mustEntry(t, "type=tcp&security=none&sni=x&fp=chrome&alpn=h2&pbk="+testPbk+"&sid=ab&spx=/a&pcs=ab"+
		"&vcn=a&ech=x&cs=TLS_AES_128_GCM_SHA256&encryption="+mlkemEnc(t), Policy{})
	if n.SNI != "" || n.Fingerprint != "" || len(n.ALPN) != 0 || n.ALPN == nil || n.PublicKey != "" ||
		n.ShortID != "" || n.SpiderX != "" || n.Mldsa65Verify != "" || n.PinnedPeerCertSha256 != nil ||
		n.VerifyPeerCertByName != nil || n.ECHConfigList != "" || n.CipherSuites != "" || w != nil {
		t.Fatalf("got %+v %v", n, w)
	}
}
