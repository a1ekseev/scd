package warp

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/a1ekseev/scd/internal/common/candidate"
	"github.com/a1ekseev/scd/internal/wgp/config"
	"github.com/a1ekseev/scd/internal/wgp/output"
	"github.com/a1ekseev/scd/internal/wgp/provider"
)

const dev1 = "00000000-0000-4000-8000-000000000001"

// masqueTest is newTest with tunnel masque, transports h3 and h2.
func masqueTest(f *fakeAPI, c *clock, n int, mutate func(*WARP)) *WARP {
	return newTest(f, c, n, func(w *WARP) {
		w.Tunnel, w.WireGuard = tunnelMasque, WireGuard{}
		w.Masque = Masque{Transports: []string{"h3", "h2"}, SNI: "consumer-masque.cloudflareclient.com", Port: 443, DNS: cloudflareDNS}
		if mutate != nil {
			mutate(w)
		}
	})
}

// parseMasque checks every line with the scd parser and returns them.
func parseMasque(t *testing.T, lines []string) []candidate.Masque {
	t.Helper()
	var out []candidate.Masque
	for _, l := range lines {
		c, err := candidate.ParseMasque(l)
		if err != nil {
			t.Fatalf("ParseMasque: %v", err)
		}
		out = append(out, c)
	}
	return out
}

// devPKIX is the base64 PKIX of the device key of c, as the PATCH body sends it.
func devPKIX(t *testing.T, c candidate.Masque) string {
	t.Helper()
	b, err := x509.MarshalPKIXPublicKey(&c.PrivateKey.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(b)
}

func TestMasque(t *testing.T) {
	f := newFake(t)
	c := &clock{time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)}
	s := &memStore{}
	w := masqueTest(f, c, 1, nil)
	if w.Kind() != candidate.KindMasque {
		t.Fatalf("kind %q", w.Kind())
	}
	res := tick(t, w, s)
	// Registered with WireGuard, then switched to MASQUE at once.
	if got := f.takeCalls(); !slices.Equal(got, []string{"POST /reg", "PATCH"}) {
		t.Fatalf("calls %v", got)
	}
	var body map[string]string
	if err := json.Unmarshal(f.bodies[0], &body); err != nil || body["key_type"] != "curve25519" || body["tunnel_type"] != "wireguard" {
		t.Fatalf("POST /reg body %s", f.bodies[0])
	}
	var patch map[string]string
	if err := json.Unmarshal([]byte(f.patches[0]), &patch); err != nil || patch["key_type"] != "secp256r1" || patch["tunnel_type"] != "masque" || len(patch) != 3 {
		t.Fatalf("PATCH body %s", f.patches[0])
	}
	if k, err := base64.StdEncoding.DecodeString(patch["key"]); err != nil {
		t.Fatal(err)
	} else if pk, err := x509.ParsePKIXPublicKey(k); err != nil || pk.(*ecdsa.PublicKey).Curve != elliptic.P256() {
		t.Fatalf("PATCH key: %v", err)
	}

	got := parseMasque(t, res.Lines)
	if len(got) != 2 || res.Status.Issued != 2 || res.Status.State != "ok" {
		t.Fatalf("result %+v", res)
	}
	peer, _ := pem.Decode([]byte(f.peerPEM))
	for i, g := range got {
		if g.Transport != []string{"h3", "h2"}[i] || g.Priority != i || g.Provider != Type || g.Label != "WARP" {
			t.Errorf("line %d: %s", i, g)
		}
		if devPKIX(t, g) != f.devices[dev1].key {
			t.Errorf("line %d: device key differs from the PATCH", i)
		}
		if pub, _ := x509.MarshalPKIXPublicKey(g.PeerKey); string(pub) != string(peer.Bytes) {
			t.Errorf("line %d: peerkey differs from the API", i)
		}
	}
	want := "@162.159.198.2:443?provider=warp&peerkey=" + base64.RawURLEncoding.EncodeToString(peer.Bytes) +
		"&address=172.16.0.2/32,2606:4700:110:8a36::2/128&sni=consumer-masque.cloudflareclient.com&transport=h3" +
		"&dns=1.1.1.1,1.0.0.1,2606:4700:4700::1111,2606:4700:4700::1001&priority=0#WARP"
	if !strings.HasSuffix(res.Lines[0], want) {
		t.Errorf("line %s, want suffix %s", got[0], want)
	}
	// The lines of one key differ only in transport and priority.
	if h2 := strings.Replace(strings.Replace(res.Lines[0], "transport=h3", "transport=h2", 1), "priority=0", "priority=1", 1); h2 != res.Lines[1] {
		t.Errorf("h2 line differs beyond transport and priority:\n%s\n%s", got[0], got[1])
	}

	// The next tick (and a restart) only checks.
	for _, w := range []*WARP{w, masqueTest(f, c, 1, nil)} {
		again := tick(t, w, s)
		if calls := f.takeCalls(); !slices.Equal(calls, []string{"GET"}) || !slices.Equal(again.Lines, res.Lines) {
			t.Fatalf("calls %v, lines changed %v", calls, !slices.Equal(again.Lines, res.Lines))
		}
	}

	// rotateOnStart: a new P-256 key, once per process.
	rot := masqueTest(f, c, 1, func(w *WARP) { w.Keys.RotateOnStart = true })
	rotated := parseMasque(t, tick(t, rot, s).Lines)
	if calls := f.takeCalls(); !slices.Equal(calls, []string{"GET", "PATCH"}) || devPKIX(t, rotated[0]) == devPKIX(t, got[0]) ||
		devPKIX(t, rotated[0]) != f.devices[dev1].key || devPKIX(t, rotated[1]) != devPKIX(t, rotated[0]) {
		t.Fatalf("calls %v; the key must change on both lines", calls)
	}
	tick(t, rot, s)
	if calls := f.takeCalls(); !slices.Equal(calls, []string{"GET"}) {
		t.Fatalf("calls %v", calls)
	}

	// The device was set back to WireGuard elsewhere: the GET answer for a
	// key that is not ours keeps the profile; the next rotation sets MASQUE
	// again.
	f.with(func() {
		d := f.devices[dev1]
		d.key, d.tunnel = base64.StdEncoding.EncodeToString(make([]byte, 32)), "wireguard"
	})
	if l := tick(t, masqueTest(f, c, 1, nil), s).Lines; len(l) != 2 || !strings.Contains(l[0], "@162.159.198.2:443?") {
		t.Fatalf("lines %d", len(l))
	}
	back := parseMasque(t, tick(t, masqueTest(f, c, 1, func(w *WARP) { w.Keys.RotateOnStart = true }), s).Lines)
	if d := f.devices[dev1]; d.tunnel != "masque" || devPKIX(t, back[0]) != d.key {
		t.Fatal("not switched back to MASQUE")
	}
}

