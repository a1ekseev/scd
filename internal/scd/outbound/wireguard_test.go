package outbound

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"github.com/xtls/xray-core/app/proxyman"
	"github.com/xtls/xray-core/proxy/wireguard"
	"github.com/xtls/xray-core/transport/internet"

	"github.com/a1ekseev/scd/internal/common/candidate"
)

func wgKey(b byte) string { return base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{b}, 32)) }

func wgCandidate(t *testing.T, extra string) candidate.WireGuard {
	t.Helper()
	c, err := candidate.ParseWireGuard("wireguard://" + wgKey(1) + "@192.0.2.10:51820?provider=static&publickey=" + wgKey(2) +
		"&address=10.2.0.2/32,fd00::2&mtu=1420&allowedips=0.0.0.0/0,::/0" + extra)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestBuildWireGuardJSON(t *testing.T) {
	for _, tc := range []struct {
		name, extra, want string
	}{
		{"minimal", "", `{"tag":"x-wg","protocol":"wireguard","settings":{"secretKey":"<redacted>","address":["10.2.0.2/32","fd00::2/128"],` +
			`"peers":[{"publicKey":"` + wgKey(2) + `","endpoint":"192.0.2.10:51820","allowedIPs":["0.0.0.0/0","::/0"]}],` +
			`"mtu":1420,"noKernelTun":true},"streamSettings":{"sockopt":{"domainStrategy":"UseIPv4"}}}`},
		{
			"full", "&dns=10.2.0.1&reserved=1,2,3&keepalive=25&presharedkey=" + wgKey(3),
			`{"tag":"x-wg","protocol":"wireguard","settings":{"secretKey":"<redacted>","address":["10.2.0.2/32","fd00::2/128"],` +
				`"peers":[{"publicKey":"` + wgKey(2) + `","preSharedKey":"<redacted>","endpoint":"192.0.2.10:51820","keepAlive":25,"allowedIPs":["0.0.0.0/0","::/0"]}],` +
				`"mtu":1420,"reserved":[1,2,3],"noKernelTun":true,"remoteDNS":["10.2.0.1"]},"streamSettings":{"sockopt":{"domainStrategy":"UseIPv4"}}}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := BuildWireGuard(wgCandidate(t, tc.extra), "x-wg", "UseIPv4")
			var buf bytes.Buffer
			enc := json.NewEncoder(&buf)
			enc.SetEscapeHTML(false)
			if err := enc.Encode(w); err != nil {
				t.Fatal(err)
			}
			if b := strings.TrimSpace(buf.String()); b != tc.want {
				t.Fatalf("JSON\n%s\nwant\n%s", b, tc.want)
			}
			if h := Hash([]Spec{w}); strings.Contains(h, wgKey(1)) || h != Hash([]Spec{w.WithTag("x-wg")}) {
				t.Fatal("hash")
			}
		})
	}
}

// ToProto sees the keys; the JSON never does.
func TestWireGuardToProto(t *testing.T) {
	w := BuildWireGuard(wgCandidate(t, "&reserved=1,2,3&presharedkey="+wgKey(3)), "x-wg", "UseIPv6v4")
	h, err := ToProto(w.WithTag("wg"))
	if err != nil {
		t.Fatal(err)
	}
	if h.Tag != "wg" {
		t.Fatalf("tag %q", h.Tag)
	}
	m, err := h.ProxySettings.GetInstance()
	if err != nil {
		t.Fatal(err)
	}
	d := m.(*wireguard.DeviceConfig)
	hexKey := func(b byte) string { return hex.EncodeToString(bytes.Repeat([]byte{b}, 32)) }
	if d.SecretKey != hexKey(1) || !d.NoKernelTun || !d.IsClient || d.Mtu != 1420 || !bytes.Equal(d.Reserved, []byte{1, 2, 3}) ||
		len(d.Peers) != 1 || d.Peers[0].PublicKey != hexKey(2) || d.Peers[0].PreSharedKey != hexKey(3) || d.Peers[0].Endpoint != "192.0.2.10:51820" {
		t.Fatalf("device %+v", d)
	}
	sm, err := h.SenderSettings.GetInstance()
	if err != nil {
		t.Fatal(err)
	}
	if ds := sm.(*proxyman.SenderConfig).GetStreamSettings().GetSocketSettings().GetDomainStrategy(); ds != internet.DomainStrategy_USE_IP64 {
		t.Fatalf("domain strategy %v", ds)
	}
}
