package outbound

import (
	"bytes"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/xtls/xray-core/app/proxyman"
	"github.com/xtls/xray-core/common/serial"
	pmasque "github.com/xtls/xray-core/proxy/masque"
	"github.com/xtls/xray-core/transport/internet/masque"
	"github.com/xtls/xray-core/transport/internet/tls"

	"github.com/a1ekseev/scd/internal/common/candidate"
)

// mqFixture is line n (1-based) of the masque fixture: 1 and 2 are the h3
// and h2 lines of one device, 5 has the endpoint IP as sni.
func mqFixture(t *testing.T, n int) candidate.Masque {
	t.Helper()
	b, err := os.ReadFile("../../../docker/fixtures/masque/candidates.txt")
	if err != nil {
		t.Fatal(err)
	}
	c, err := candidate.ParseMasque(strings.Split(string(b), "\n")[n-1])
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func encodeJSON(t *testing.T, v any) string {
	t.Helper()
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

// The JSON is golden (testdata/masque-<transport>.json) and never holds the
// device key; security is tls for both transports, alpn h2 only for h2.
func TestBuildMasqueJSON(t *testing.T) {
	for _, tc := range []struct {
		line   int
		golden string
	}{{1, "masque-h3.json"}, {2, "masque-h2.json"}} {
		t.Run(tc.golden, func(t *testing.T) {
			m := BuildMasque(mqFixture(t, tc.line), "x-observe-mq-nl-amsterdam-mq-00000000")
			got := encodeJSON(t, m)
			want, err := os.ReadFile("testdata/" + tc.golden)
			if err != nil {
				t.Fatal(err)
			}
			if got != string(want) {
				t.Fatalf("JSON\n%s\nwant\n%s", got, want)
			}
			if !strings.Contains(got, `"privateKey": "<redacted>"`) || strings.Contains(got, "fingerprint") || strings.Contains(got, "pinnedPeerCertSha256") {
				t.Fatalf("JSON %s", got)
			}
		})
	}
}

// ToProto (conf.Build) accepts the JSON; the proto has the SEC 1 key as
// PKCS #8 (Xray converts it), the PKIX endpoint key, tls with the server
// name and alpn h2 only for h2.
func TestMasqueToProto(t *testing.T) {
	for _, n := range []int{1, 2, 5} {
		c := mqFixture(t, n)
		h, err := ToProto(BuildMasque(c, "mq"))
		if err != nil {
			t.Fatal(err)
		}
		pm, err := h.ProxySettings.GetInstance()
		if err != nil {
			t.Fatal(err)
		}
		cc := pm.(*pmasque.ClientConfig)
		if ip := cc.GetServer().GetAddress().GetIp(); !slices.Equal(ip, c.Endpoint.Addr().AsSlice()) || cc.GetServer().GetPort() != 443 ||
			!slices.Equal(cc.RemoteDns, []string{"192.0.2.53"}) {
			t.Fatalf("client %+v", cc)
		}
		sm, err := h.SenderSettings.GetInstance()
		if err != nil {
			t.Fatal(err)
		}
		ss := sm.(*proxyman.SenderConfig).GetStreamSettings()
		if ss.ProtocolName != "masque" || ss.SecurityType != serial.GetMessageType(&tls.Config{}) || len(ss.TransportSettings) != 1 || len(ss.SecuritySettings) != 1 {
			t.Fatalf("stream %+v", ss)
		}
		tm, _ := ss.TransportSettings[0].Settings.GetInstance()
		mc := tm.(*masque.Config)
		key, _ := x509.MarshalPKCS8PrivateKey(c.PrivateKey)
		peer, _ := x509.MarshalPKIXPublicKey(c.PeerKey)
		d := mc.GetWarp()
		if d == nil || !bytes.Equal(d.PrivateKey, key) || !bytes.Equal(d.PublicKey, peer) ||
			!slices.Equal(d.Address, []string{"172.16.0.2/32", "2001:db8::2/128"}) || len(mc.Headers) != 0 {
			t.Fatalf("masque %+v", mc)
		}
		st, _ := ss.SecuritySettings[0].GetInstance()
		tc := st.(*tls.Config)
		wantALPN := []string(nil)
		if c.Transport == candidate.TransportH2 {
			wantALPN = []string{"h2"}
		}
		if tc.ServerName != c.SNI || !slices.Equal(tc.NextProtocol, wantALPN) || tc.Fingerprint != "" || len(tc.PinnedPeerCertSha256) != 0 {
			t.Fatalf("line %d: tls %+v", n, tc)
		}
	}
}

// The key reaches Xray as std base64 SEC 1 DER; the hash and the JSON never
// hold it.
func TestMasqueKeyOnlyInXrayJSON(t *testing.T) {
	c := mqFixture(t, 1)
	sec1, _ := x509.MarshalECPrivateKey(c.PrivateKey)
	key := base64.StdEncoding.EncodeToString(sec1)
	m := BuildMasque(c, "mq")
	raw, err := m.xrayJSON()
	if err != nil || !strings.Contains(string(raw), `"privateKey":"`+key+`"`) {
		t.Fatalf("xray JSON %s, %v", raw, err)
	}
	b, _ := json.Marshal(m)
	if strings.Contains(string(b), key) || strings.Contains(Hash([]Spec{m}), key) {
		t.Fatal("the key leaked")
	}
}
