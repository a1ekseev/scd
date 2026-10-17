package normalize

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/xtls/xray-core/common/uuid"

	"github.com/a1ekseev/scd/internal/common/subscription"
	"github.com/a1ekseev/scd/internal/common/vless"
)

const (
	corpusDir = "../../../testdata/reference/scd/corpus/"
	testID    = "7c0e4b8a-2f3d-4e6a-9b1c-5d8f0a2e4c61"
	testPbk   = "kp_D9wlmhWWw8QVA9OCZwedG12QDFk68zaolbEh6c1M"
)

// corpusLines returns the lines of d26-links.txt (index 0 = line 1).
func corpusLines(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(corpusDir + "d26-links.txt")
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimRight(string(data), "\n"), "\n")
}

// corpusEncryption returns the encryption parameter of a d26-links.txt line.
func corpusEncryption(t *testing.T, line int) string {
	t.Helper()
	enc, ok := parseLine(t, corpusLines(t)[line-1], line).Get("encryption")
	if !ok {
		t.Fatalf("line %d has no encryption", line)
	}
	return enc
}

// Keys from `xray vlessenc` of Xray 26.10.10 (d26-keys.md).
func x25519Enc(t *testing.T) string { t.Helper(); return corpusEncryption(t, 14) }
func mlkemEnc(t *testing.T) string  { t.Helper(); return corpusEncryption(t, 15) }

// vlessURL builds a link to a private test host from a raw query.
func vlessURL(id, query string) string {
	return "vless://" + id + "@h.example.test:443?" + query + "#🇩🇪 Berlin, Germany"
}

func normalizeQuery(t *testing.T, query string, p Policy) (Normalized, []Warning, *vless.Skip) {
	t.Helper()
	return Normalize(parseLine(t, vlessURL(testID, query), 1), p)
}

func mustEntry(t *testing.T, query string, p Policy) (Normalized, []Warning) {
	t.Helper()
	n, w, skip := normalizeQuery(t, query, p)
	if skip != nil {
		t.Fatalf("%s: unexpected skip %+v", query, *skip)
	}
	return n, w
}

func mustSkip(t *testing.T, query string, p Policy, code, reason string, details ...string) {
	t.Helper()
	_, w, skip := normalizeQuery(t, query, p)
	if skip == nil {
		t.Fatalf("%s: got entry, want skip %q", query, reason)
	}
	if skip.Code != code || skip.Reason != reason {
		t.Fatalf("%s: got %s %q, want %s %q", query, skip.Code, skip.Reason, code, reason)
	}
	if details != nil && !slices.Equal(skip.Details, details) {
		t.Fatalf("%s: details %q, want %q", query, skip.Details, details)
	}
	if skip.Label == nil || *skip.Label != "🇩🇪 Berlin, Germany" || skip.Line != 1 {
		t.Fatalf("%s: skip must carry line and label: %+v", query, *skip)
	}
	if w != nil {
		t.Fatalf("%s: warnings with a skip: %v", query, w)
	}
}

func warningCodes(w []Warning) []string {
	var codes []string
	for _, x := range w {
		codes = append(codes, x.Code)
	}
	return codes
}

const (
	wantCombo      = "Unsupported VLESS type/security/flow combination."
	wantEncryption = "Unsupported VLESS encryption value."
	wantKey        = "Invalid VLESS encryption key."
	wantID         = "The VLESS user id is not accepted by Xray."
	wantFlow       = "Unsupported VLESS flow value."
	wantInsecure   = "allowInsecure must be 0, 1, true, or false."
	wantInsecureNo = "allowInsecure=1 requires pcs or vcn: Xray v26.10.10 no longer supports allowInsecure (set subscriptions[].allowInsecure: allow to keep such links)."
)