// A failed PATCH after the registration leaves a device without a MASQUE
// key: no lines, and the next tick retries the PATCH, not the registration.
func TestMasquePatchRetry(t *testing.T) {
	f := newFake(t)
	c := &clock{time.Now()}
	s := &memStore{}
	f.patch = []int{500}
	_, err := masqueTest(f, c, 1, nil).Tick(t.Context(), s)
	if err == nil || err.Error() != "device 1: rotate: status 500; no MASQUE key yet" {
		t.Fatalf("err %v", err)
	}
	if calls := f.takeCalls(); !slices.Equal(calls, []string{"POST /reg", "PATCH"}) {
		t.Fatalf("calls %v", calls)
	}
	res := tick(t, masqueTest(f, c, 1, nil), s)
	if calls := f.takeCalls(); !slices.Equal(calls, []string{"GET", "PATCH"}) || len(res.Lines) != 2 {
		t.Fatalf("calls %v, lines %d", calls, len(res.Lines))
	}

	// An endpoint key the scd parser would refuse: no lines, the file stays.
	f.with(func() { f.peerPEM = "not a key" })
	_, err = masqueTest(f, c, 1, func(w *WARP) { w.Keys.RotateOnStart = true }).Tick(t.Context(), s)
	if err == nil || err.Error() != "device 1: peer public_key: must be a PEM PKIX ECDSA P-256 key" {
		t.Fatalf("err %v", err)
	}
}

