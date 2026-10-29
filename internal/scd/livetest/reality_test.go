//go:build xraylive

package livetest

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/pem"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/xtls/xray-core/app/proxyman"
	"github.com/xtls/xray-core/core"
	"github.com/xtls/xray-core/transport/internet/reality"
	"golang.org/x/net/proxy"

	"github.com/a1ekseev/scd/internal/common/subscription"
	"github.com/a1ekseev/scd/internal/common/vless"
	"github.com/a1ekseev/scd/internal/scd/normalize"
	"github.com/a1ekseev/scd/internal/scd/outbound"
	"github.com/a1ekseev/scd/internal/scd/resources"
	"github.com/a1ekseev/scd/internal/scd/topology"
)

const (
	realCertLog   = "REALITY: received real certificate"
	aliveAfter    = 2 * time.Second
	realCertWait  = 10 * time.Second
	requestExpiry = 5 * time.Second
)

// realityLink is a tcp+REALITY+vision link to addr with a random public key
// and the serverName example.com, a SAN of the httptest certificate.
func realityLink(t *testing.T, addr string) subscription.Line {
	t.Helper()
	pbk := make([]byte, 32)
	_, _ = rand.Read(pbk)
	raw := "vless://00000000-0000-4000-8000-000000000001@" + addr +
		"?type=tcp&security=reality&encryption=none&fp=chrome&flow=xtls-rprx-vision" +
		"&sni=example.com&sid=ab&pbk=" + base64.RawURLEncoding.EncodeToString(pbk) + "#reality-live"
	return subscription.Line{Number: 1, Raw: raw, Trimmed: raw}
}

// realityOutbound builds the link the way scd does (Parse → Normalize →
// Build → ToProto) and checks that SpiderX/SpiderY are filled (D13).
func realityOutbound(t *testing.T, line subscription.Line) *core.OutboundHandlerConfig {
	t.Helper()
	link, skip := vless.Parse(line)
	if skip != nil {
		t.Fatalf("parse skip: %+v", *skip)
	}
	n, _, skip := normalize.Normalize(link, normalize.Policy{TagPrefix: "vless"})
	if skip != nil {
		t.Fatalf("normalize skip: %+v", *skip)
	}
	h, err := outbound.ToProto(outbound.Build(n))
	if err != nil {
		t.Fatal(err)
	}
	ss, err := h.SenderSettings.GetInstance()
	if err != nil {
		t.Fatal(err)
	}
	sec := ss.(*proxyman.SenderConfig).GetStreamSettings().GetSecuritySettings()
	if len(sec) != 1 {
		t.Fatalf("security settings = %v", sec)
	}
	rc, err := sec[0].GetInstance()
	if err != nil {
		t.Fatal(err)
	}
	r, ok := rc.(*reality.Config)
	if !ok || r.SpiderX == "" || len(r.SpiderY) != 10 {
		t.Fatalf("REALITY config without SpiderX/SpiderY (D13): %v", rc)
	}
	return h
}

// writeCertPEM writes the certificate of srv as a PEM file for SSL_CERT_FILE.
func writeCertPEM(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ca.pem")
	data := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// requestThroughSOCKS sends one HTTP request to target through the SOCKS5
// inbound at socksAddr. It is expected to fail (the REALITY handshake does
// not complete), so the result is only logged.
func requestThroughSOCKS(t *testing.T, socksAddr, target string) {
	t.Helper()
	d, err := proxy.SOCKS5("tcp", socksAddr, nil, proxy.Direct)
	if err != nil {
		t.Fatal(err)
	}
	cd, ok := d.(proxy.ContextDialer)
	if !ok {
		t.Fatal("SOCKS5 dialer has no DialContext")
	}
	hc := &http.Client{Transport: &http.Transport{DialContext: cd.DialContext}, Timeout: requestExpiry}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+target+"/", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := hc.Do(req)
	if err != nil {
		t.Logf("request through REALITY failed as expected: %v", err)
		return
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	t.Logf("request through REALITY: %s", resp.Status)
}

// TestLiveRealityDoesNotCrashXray (D13, 09 §1.4): a REALITY outbound that
// receives a real certificate makes Xray run its spider; with SpiderY filled
// by scd the spider finishes and Xray keeps running (with SpiderX/SpiderY
// zeroed it panics with "index out of range [8] with length 0").
func TestLiveRealityDoesNotCrashXray(t *testing.T) {
	if os.Getenv(envXrayBinary) == "" {
		t.Skip(envXrayBinary + " not set")
	}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("real site"))
	}))
	t.Cleanup(srv.Close)
	srvAddr := srv.Listener.Addr().String()

	p := launchXray(t, "SSL_CERT_FILE="+writeCertPEM(t, srv))
	c := p.client
	ctx := t.Context()

	h := realityOutbound(t, realityLink(t, srvAddr))
	if err := c.AddOutbound(ctx, h); err != nil {
		t.Fatalf("AddOutbound: %v", err)
	}
	tun := topology.Tunnel{
		InboundTag: "in-x",
		RouteTag:   "route-x",
		Listen:     "127.0.0.1",
		Port:       freePort(t),
	}
	in, err := resources.Inbound(tun)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.AddInbound(ctx, in); err != nil {
		t.Fatalf("AddInbound: %v", err)
	}
	if err := c.AddRule(ctx, resources.Rule(tun, h.Tag)); err != nil {
		t.Fatalf("AddRule: %v", err)
	}

	requestThroughSOCKS(t, net.JoinHostPort(tun.Listen, strconv.Itoa(tun.Port)), srvAddr)

	deadline := time.Now().Add(realCertWait)
	for !strings.Contains(p.log.String(), realCertLog) {
		if p.exited() || time.Now().After(deadline) {
			t.Fatalf("no %q in the Xray log (exited: %v):\n%s", realCertLog, p.exited(), p.log.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
	time.Sleep(aliveAfter)
	if p.exited() {
		t.Fatalf("xray exited %s after the REALITY spider started: %v\n%s", aliveAfter, p.waitErr, p.log.String())
	}
	if strings.Contains(p.log.String(), "panic:") {
		t.Fatalf("xray panicked:\n%s", p.log.String())
	}
	if _, err := c.ListOutbounds(ctx); err != nil {
		t.Fatalf("ListOutbounds after the spider: %v", err)
	}
}