func TestProfileMatrix(t *testing.T) {
	x := x25519Enc(t)
	m := mlkemEnc(t)
	const vision = "xtls-rprx-vision"
	tests := []struct {
		name, typ, security, enc, flow string
		profile, network, wantFlow     string
		cleared                        bool
	}{
		{"#1 tcp tls", "tcp", "tls", "", "", "tcp-tls", "tcp", "", false},
		{"#1 raw tls", "raw", "tls", "none", "", "tcp-tls", "tcp", "", false},
		{"#1 no type", "", "tls", "", "", "tcp-tls", "tcp", "", false},
		{"#2 tcp tls vision", "tcp", "tls", "", vision, "tcp-tls", "tcp", vision, false},
		{"#2 tcp tls vision-udp443", "tcp", "tls", "", vision + "-udp443", "tcp-tls", "tcp", vision + "-udp443", false},
		{"#3 tcp reality", "tcp", "reality", "", "", "tcp-reality", "tcp", "", false},
		{"#4 tcp reality vision", "tcp", "reality", "", vision, "tcp-reality-vision", "tcp", vision, false},
		{"#4 raw reality vision", "RAW", "REALITY", "", vision, "tcp-reality-vision", "tcp", vision, false},
		{"#5 ws tls", "ws", "tls", "", "", "ws-tls", "ws", "", false},
		{"#6 ws tls vision", "ws", "tls", "", vision, "ws-tls", "ws", "", true},
		{"#8 xhttp tls", "xhttp", "tls", "", "", "xhttp-tls", "xhttp", "", false},
		{"#9 xhttp reality", "xhttp", "reality", "", "", "xhttp-reality", "xhttp", "", false},
		{"#10 xhttp tls vision", "xhttp", "tls", "", vision, "xhttp-tls", "xhttp", "", true},
		{"#10 xhttp reality vision", "xhttp", "reality", "", vision, "xhttp-reality", "xhttp", "", true},
		{"#11 tcp mlkem", "tcp", "none", x, "", "tcp-mlkem", "tcp", "", false},
		{"#11 tcp mlkem no security", "tcp", "", m, "", "tcp-mlkem", "tcp", "", false},
		{"#11 ws mlkem", "ws", "none", x, "", "ws-mlkem", "ws", "", false},
		{"#11 xhttp mlkem", "xhttp", "none", m, "", "xhttp-mlkem", "xhttp", "", false},
		{"#12 tcp mlkem vision", "tcp", "none", x, vision, "tcp-mlkem", "tcp", vision, false},
		{"#12 ws mlkem vision", "ws", "none", m, vision, "ws-mlkem", "ws", vision, false},
		{"#12 xhttp mlkem vision", "xhttp", "", x, vision, "xhttp-mlkem", "xhttp", vision, false},
		{"#13 tcp tls mlkem", "tcp", "tls", m, "", "tcp-tls", "tcp", "", false},
		{"#13 tcp reality mlkem vision", "tcp", "reality", x, vision, "tcp-reality-vision", "tcp", vision, false},
		{"#13 xhttp reality mlkem", "xhttp", "reality", m, "", "xhttp-reality", "xhttp", "", false},
		{"#13 xhttp reality mlkem vision", "xhttp", "reality", m, vision, "xhttp-reality-vision", "xhttp", vision, false},
		{"#13 xhttp tls mlkem vision", "xhttp", "tls", x, vision, "xhttp-tls", "xhttp", vision, false},
		{"#14 ws tls mlkem vision", "ws", "tls", x, vision, "ws-tls", "ws", vision, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var q []string
			if tc.typ != "" {
				q = append(q, "type="+tc.typ)
			}
			if tc.security != "" {
				q = append(q, "security="+tc.security)
			}
			if tc.enc != "" {
				q = append(q, "encryption="+tc.enc)
			}
			if tc.flow != "" {
				q = append(q, "flow="+tc.flow)
			}
			if strings.EqualFold(tc.security, "reality") {
				q = append(q, "pbk="+testPbk) // the only required REALITY parameter (4.4)
			}
			n, w := mustEntry(t, strings.Join(q, "&"), Policy{})
			if n.Profile != tc.profile || n.Network != tc.network || n.Flow != tc.wantFlow {
				t.Fatalf("got profile %s network %s flow %q, want %s %s %q",
					n.Profile, n.Network, n.Flow, tc.profile, tc.network, tc.wantFlow)
			}
			wantSec := strings.ToLower(tc.security)
			if wantSec == "" {
				wantSec = "none"
			}
			if n.Security != wantSec {
				t.Fatalf("security %q, want %q", n.Security, wantSec)
			}
			wantEnc := tc.enc
			if wantEnc == "" {
				wantEnc = "none"
			}
			if n.Encryption != wantEnc {
				t.Fatalf("encryption %q, want %q", n.Encryption, wantEnc)
			}
			if tc.cleared {
				want := []Warning{{
					Code:    "flow_cleared",
					Message: "flow " + tc.flow + " is not supported on " + n.Network + "+" + n.Security + "; flow cleared.",
				}}
				if !slices.Equal(w, want) {
					t.Fatalf("warnings %v, want %v", w, want)
				}
			} else if w != nil {
				t.Fatalf("unexpected warnings %v", w)
			}
			if !strings.HasPrefix(n.Tag, "vless-de-berlin-"+tc.profile+"-") {
				t.Fatalf("tag %s", n.Tag)
			}
		})
	}
}