func TestMasqueBadAnswer(t *testing.T) {
	ec, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, _ := x509.MarshalPKIXPublicKey(&ec.PublicKey)
	p384 := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
	f := &fakeAPI{}
	w := masqueTest(f, &clock{}, 1, nil)
	ok := profile{V4: "172.16.0.2", PeerKey: dummyPEM, EndpointV4: "162.159.198.2:0"}
	cases := []struct {
		name string
		mut  func(*profile)
		want string
	}{
		{"P-384 peer", func(p *profile) { p.PeerKey = p384 }, "peer public_key: must be a PEM PKIX ECDSA P-256 key"},
		{"base64 peer", func(p *profile) { p.PeerKey = dummyKey }, "peer public_key"},
		{"v6 endpoint", func(p *profile) { p.EndpointV4 = "[2606:4700:103::2]:0" }, "peer endpoint v4: must be IPv4:port"},
		{"no endpoint", func(p *profile) { p.EndpointV4 = "" }, "peer endpoint v4"},
		{"bad address", func(p *profile) { p.V4 = "x" }, "interface addresses"},
		{"sni IP not the endpoint", func(p *profile) { p.EndpointV4 = "162.159.198.1:0" }, "sni: an IP must be the endpoint IP"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := ok
			tc.mut(&p)
			w := w
			if strings.HasPrefix(tc.name, "sni") {
				w = masqueTest(f, &clock{}, 1, func(w *WARP) { w.Masque.SNI = "162.159.198.2" })
			}
			if _, err := w.masqueLines(p, dummyECKey); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err %v, want %q", err, tc.want)
			}
		})
	}
	// An IP sni equal to the endpoint IP is accepted.
	w = masqueTest(f, &clock{}, 1, func(w *WARP) { w.Masque.SNI = "162.159.198.2"; w.Masque.Transports = []string{"h2"} })
	if l, err := w.masqueLines(ok, dummyECKey); err != nil || len(l) != 1 || !strings.Contains(l[0], "sni=162.159.198.2&transport=h2") {
		t.Fatalf("lines %d, err %v", len(l), err)
	}
}

// TestMasqueNoSecrets checks that no token or MASQUE key reaches warnings,
// status, errors or print-config, and that the output kind is checked.
func TestMasqueNoSecrets(t *testing.T) {
	f := newFake(t)
	c := &clock{time.Now()}
	s := &memStore{}
	var seen []string
	run := func(w *WARP) {
		res, err := w.Tick(t.Context(), s)
		b, _ := json.Marshal(res.Status)
		p, _ := json.Marshal(w)
		seen = append(seen, string(b), string(p), fmt.Sprint(err))
		seen = append(seen, res.Warnings...)
	}
	rot := func(w *WARP) { w.Keys.RotateOnStart = true; w.DeviceLostAfterChecks = 1 }
	run(masqueTest(f, c, 2, rot))
	run(masqueTest(f, c, 2, rot))
	f.with(func() { f.peerPEM = "bad" })
	run(masqueTest(f, c, 2, rot))
	f.with(func() { f.get = []int{404} })
	run(masqueTest(f, c, 2, nil))

	var rec record
	if err := json.Unmarshal(s.b, &rec); err != nil {
		t.Fatal(err)
	}
	var secrets []string
	for i := range f.seq {
		secrets = append(secrets, fmt.Sprintf("secret-token-%d", i+1))
	}
	for _, d := range rec.Devices {
		if d.MasqueKey != nil {
			secrets = append(secrets, base64.StdEncoding.EncodeToString(d.MasqueKey), base64.RawURLEncoding.EncodeToString(d.MasqueKey))
		}
	}
	if len(secrets) < 4 {
		t.Fatalf("too few secrets: %d", len(secrets))
	}
	all := strings.Join(seen, "\n")
	for _, sec := range secrets {
		if strings.Contains(all, sec) {
			t.Fatalf("secret in output:\n%s", all)
		}
	}
	if !strings.Contains(all, "peer public_key") {
		t.Fatalf("errors missing:\n%s", all)
	}
}

