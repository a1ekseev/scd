package paramcheck_test

import (
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/xtls/xray-core/core"

	"github.com/a1ekseev/scd/internal/common/subscription"
	"github.com/a1ekseev/scd/internal/common/vless"
	"github.com/a1ekseev/scd/internal/scd/normalize"
	"github.com/a1ekseev/scd/internal/scd/outbound"
	"github.com/a1ekseev/scd/internal/scd/outbound/paramcheck"
)

// corpusLink parses d26-links.txt line n.
func corpusLink(t *testing.T, n int) vless.Link {
	t.Helper()
	data, err := os.ReadFile("../../../../testdata/reference/scd/corpus/d26-links.txt")
	if err != nil {
		t.Fatal(err)
	}
	raw := strings.Split(string(data), "\n")[n-1]
	l, skip := vless.Parse(subscription.Line{Number: n, Raw: raw, Trimmed: strings.TrimSpace(raw)})
	if skip != nil {
		t.Fatalf("line %d: %+v", n, *skip)
	}
	return l
}

func handler(t *testing.T, l vless.Link) *core.OutboundHandlerConfig {
	t.Helper()
	n, _, skip := normalize.Normalize(l, normalize.Policy{AllowInsecure: true})
	if skip != nil {
		t.Fatalf("line %d: %+v", l.Line, *skip)
	}
	h, err := outbound.ToProto(outbound.Build(n))
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestCovered(t *testing.T) {
	want := []string{
		"type", "security", "encryption", "flow", "sni", "fp", "alpn", "pcs", "vcn", "ech", "cs",
		"pbk", "sid", "spx", "pqv", "headertype", "host", "path", "heartbeatperiod", "mode", "extra", "fm",
	}
	if !slices.Equal(paramcheck.Covered, want) {
		t.Fatalf("Covered = %q", paramcheck.Covered)
	}
}

func TestCheckUnknownParam(t *testing.T) {
	if err := paramcheck.Check("serviceName", "x", handler(t, corpusLink(t, 1))); err == nil {
		t.Fatal("an uncovered parameter must be an error")
	}
}

func TestCheckNilHandler(t *testing.T) {
	if err := paramcheck.Check("type", "tcp", nil); err == nil {
		t.Fatal("nil handler must be an error")
	}
}

// TestCheckAcceptsLinkValue checks the parameter value from the link itself
// (positive) and a different value (negative) against the built handler.
func TestCheckAcceptsLinkValue(t *testing.T) {
	cases := []struct {
		line   int
		param  string
		wrong  string // a value that must not match
		values []string
	}{
		{1, "type", "ws", nil},
		{2, "type", "xhttp", nil}, // raw → tcp
		{7, "type", "tcp", nil},
		{10, "type", "ws", nil},
		{1, "security", "reality", nil},
		{5, "security", "tls", nil},
		{14, "security", "tls", nil},
		{1, "encryption", "mlkem768x25519plus.native.0rtt.AAAA", []string{"none"}},
		{14, "encryption", "none", nil},
		{30, "encryption", "none", nil},
		{3, "flow", "xtls-rprx-vision-udp443", nil},
		{1, "sni", "other.example.test", nil},
		{5, "sni", "other.example.test", nil},
		{54, "sni", "x.example.test", []string{""}}, // reality without sni
		{38, "fp", "firefox", nil},
		{5, "fp", "firefox", nil},
		{41, "alpn", "h2", nil},
		{43, "pcs", "abcd", nil},
		{45, "vcn", "a.example.test", nil},
		{46, "ech", "x", nil},
		{48, "cs", "TLS_AES_128_GCM_SHA256", nil},
		{49, "pbk", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", nil},
		{50, "sid", "ab", []string{""}},
		{51, "sid", "", nil},
		{52, "spx", "/", nil},
		{53, "pqv", "AAAA", nil},
		{56, "headertype", "http", []string{"none"}},
		{57, "headertype", "none", nil},
		{57, "host", "h1.example.test", nil},
		{57, "path", "/a", nil},
		{7, "host", "other.example.test", nil},
		{60, "path", "/ws?ed=2048x", nil},
		{10, "host", "other.example.test", nil},
		{10, "path", "/other", nil},
		{62, "heartbeatperiod", "1", nil},
		{64, "mode", "packet-up", nil},
		{10, "mode", "stream-up", nil},
		{65, "extra", `{"xPaddingBytes":"1-2"}`, nil},
		{66, "extra", `{"xmux":{"maxConcurrency":"1-2"}}`, nil},
		{67, "extra", `{"noGRPCHeader":true}`, nil},
		{68, "fm", "", nil},
		{71, "fm", "", nil},
	}
	for _, tc := range cases {
		t.Run(tc.param+"/"+strings.TrimSpace(corpusLink(t, tc.line).Label), func(t *testing.T) {
			l := corpusLink(t, tc.line)
			h := handler(t, l)
			values := tc.values
			if values == nil {
				v, _ := l.Get(tc.param)
				values = []string{v}
			}
			for _, v := range values {
				if err := paramcheck.Check(tc.param, v, h); err != nil {
					t.Errorf("%s=%q: %v", tc.param, v, err)
				}
			}
			if tc.param == "fm" {
				// A handler without finalmask fails the fm check.
				if err := paramcheck.Check("fm", "{}", handler(t, corpusLink(t, 1))); err == nil {
					t.Error("fm check passed without finalmask")
				}
				return
			}
			if err := paramcheck.Check(tc.param, tc.wrong, h); err == nil {
				t.Errorf("%s=%q matched a handler built from %q", tc.param, tc.wrong, values)
			}
		})
	}
}

func TestCheckExtraNeedsAField(t *testing.T) {
	// An extra whose keys are all unknown to Xray does not reach the proto.
	h := handler(t, corpusLink(t, 10))
	if err := paramcheck.Check("extra", `{"unknownKey":1}`, h); err == nil {
		t.Fatal("extra without a known field must fail")
	}
	if err := paramcheck.Check("extra", `not json`, h); err == nil {
		t.Fatal("invalid extra must fail")
	}
}

func TestCheckWrongNetwork(t *testing.T) {
	// ws parameters against a tcp handler.
	h := handler(t, corpusLink(t, 1))
	for _, p := range []string{"heartbeatperiod", "mode", "extra", "pbk", "sid", "spx", "pqv", "host", "path"} {
		if err := paramcheck.Check(p, "1", h); err == nil {
			t.Errorf("%s matched a tcp+tls handler", p)
		}
	}
}