func TestUnsupportedCombos(t *testing.T) {
	tests := []struct {
		name, query string
		details     []string
	}{
		{"grpc (P19)", "type=grpc&security=tls&serviceName=svc", []string{"type=grpc", "security=tls", "flow="}},
		{"httpupgrade", "type=httpupgrade&security=tls", []string{"type=httpupgrade", "security=tls", "flow="}},
		{"kcp with ML-KEM", "type=kcp&encryption=" + x25519Enc(t), []string{"type=kcp", "security=", "flow="}},
		{"ws reality", "type=ws&security=reality&pbk=" + testPbk, []string{"type=ws", "security=reality", "flow="}},
		{"tcp without security", "type=tcp", []string{"type=tcp", "security=", "flow="}},
		{"no type, no security", "sni=x", []string{"type=tcp", "security=", "flow="}},
		{"security none", "type=TCP&security=none&flow=xtls-rprx-vision", []string{"type=TCP", "security=none", "flow=xtls-rprx-vision"}},
		{"none with encryption=none", "type=ws&security=none&encryption=none", []string{"type=ws", "security=none", "flow="}},
		{"xtls", "type=tcp&security=xtls", []string{"type=tcp", "security=xtls", "flow="}},
		{"unknown security", "security=foo", []string{"type=tcp", "security=foo", "flow="}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mustSkip(t, tc.query, Policy{AllowInsecure: true}, vless.CodeUnsupportedCombo, wantCombo, tc.details...)
		})
	}
}

func TestEncryptionValues(t *testing.T) {
	mustSkip(t, "type=tcp&security=tls&encryption=None", Policy{}, vless.CodeUnsupportedValue, wantEncryption, "encryption=None")
	mustSkip(t, "type=tcp&security=tls&encryption=aes-128-gcm", Policy{}, vless.CodeUnsupportedValue, wantEncryption, "encryption=aes-128-gcm")
	for _, enc := range []string{"", "encryption=", "encryption=none"} {
		n, _ := mustEntry(t, "type=tcp&security=tls&"+enc, Policy{})
		if n.Encryption != "none" {
			t.Fatalf("%q: encryption %q", enc, n.Encryption)
		}
	}
	m := mlkemEnc(t)
	n, _ := mustEntry(t, "type=tcp&security=none&encryption="+m, Policy{})
	if n.Profile != "tcp-mlkem" || n.Encryption != m {
		t.Fatalf("got %s %q", n.Profile, n.Encryption)
	}
}

func TestMLKEMX25519KeyAccepted(t *testing.T) {
	if err := validateMLKEM(x25519Enc(t)); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"xorpub.1rtt", "random.0rtt"} {
		enc := strings.Replace(x25519Enc(t), "native.0rtt", mode, 1)
		if err := validateMLKEM(enc); err != nil {
			t.Fatalf("%s: %v", mode, err)
		}
	}
}

func TestMLKEMKeyAccepted(t *testing.T) {
	if err := validateMLKEM(mlkemEnc(t)); err != nil {
		t.Fatal(err)
	}
	// Two keys (X25519 and ML-KEM-768), d26-links.txt line 31.
	two := x25519Enc(t) + "." + strings.TrimPrefix(mlkemEnc(t), "mlkem768x25519plus.native.0rtt.")
	if err := validateMLKEM(two); err != nil {
		t.Fatal(err)
	}
}