// The output of a masque provider gets its Kind: wireguard lines are not
// accepted there, nor masque lines in a wireguard output.
func TestMasqueKind(t *testing.T) {
	f := newFake(t)
	lines := tick(t, masqueTest(f, &clock{time.Now()}, 1, nil), &memStore{}).Lines
	wg := tick(t, newTest(f, &clock{time.Now()}, 1, nil), &memStore{}).Lines
	dir := t.TempDir()
	w := masqueTest(f, &clock{}, 1, nil)
	if _, err := output.Publish(output.Output{ID: "warpmq", Path: filepath.Join(dir, "mq"), Kind: w.Kind()}, lines); err != nil {
		t.Fatal(err)
	}
	if _, err := output.Publish(output.Output{ID: "warpmq", Path: filepath.Join(dir, "mq"), Kind: w.Kind()}, wg); err == nil {
		t.Fatal("wireguard line in a masque output")
	}
	if _, err := output.Publish(output.Output{ID: "warp", Path: filepath.Join(dir, "wg"), Kind: candidate.KindWireGuard}, lines); err == nil {
		t.Fatal("masque line in a wireguard output")
	}
}

func TestMasqueConfig(t *testing.T) {
	const head = `server: {listen: 127.0.0.1:8080}
state: {dir: /app/state, keyFile: /run/secrets/key}
deviceNamePrefix: wgp-a-
outputs: [{id: warpmq, path: /app/out/warpmq, provider: warpmq, candidates: 1}]
identification: {warp: {userAgent: ua, clientVersion: cv, apiVersion: v0a5641, model: PC, locale: en_US, osVersion: "16"}}
providers:
  - {id: warpmq, type: warp, schedule: "@hourly", tunnel: masque, keys: {rotateOnStart: true}, reRegisterMinIntervalMinutes: 60, label: WARP`
	const mq = ", masque: {transports: [h3, h2], sni: consumer-masque.cloudflareclient.com"
	cases := []struct{ name, cfg, want string }{
		{name: "ok", cfg: mq + "}}"},
		{name: "sni endpoint IP", cfg: ", masque: {transports: [h2], sni: 162.159.198.2}}"},
		{name: "no masque", cfg: "}", want: "providers.0.masque: required"},
		{name: "wireguard keys", cfg: mq + "}, wireguard: {mtu: 1280}}", want: "providers.0.wireguard: not allowed with tunnel masque"},
		{name: "no transports", cfg: ", masque: {sni: x.example}}", want: "masque.transports: required"},
		{name: "empty transports", cfg: ", masque: {transports: [], sni: x.example}}", want: "masque.transports: must not be empty"},
		{name: "bad transport", cfg: ", masque: {transports: [h1], sni: x.example}}", want: "masque.transports.0: must be h3 or h2"},
		{name: "duplicate transport", cfg: ", masque: {transports: [h3, h3], sni: x.example}}", want: `masque.transports.1: duplicate transport "h3"`},
		{name: "no sni", cfg: ", masque: {transports: [h3]}}", want: "masque.sni: required"},
		{name: "bad sni", cfg: ", masque: {transports: [h3], sni: \"a b\"}}", want: "sni: must be a hostname or the endpoint IP"},
		{name: "v6 sni", cfg: ", masque: {transports: [h3], sni: \"2606:4700:103::2\"}}", want: "sni: an IP must be the endpoint IP"},
		{name: "bad port", cfg: mq + ", port: 0}}", want: "masque.port: must be at least 1"},
		{name: "dns domain", cfg: mq + ", dns: [one.one.one.one]}}", want: "masque: dns: each must be an IP address"},
		{name: "unknown key", cfg: mq + ", mtu: 1280}}", want: `unknown key "mtu"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "config.yml")
			if err := os.WriteFile(p, []byte(head+tc.cfg+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			l, err := config.Load(p)
			if tc.want != "" {
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("err %v, want %q", err, tc.want)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			pc, _ := l.Config.Provider("warpmq")
			w := pc.Impl.(*WARP)
			if w.Kind() != candidate.KindMasque || w.Masque.Port != 443 || len(w.Masque.DNS) != 4 || w.WireGuard.MTU != 0 {
				t.Fatalf("defaults %+v", w)
			}
			// print-config shows the masque subtree only.
			b, _ := json.Marshal(w)
			if strings.Contains(string(b), `"wireguard"`) || !strings.Contains(string(b), `"masque":{"transports":[`) {
				t.Fatalf("print-config %s", b)
			}
		})
	}
}

var _ provider.Provider = (*WARP)(nil)
