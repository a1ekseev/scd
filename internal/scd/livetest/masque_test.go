//go:build xraylive

package livetest

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/xtls/xray-core/core"
	"github.com/xtls/xray-core/infra/conf"

	"github.com/a1ekseev/scd/internal/common/candidate"
	"github.com/a1ekseev/scd/internal/scd/manifest"
	"github.com/a1ekseev/scd/internal/scd/outbound"
	"github.com/a1ekseev/scd/internal/scd/resources"
	"github.com/a1ekseev/scd/internal/scd/topology"
)

// A real Xray accepts the MASQUE outbounds scd builds, h3 and h2
// (AddOutbound runs the security check that conf.Build skips), and removes
// them; no network is used: the tunnel is dialled on the first connection,
// which this test never makes.
func TestLiveMasqueOutbound(t *testing.T) {
	c, _, _ := startXray(t)
	b, err := os.ReadFile(root + "docker/fixtures/masque/candidates.txt")
	if err != nil {
		t.Fatal(err)
	}
	m := manifest.BuildCandidates(candidate.KindMasque, string(b), "mq", "mq", "", time.Unix(0, 0))
	transports := map[string]bool{}
	for _, e := range m.Entries {
		transports[e.SelectionCandidate().Transport] = true
		h, err := outbound.ToProto(e.JSONOutbound.WithTag("x-observe-" + e.Tag))
		if err != nil {
			t.Fatal(err)
		}
		if err := c.AddOutbound(t.Context(), h); err != nil {
			t.Fatalf("AddOutbound %s: %v", e.Tag, err)
		}
	}
	if len(m.Entries) == 0 || !transports["h3"] || !transports["h2"] {
		t.Fatalf("entries %d, transports %v", len(m.Entries), transports)
	}
	mine := func(h *core.OutboundHandlerConfig) bool { return strings.HasPrefix(h.GetTag(), "x-observe-mq-") }
	list, err := c.ListOutbounds(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var tags []string
	for _, h := range list {
		if mine(h) {
			tags = append(tags, h.GetTag())
		}
	}
	if len(tags) != len(m.Entries) {
		t.Fatalf("Xray has %v, added %d", tags, len(m.Entries))
	}
	for _, tag := range tags {
		if err := c.RemoveOutbound(t.Context(), tag); err != nil {
			t.Fatalf("RemoveOutbound %s: %v", tag, err)
		}
	}
	if list, err = c.ListOutbounds(t.Context()); err != nil || slices.ContainsFunc(list, mine) {
		t.Fatalf("MASQUE outbounds left in Xray (%v)", err)
	}
}

// Without security tls the same outbound passes conf.Build but AddOutbound
// fails, and Xray keeps running: the reason the builder always sets it.
func TestLiveMasqueNeedsTLS(t *testing.T) {
	c, _, _ := startXray(t)
	b, err := os.ReadFile(root + "docker/fixtures/masque/candidates.txt")
	if err != nil {
		t.Fatal(err)
	}
	m := manifest.BuildCandidates(candidate.KindMasque, string(b), "mq", "mq", "", time.Unix(0, 0))
	ob := m.Entries[0].JSONOutbound.(outbound.Masque)
	ob.StreamSettings.Security = "none"
	h, err := outbound.ToProto(ob)
	if err != nil {
		t.Fatalf("conf.Build: %v", err)
	}
	if err := c.AddOutbound(t.Context(), h); err == nil || !strings.Contains(err.Error(), "tls") {
		t.Fatalf("AddOutbound without tls: %v", err)
	}
	if _, err := c.ListOutbounds(t.Context()); err != nil {
		t.Fatalf("Xray after the refused add: %v", err)
	}
}

// selfSignedCert writes a self-signed P-256 certificate for localhost and
// its key to dir and returns their paths and the hex sha256 of the
// certificate (the client's pinnedPeerCertSha256).
func selfSignedCert(t *testing.T, dir string) (certFile, keyFile, sha string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "localhost"},
		DNSNames:     []string{"localhost"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	pkcs8, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certFile, keyFile = filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	for path, b := range map[string]*pem.Block{certFile: {Type: "CERTIFICATE", Bytes: der}, keyFile: {Type: "PRIVATE KEY", Bytes: pkcs8}} {
		if err := os.WriteFile(path, pem.EncodeToMemory(b), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	sum := sha256.Sum256(der)
	return certFile, keyFile, hex.EncodeToString(sum[:])
}

// TestLiveMasqueTraffic: a second Xray with the RFC 9484 masque inbound
// (self-signed TLS, h3 and h2) on 127.0.0.1 is the peer; an RFC-mode masque
// outbound (user/pass, pinnedPeerCertSha256) added through AddOutbound with
// a SOCKS inbound and a rule carries an HTTP request to the HTTP server
// behind the peer, over h3 and over h2, and RemoveOutbound takes it out.
// scd's own WARP-mode JSON speaks the WARP dialect, which this inbound does
// not, so for it only AddOutbound is checked (TestLiveMasqueOutbound).
func TestLiveMasqueTraffic(t *testing.T) {
	c, _, _ := startXray(t)
	certFile, keyFile, sha := selfSignedCert(t, t.TempDir())
	vars := map[string]string{
		"{{MASQUE_PORT}}": strconv.Itoa(freeUDPPort(t, true)),
		"{{MASQUE_PASS}}": rand.Text(),
		"{{CERT_FILE}}":   certFile,
		"{{KEY_FILE}}":    keyFile,
		"{{CERT_SHA256}}": sha,
		"{{HTTP_ADDR}}":   tunnelServer(t),
	}
	launchXrayConf(t, "testdata/masque-peer", vars)
	tmpl, err := os.ReadFile("testdata/masque-rfc-outbound.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, alpn := range []string{"h3", "h2"} {
		t.Run(alpn, func(t *testing.T) {
			vars["{{ALPN}}"] = alpn
			var pairs []string
			for k, v := range vars {
				pairs = append(pairs, k, v)
			}
			var dc conf.OutboundDetourConfig
			if err := json.Unmarshal([]byte(strings.NewReplacer(pairs...).Replace(string(tmpl))), &dc); err != nil {
				t.Fatal(err)
			}
			h, err := dc.Build()
			if err != nil {
				t.Fatal(err)
			}
			if err := c.AddOutbound(t.Context(), h); err != nil {
				t.Fatalf("AddOutbound: %v", err)
			}
			tun := topology.Tunnel{InboundTag: "in-mq-" + alpn, RouteTag: "route-mq-" + alpn, Listen: "127.0.0.1", Port: freePort(t)}
			in, err := resources.Inbound(tun)
			if err != nil {
				t.Fatal(err)
			}
			if err := c.AddInbound(t.Context(), in); err != nil {
				t.Fatalf("AddInbound: %v", err)
			}
			if err := c.AddRule(t.Context(), resources.Rule(tun, h.Tag)); err != nil {
				t.Fatalf("AddRule: %v", err)
			}
			getThroughSOCKS(t, net.JoinHostPort(tun.Listen, strconv.Itoa(tun.Port)))

			if err := c.RemoveRule(t.Context(), tun.RouteTag); err != nil {
				t.Fatal(err)
			}
			if err := c.RemoveInbound(t.Context(), tun.InboundTag); err != nil {
				t.Fatal(err)
			}
			if err := c.RemoveOutbound(t.Context(), h.Tag); err != nil {
				t.Fatalf("RemoveOutbound: %v", err)
			}
			if list, err := c.ListOutbounds(t.Context()); err != nil || slices.ContainsFunc(list, func(o *core.OutboundHandlerConfig) bool { return o.Tag == h.Tag }) {
				t.Fatalf("%s left in Xray (%v)", h.Tag, err)
			}
		})
	}
}