func TestMLKEMInvalidKeySkipped(t *testing.T) {
	const prefix = "mlkem768x25519plus.native.0rtt."
	notKey := prefix + base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0xff}, 1184))
	tests := map[string]string{
		"corpus line 33 (random 1184 bytes)": corpusEncryption(t, 33),
		"1184 bytes 0xff":                    notKey,
		"16 bytes":                           prefix + base64.RawURLEncoding.EncodeToString(make([]byte, 16)),
		"std base64 alphabet":                prefix + strings.Repeat("ab/c", 11),
		"padded base64":                      x25519Enc(t) + "=",
		"good key then bad key":              x25519Enc(t) + "." + strings.TrimPrefix(notKey, prefix),
		"padding segments only":              prefix + "100-111-1111.75-0-111",
		// Xray slices the padding off the front (vless.go:357-371): padding
		// after a key corrupts both.
		"key then padding":              x25519Enc(t) + ".100-111-1111",
		"key, padding, key":             x25519Enc(t) + ".100-111-1111." + strings.TrimPrefix(x25519Enc(t), prefix),
		"padding abc":                   prefix + "abc." + strings.TrimPrefix(x25519Enc(t), prefix),
		"padding not numbers":           prefix + "100-x-111." + strings.TrimPrefix(x25519Enc(t), prefix),
		"first padding below 35":        prefix + "100-34-111." + strings.TrimPrefix(x25519Enc(t), prefix),
		"first padding count below 100": prefix + "99-111-111." + strings.TrimPrefix(x25519Enc(t), prefix),
		"empty padding segment":         prefix + "100-111-111.." + strings.TrimPrefix(x25519Enc(t), prefix),
		"padding over 65553":            prefix + "100-111-111.0-0-0.0-65553-1." + strings.TrimPrefix(x25519Enc(t), prefix),
	}
	for name, enc := range tests {
		t.Run(name, func(t *testing.T) {
			if err := validateMLKEM(enc); err == nil {
				t.Fatal("validateMLKEM accepted an invalid key")
			}
			mustSkip(t, "type=tcp&security=none&encryption="+enc, Policy{},
				vless.CodeUnsupportedValue, wantKey, "encryption="+enc[:40]+"…")
		})
	}
}

func TestMLKEMValidPaddingAccepted(t *testing.T) {
	key := strings.TrimPrefix(x25519Enc(t), "mlkem768x25519plus.native.0rtt.")
	for _, padding := range []string{"100-111-1111.75-0-111", "100-35-35", "100-111-1111.75-0-111.50-0-3333.1-1-1"} {
		enc := "mlkem768x25519plus.native.0rtt." + padding + "." + key
		if err := validateMLKEM(enc); err != nil {
			t.Fatalf("%s: %v", padding, err)
		}
		n, w := mustEntry(t, "type=tcp&encryption="+enc, Policy{})
		if n.Encryption != enc || w != nil {
			t.Fatalf("got %q %v", n.Encryption, w)
		}
	}
	// Mode and rtt are left to conf (stage xray): not checked here.
	for _, mode := range []string{"foo.0rtt", "native.2rtt"} {
		if err := validateMLKEM(strings.Replace(x25519Enc(t), "native.0rtt", mode, 1)); err != nil {
			t.Fatalf("%s: %v", mode, err)
		}
	}
}

func TestMLKEMNative0RTTRealShapeKey(t *testing.T) {
	data, err := os.ReadFile("../../../docker/fixtures/real-shapes/remnawave.txt")
	if err != nil {
		t.Fatal(err)
	}
	first, _, _ := strings.Cut(string(data), "\n")
	enc, _ := parseLine(t, first, 1).Get("encryption")
	if !strings.HasPrefix(enc, "mlkem768x25519plus.native.0rtt.") {
		t.Fatalf("unexpected encryption %.40s", enc)
	}
	key := enc[len("mlkem768x25519plus.native.0rtt."):]
	if b, err := base64.RawURLEncoding.DecodeString(key); err != nil || len(b) != 1184 {
		t.Fatalf("expected one 1184-byte key, got %d bytes (%v)", len(b), err)
	}
	if err := validateMLKEM(enc); err != nil {
		t.Fatal(err)
	}
}

func TestFlow(t *testing.T) {
	t.Run("VisionClearedOnWSTLS", func(t *testing.T) {
		n, w := mustEntry(t, "type=ws&security=tls&flow=xtls-rprx-vision&path=/ws", Policy{})
		want := []Warning{{"flow_cleared", "flow xtls-rprx-vision is not supported on ws+tls; flow cleared."}}
		if n.Flow != "" || n.Profile != "ws-tls" || !slices.Equal(w, want) {
			t.Fatalf("got %q %s %v", n.Flow, n.Profile, w)
		}
	})
	t.Run("VisionClearedOnXHTTPReality", func(t *testing.T) {
		n, w := mustEntry(t, "type=xhttp&security=reality&flow=xtls-rprx-vision-udp443&pbk="+testPbk, Policy{})
		want := []Warning{{"flow_cleared", "flow xtls-rprx-vision-udp443 is not supported on xhttp+reality; flow cleared."}}
		if n.Flow != "" || n.Profile != "xhttp-reality" || !slices.Equal(w, want) {
			t.Fatalf("got %q %s %v", n.Flow, n.Profile, w)
		}
	})
	t.Run("VisionKeptWithMLKEMOnWS", func(t *testing.T) {
		n, w := mustEntry(t, "type=ws&security=none&flow=xtls-rprx-vision&encryption="+mlkemEnc(t), Policy{})
		if n.Flow != "xtls-rprx-vision" || n.Profile != "ws-mlkem" || w != nil {
			t.Fatalf("got %q %s %v", n.Flow, n.Profile, w)
		}
	})
	t.Run("VisionUdp443Kept", func(t *testing.T) {
		n, w := mustEntry(t, "type=tcp&security=reality&flow=xtls-rprx-vision-udp443&pbk="+testPbk, Policy{})
		if n.Flow != "xtls-rprx-vision-udp443" || n.Profile != "tcp-reality-vision" || w != nil {
			t.Fatalf("got %q %s %v", n.Flow, n.Profile, w)
		}
	})
	t.Run("Bogus", func(t *testing.T) {
		mustSkip(t, "type=tcp&security=tls&flow=xtls-rprx-bogus", Policy{},
			vless.CodeUnsupportedValue, wantFlow, "flow=xtls-rprx-bogus")
		mustSkip(t, "type=ws&security=tls&flow=XTLS-RPRX-VISION", Policy{},
			vless.CodeUnsupportedValue, wantFlow, "flow=XTLS-RPRX-VISION")
	})
	t.Run("EmptyFlow", func(t *testing.T) {
		n, w := mustEntry(t, "type=ws&security=tls&flow=", Policy{})
		if n.Flow != "" || w != nil {
			t.Fatalf("got %q %v", n.Flow, w)
		}
	})
}

func TestUserID(t *testing.T) {
	want, err := uuid.ParseString("my-user")
	if err != nil {
		t.Fatal(err)
	}
	n, _, skip := Normalize(parseLine(t, vlessURL("my-user", "type=tcp&security=tls"), 1), Policy{})
	if skip != nil || n.UUID != want.String() || !strings.HasPrefix(n.UUID, "db93355e-") {
		t.Fatalf("my-user: got %q (%v), want %s", n.UUID, skip, want.String())
	}
	n, _, skip = Normalize(parseLine(t, vlessURL(strings.ToUpper(testID), "type=tcp&security=tls"), 1), Policy{})
	if skip != nil || n.UUID != testID {
		t.Fatalf("uppercase: got %q (%v)", n.UUID, skip)
	}
	for _, id := range []string{"zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz", strings.Repeat("a", 31), strings.Repeat("a", 37)} {
		_, _, skip = Normalize(parseLine(t, vlessURL(id, "type=tcp&security=tls"), 1), Policy{})
		if skip == nil || skip.Code != vless.CodeUnsupportedValue || skip.Reason != wantID {
			t.Fatalf("%s: got %+v", id, skip)
		}
	}
}

func TestAllowInsecure(t *testing.T) {
	const pcs = "2D:AA:13:89:E3:E6:0F:82:11:59:DE:ED:A7:5E:55:BD:ED:FA:90:B7:31:A4:02:82:36:98:36:A7:2A:5B:4B:A1"
	for _, v := range []string{"", "yes", "2"} {
		for _, p := range []Policy{{}, {AllowInsecure: true}} {
			mustSkip(t, "type=tcp&security=tls&allowInsecure="+v, p,
				vless.CodeUnsupportedValue, wantInsecure, "allowInsecure="+v)
		}
	}
	mustSkip(t, "type=tcp&security=tls&allowInsecure=1", Policy{}, vless.CodeUnsupportedValue, wantInsecureNo)
	mustSkip(t, "type=ws&security=tls&allowInsecure=TRUE&pcs=", Policy{}, vless.CodeUnsupportedValue, wantInsecureNo)

	n, w := mustEntry(t, "type=tcp&security=tls&allowInsecure=1", Policy{AllowInsecure: true})
	want := []Warning{{"allow_insecure_ignored", "allowInsecure is not supported by Xray v26.10.10; the outbound will verify certificates (use pcs/vcn)."}}
	if !slices.Equal(w, want) || n.AllowInsecure == nil || !*n.AllowInsecure {
		t.Fatalf("allow: got %v %v", w, n.AllowInsecure)
	}
	for _, q := range []string{"allowInsecure=1&vcn=a.example.test", "allowInsecure=True&pcs=" + pcs, "allowInsecure=0", "allowInsecure=FALSE"} {
		_, w := mustEntry(t, "type=tcp&security=tls&"+q, Policy{})
		if w != nil {
			t.Fatalf("%s: warnings %v", q, w)
		}
	}
	n, _ = mustEntry(t, "type=tcp&security=tls&allowInsecure=0", Policy{})
	if n.AllowInsecure == nil || *n.AllowInsecure {
		t.Fatalf("allowInsecure=0: got %v", n.AllowInsecure)
	}
	n, _ = mustEntry(t, "type=tcp&security=tls", Policy{})
	if n.AllowInsecure != nil {
		t.Fatal("absent allowInsecure must stay nil")
	}
	// allowInsecure is a TLS parameter: with reality or ML-KEM over none it
	// does not apply and is dropped silently (O16).
	for _, q := range []string{"type=tcp&security=reality&pbk=" + testPbk + "&allowInsecure=1", "type=tcp&encryption=" + x25519Enc(t) + "&allowInsecure=yes"} {
		n, w := mustEntry(t, q, Policy{})
		if w != nil || n.AllowInsecure != nil {
			t.Fatalf("%s: got %v %v", q, w, n.AllowInsecure)
		}
	}
}

func TestIrrelevantParamsIgnored(t *testing.T) {
	const pcs = "2DAA1389E3E60F821159DEEDA75E55BDEDFA90B731A40282369836A72A5B4BA1"
	q := "type=tcp&security=reality&sni=www.example.test&fp=chrome&pbk=" + testPbk +
		"&sid=ab&alpn=h2&ech=AAAA&pcs=" + pcs + "&cs=TLS_AES_128_GCM_SHA256&support-x25519mlkem768=true" +
		"&serviceName=svc&seed=s&x_padding_bytes=100-1000&host=h.example.test&path=%2F"
	n, w := mustEntry(t, q, Policy{})
	if w != nil {
		t.Fatalf("warnings %v", w)
	}
	if len(n.ALPN) != 0 || n.ECHConfigList != "" || n.PinnedPeerCertSha256 != nil || n.CipherSuites != "" || n.Path != "" || n.Host != "" {
		t.Fatalf("inapplicable params leaked: %+v", n)
	}
	var keys []string
	for _, p := range n.Query {
		keys = append(keys, p.Key)
	}
	want := []string{"type", "security", "sni", "fp", "pbk", "sid", "alpn", "ech", "pcs", "cs", "support-x25519mlkem768", "servicename", "seed", "x_padding_bytes", "host", "path"}
	if !slices.Equal(keys, want) {
		t.Fatalf("query keys %v, want %v", keys, want)
	}

	// provider2.txt shape: tcp+tls with path=/ and no headerType (12 §7 p.4).
	data, err := os.ReadFile("../../../docker/fixtures/real-shapes/provider2.txt")
	if err != nil {
		t.Fatal(err)
	}
	lines := subscription.ScanLines(string(data))
	if len(lines) != 50 {
		t.Fatalf("provider2.txt: %d lines", len(lines))
	}
	for _, sl := range lines {
		l, skip := vless.Parse(sl)
		if skip != nil {
			t.Fatalf("line %d: %+v", sl.Number, *skip)
		}
		n, w, skip := Normalize(l, Policy{})
		if skip != nil || w != nil || n.Path != "" || n.Profile != "tcp-tls" {
			t.Fatalf("line %d: got %+v %v %v", sl.Number, skip, w, n.Path)
		}
	}
}

func TestUnknownParamWarning(t *testing.T) {
	n, w := mustEntry(t, "type=tcp&foo=1&security=tls&bar=2&foo=3", Policy{})
	want := []Warning{{"param_ignored", "Ignored unknown VLESS parameter(s): foo, bar."}}
	if !slices.Equal(w, want) {
		t.Fatalf("warnings %v, want %v", w, want)
	}
	for _, p := range n.Query {
		if p.Key == "foo" || p.Key == "bar" {
			t.Fatalf("unknown param %s in query", p.Key)
		}
	}
}

func TestWarningOrder(t *testing.T) {
	_, w := mustEntry(t, "type=ws&security=tls&flow=xtls-rprx-vision&allowInsecure=1&zz=1", Policy{AllowInsecure: true})
	if got := warningCodes(w); !slices.Equal(got, []string{"flow_cleared", "allow_insecure_ignored", "param_ignored"}) {
		t.Fatalf("got %v", got)
	}
}

func TestCheckOrder(t *testing.T) {
	// combo before encryption, encryption before id, id before flow, flow
	// before allowInsecure.
	l := parseLine(t, vlessURL("zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz", "type=grpc&security=tls&encryption=None&flow=bogus&allowInsecure=x"), 1)
	if _, _, s := Normalize(l, Policy{}); s == nil || s.Reason != wantCombo {
		t.Fatalf("got %+v", s)
	}
	l = parseLine(t, vlessURL("zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz", "security=tls&encryption=None&flow=bogus&allowInsecure=x"), 1)
	if _, _, s := Normalize(l, Policy{}); s == nil || s.Reason != wantEncryption {
		t.Fatalf("got %+v", s)
	}
	l = parseLine(t, vlessURL("zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz", "security=tls&flow=bogus&allowInsecure=x"), 1)
	if _, _, s := Normalize(l, Policy{}); s == nil || s.Reason != wantID {
		t.Fatalf("got %+v", s)
	}
	l = parseLine(t, vlessURL(testID, "security=tls&flow=bogus&allowInsecure=x"), 1)
	if _, _, s := Normalize(l, Policy{}); s == nil || s.Reason != wantFlow {
		t.Fatalf("got %+v", s)
	}
}

func TestNormalizedFields(t *testing.T) {
	raw := "vless://" + testID + "@h.example.test:8443?type=tcp&security=tls#🇩🇪 Berlin, Germany"
	n, w, skip := Normalize(parseLine(t, raw, 7), Policy{})
	if skip != nil || w != nil {
		t.Fatalf("got %v %v", skip, w)
	}
	if n.Kind != "outbound" || n.Protocol != "vless" || n.Line != 7 || n.Raw != raw || n.Address != "h.example.test" ||
		n.Port != 8443 || n.City != "Berlin" || n.Country == nil || n.Country.ISO2 != "DE" ||
		n.Tag != BuildTag(n.Label, n.Address, "tcp-tls", raw) || n.ALPN == nil {
		t.Fatalf("got %+v", n)
	}
	data, err := json.Marshal(n)
	if err != nil {
		t.Fatal(err)
	}
	rawJSON, _ := json.Marshal(raw)
	want := `{"kind":"outbound","protocol":"vless","profile":"tcp-tls","tag":"` + n.Tag + `","line":7,` +
		`"raw":` + string(rawJSON) + `,"label":"🇩🇪 Berlin, Germany","address":"h.example.test","port":8443,` +
		`"uuid":"` + testID + `","encryption":"none","network":"tcp","security":"tls","alpn":[],` +
		`"country":{"emoji":"🇩🇪","iso2":"DE","nameEn":"Germany","nameRu":"Германия"},"city":"Berlin",` +
		`"query":{"type":"tcp","security":"tls"}}`
	if string(data) != want {
		t.Fatalf("JSON\n got %s\nwant %s", data, want)
	}
	empty, err := json.Marshal(Normalized{})
	if err != nil || !strings.HasSuffix(string(empty), `"alpn":null,"query":{}}`) {
		t.Fatalf("empty query must be an object: %s %v", empty, err)
	}
}

// d26Owner lists the corpus rows that stage scd skips in later tasks
// (applyTransport 4.5); until then they are entries.
var d26Owner = map[int]string{
	63: "4.5 heartbeatPeriod", 90: "4.5 extra", 91: "4.5 extra", 92: "4.5 fm",
}

func TestD26Corpus(t *testing.T) {
	data, err := os.ReadFile(corpusDir + "d26-expect.json")
	if err != nil {
		t.Fatal(err)
	}
	var rows []struct {
		Line           int      `json:"line"`
		Expect         string   `json:"expect"`
		Stage          string   `json:"stage"`
		ReasonCode     string   `json:"reasonCode"`
		ExpectContains string   `json:"expectContains"`
		Warnings       []string `json:"warnings"`
		Profile        string   `json:"profile"`
	}
	if err := json.Unmarshal(data, &rows); err != nil {
		t.Fatal(err)
	}
	lines := corpusLines(t)
	if len(rows) != len(lines) {
		t.Fatalf("%d rows, %d lines", len(rows), len(lines))
	}
	checked := 0
	for _, r := range rows {
		raw := lines[r.Line-1]
		l, pskip := vless.Parse(subscription.Line{Number: r.Line, Raw: raw, Trimmed: strings.TrimSpace(raw)})
		if r.Stage == "parse" {
			if pskip == nil || pskip.Code != r.ReasonCode {
				t.Errorf("line %d: parser result %+v, want %s", r.Line, pskip, r.ReasonCode)
			}
			continue
		}
		if pskip != nil {
			t.Errorf("line %d: parser skipped: %+v", r.Line, *pskip)
			continue
		}
		n, w, skip := Normalize(l, Policy{})
		owner, deferred := d26Owner[r.Line]
		switch {
		case r.Expect == "entry" || r.Stage == "xray":
			// xray-stage rows are rejected later by conf.Build/live Xray.
			if skip != nil {
				t.Errorf("line %d: got skip %+v, want entry", r.Line, *skip)
				continue
			}
			if r.Expect == "entry" && n.Profile != r.Profile {
				t.Errorf("line %d: profile %s, want %s", r.Line, n.Profile, r.Profile)
			}
			if r.Expect == "entry" && !slices.Equal(warningCodes(w), r.Warnings) {
				t.Errorf("line %d: warnings %v, want %v", r.Line, w, r.Warnings)
			}
		case deferred:
			if skip != nil {
				t.Errorf("line %d: stale deferral to %s, normalize already skips: %+v", r.Line, owner, *skip)
			}
		default:
			if skip == nil {
				t.Errorf("line %d: got entry %s, want skip %q", r.Line, n.Profile, r.ExpectContains)
				continue
			}
			if skip.Code != r.ReasonCode || !strings.Contains(skip.Reason, r.ExpectContains) {
				t.Errorf("line %d: got %s %q, want %s %q", r.Line, skip.Code, skip.Reason, r.ReasonCode, r.ExpectContains)
			}
		}
		checked++
	}
	if checked != 105 {
		t.Fatalf("checked %d normalize rows, want 105", checked)
	}
}

func TestNormalizeMatchesTSEntries(t *testing.T) {
	data, err := os.ReadFile(corpusDir + "ts-parse.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Line int    `json:"line"`
		Raw  string `json:"raw"`
		TS   struct {
			OK         bool `json:"ok"`
			Normalized struct {
				Profile, Tag, Network, Security, Flow, Encryption, UUID string
			} `json:"normalized"`
		} `json:"ts"`
	}
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, c := range cases {
		if !c.TS.OK {
			continue
		}
		l, pskip := vless.Parse(subscription.Line{Number: c.Line, Raw: c.Raw, Trimmed: strings.TrimSpace(c.Raw)})
		if pskip != nil || strings.Contains(l.Label, "\t") {
			continue // parser divergences, covered by TestBuildTagMatchesTSCorpus
		}
		n, _, skip := Normalize(l, Policy{AllowInsecure: true})
		if skip != nil {
			t.Errorf("line %d: TS entry skipped: %+v", c.Line, *skip)
			continue
		}
		ts := c.TS.Normalized
		if n.Profile != ts.Profile || n.Tag != ts.Tag || n.Network != ts.Network || n.Security != ts.Security ||
			n.Flow != ts.Flow || n.Encryption != ts.Encryption || !strings.EqualFold(n.UUID, ts.UUID) {
			t.Errorf("line %d: got %s %s %s/%s %q %s, TS %s %s %s/%s %q %s", c.Line,
				n.Profile, n.Tag, n.Network, n.Security, n.Flow, n.UUID,
				ts.Profile, ts.Tag, ts.Network, ts.Security, ts.Flow, ts.UUID)
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("no TS entries compared")
	}
	t.Logf("compared %d TS entries", checked)
}
