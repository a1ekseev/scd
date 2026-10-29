package proton

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha512"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/a1ekseev/scd/internal/common/candidate"
	"github.com/a1ekseev/scd/internal/common/wgpstatus"
	"github.com/a1ekseev/scd/internal/wgp/config"
	"github.com/a1ekseev/scd/internal/wgp/provider"
	"github.com/a1ekseev/scd/internal/wgp/transport"
)

// The OpenVPN credentials of the fake account: never read or shown.
const (
	fakeOVPNName = "ovpn-name-Secret-51"
	fakeOVPNPass = "ovpn-pass-Secret-52"
)

// fakeCert is a certificate of the fake account.
type fakeCert struct {
	Serial, DeviceName, Mode, Key string
	// Ghost is listed, but its revocation finds nothing (Count 0).
	Ghost bool
}

// vpn answers the VPN routes of a tick. f.mu is held.
func (f *fakeAPI) vpn(w http.ResponseWriter, r *http.Request, route string, body map[string]any) {
	switch route {
	case "GET /vpn/v2":
		f.reply(w, 200, map[string]any{"Code": 1000, "VPN": map[string]any{
			"PlanName": "free", "MaxTier": f.maxTier, "MaxConnect": f.maxConnect, "Name": fakeOVPNName, "Password": fakeOVPNPass,
		}})
	case "GET /vpn/v1/logicals":
		f.reply(w, 200, map[string]any{"Code": 1000, "LogicalServers": f.logicals})
	case "POST /vpn/v1/certificate":
		f.certificate(w, body)
	case "GET /vpn/v1/certificate/all":
		q := r.URL.Query()
		limit, _ := strconv.Atoi(q.Get("Limit"))
		if q.Get("Mode") != "persistent" || limit != 50 {
			f.fail(w, 422, 2001)
			return
		}
		list, begun := []map[string]any{}, q.Get("BeginID") == ""
		for _, c := range f.certs {
			if begun && c.Mode == "persistent" && len(list) < limit {
				list = append(list, map[string]any{"SerialNumber": c.Serial, "DeviceName": c.DeviceName, "Mode": c.Mode, "ClientKey": c.Key})
			}
			begun = begun || c.Serial == q.Get("BeginID")
		}
		f.reply(w, 200, map[string]any{"Code": 1000, "Certificates": list, "Total": len(f.certs)})
	case "DELETE /vpn/v1/certificate":
		n := 0
		f.certs = slices.DeleteFunc(f.certs, func(c fakeCert) bool {
			if c.Serial == body["SerialNumber"] {
				if !c.Ghost {
					n++
				}
				return true
			}
			return false
		})
		f.reply(w, 200, map[string]any{"Code": 1000, "Count": n})
	default:
		f.reply(w, 200, map[string]any{"Code": 1000})
	}
}

// certificate issues a session or persistent certificate. A renewal
// replaces the certificate of the same DeviceName. f.mu is held.
func (f *fakeAPI) certificate(w http.ResponseWriter, body map[string]any) {
	key, _ := body["ClientPublicKey"].(string)
	blk, _ := pem.Decode([]byte(key))
	var ed ed25519.PublicKey
	if blk != nil {
		k, _ := x509.ParsePKIXPublicKey(blk.Bytes)
		ed, _ = k.(ed25519.PublicKey)
	}
	minutes, err := strconv.Atoi(strings.TrimSuffix(fmt.Sprint(body["Duration"]), " min"))
	if f.failCert || ed == nil || err != nil {
		f.fail(w, 422, 2001)
		return
	}
	mode, _ := body["Mode"].(string)
	name, _ := body["DeviceName"].(string)
	f.serials++
	serial := fmt.Sprintf("serial-%d", f.serials)
	if mode == "" {
		serial = fmt.Sprintf("session-serial-%d", f.serials)
	}
	if body["Renew"] == true {
		f.certs = slices.DeleteFunc(f.certs, func(c fakeCert) bool { return c.DeviceName == name && c.Mode == mode })
	}
	f.certs = append(f.certs, fakeCert{Serial: serial, DeviceName: name, Mode: mode, Key: key})
	fp := sha512.Sum512(montgomery(ed))
	now, d := f.now(), time.Duration(minutes)*time.Minute
	f.reply(w, 200, map[string]any{
		"Code": 1000, "SerialNumber": serial, "ClientKeyFingerprint": base64.StdEncoding.EncodeToString(fp[:]),
		"ExpirationTime": now.Add(d).Unix(), "RefreshTime": now.Add(d * 3 / 4).Unix(), "Mode": mode, "DeviceName": name,
	})
}

// montgomery maps an Ed25519 public key to its X25519 public key,
// u = (1+y)/(1-y) mod p: the fake checks keys without the derivation of the
// provider.
func montgomery(pub ed25519.PublicKey) []byte {
	p := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 255), big.NewInt(19))
	yb := slices.Clone(pub)
	yb[31] &= 0x7f
	slices.Reverse(yb)
	y := new(big.Int).SetBytes(yb)
	den := new(big.Int).Sub(big.NewInt(1), y)
	den.ModInverse(den.Mod(den, p), p)
	u := new(big.Int).Add(big.NewInt(1), y)
	u.Mul(u, den).Mod(u, p)
	ub := u.FillBytes(make([]byte, 32))
	slices.Reverse(ub)
	return ub
}

// clock is a settable test clock.
type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

// signedIn returns a state record with a valid session of f.
func signedIn(t *testing.T, f *fakeAPI) *memStore {
	t.Helper()
	f.mu.Lock()
	f.uid = "fake-uid-1"
	f.newTokens()
	tk := tokens{UID: f.uid, AccessToken: f.access, RefreshToken: f.refresh}
	f.mu.Unlock()
	b, _ := json.Marshal(map[string]any{sessionKey: tk})
	return &memStore{b: b}
}

// newTick returns a provider with n candidates as Factory.New would, on the
// clock c.
func newTick(t *testing.T, cfg transport.Config, c *clock, n int, mutate func(*Proton)) *Proton {
	t.Helper()
	p := newTestProton(t, cfg)
	p.out.Candidates, p.now, p.revokeAfter = n, c.now, 30*time.Minute
	s := &p.settings
	s.Tier, s.Keys.RotateOnStart = "free", true
	s.Certificate.DurationMinutes, s.Certificate.RenewBeforeMinutes = 1440, 360
	s.Certificate.Features.VPNAccelerator = true
	s.Servers.ExitCountries, s.Servers.EntryCountries, s.Servers.Cities = []string{}, []string{}, []string{}
	s.Servers.RequireFeatures, s.Servers.ExcludeFeatures = []string{}, []string{"secureCore", "tor"}
	s.Servers.MaxLoad, s.Servers.MaxScore, s.Servers.SwitchMarginPercent, s.Servers.SwitchConfirmChecks = 90, 999, 20, 2
	s.WireGuard.Port, s.WireGuard.DNS, s.WireGuard.MTU = 51820, []string{"10.2.0.1"}, 1420
	s.Label = defaultLabel
	if mutate != nil {
		mutate(p)
	}
	p.rotate = s.Keys.RotateOnStart
	return p
}

// phys is a physical server whose key is derived from its id.
func phys(id, ip string, status int) map[string]any {
	k := sha512.Sum512([]byte("peer " + id))
	return map[string]any{
		"ID": id, "EntryIP": ip, "ExitIP": ip, "Status": status, "Label": "0", "Domain": "node.example",
		"X25519PublicKey": base64.StdEncoding.EncodeToString(k[:32]), "Generation": 0, "ServicesDown": 0,
	}
}

// logical is a logical server of the fake list; p gives its physical
// servers (one online by default).
func srv(id, name, city, exit string, tier, features, load int, score float64, p ...map[string]any) map[string]any {
	if p == nil {
		p = []map[string]any{phys(id+"-p", "192.0.2."+strconv.Itoa(len(id)+10), 1)}
	}
	return map[string]any{
		"ID": id, "Name": name, "City": city, "EntryCountry": exit, "ExitCountry": exit, "Tier": tier,
		"Features": features, "Status": 1, "Load": load, "Score": score, "Servers": p, "Domain": "node.example",
	}
}

func tickOK(t *testing.T, p *Proton, s provider.Store) provider.Result {
	t.Helper()
	res, err := p.Tick(t.Context(), s)
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	return res
}

func parse(t *testing.T, lines []string) []candidate.WireGuard {
	t.Helper()
	var out []candidate.WireGuard
	for _, l := range lines {
		c, err := candidate.ParseWireGuard(l)
		if err != nil {
			t.Fatalf("line: %v", err)
		}
		out = append(out, c)
	}
	return out
}

func servers(t *testing.T, lines []string) []string {
	t.Helper()
	var names []string
	for _, c := range parse(t, lines) {
		names = append(names, c.Meta.Server)
	}
	return names
}

// testList has one server per filter reason.
func testList() []map[string]any {
	offline := srv("off", "NL#9", "Amsterdam", "NL", 0, 0, 10, 1)
	offline["Status"] = 0
	sc := srv("sc", "CH-NL#1", "Amsterdam", "NL", 0, featSecureCore, 10, 2)
	sc["EntryCountry"] = "CH"
	smart := srv("smart", "TR#1", "Istanbul", "TR", 0, 0, 10, 3)
	smart["HostCountry"] = "RO"
	return []map[string]any{
		srv("nl1", "NL-FREE#1", "Amsterdam", "NL", 0, 0, 30, 5),
		srv("nl2", "NL-FREE#2", "Rotterdam", "NL", 0, featIPv6, 20, 4),
		srv("jp1", "JP-FREE#1", "Tokyo", "JP", 0, 0, 10, 6),
		srv("plus", "NL#1", "Amsterdam", "NL", 2, 0, 10, 1),
		srv("pm", "NL#2", "Amsterdam", "NL", 3, 0, 10, 1),
		srv("busy", "NL-FREE#3", "Amsterdam", "NL", 0, 0, 95, 1),
		srv("auto", "NL-FREE#4", "Amsterdam", "NL", 0, 0, 10, 1000),
		srv("tor", "NL-FREE#5", "Amsterdam", "NL", 0, 2, 10, 1),
		srv("down", "NL-FREE#6", "Amsterdam", "NL", 0, 0, 10, 1, phys("down-p", "192.0.2.99", 0)),
		offline, sc, smart,
	}
}

func TestServerFilters(t *testing.T) {
	for _, tc := range []struct {
		name    string
		maxTier int
		mutate  func(*Proton)
		want    []string
	}{
		{"defaults", 0, nil, []string{"TR#1", "NL-FREE#2", "NL-FREE#1", "JP-FREE#1"}},
		{"plus account on free", 2, nil, []string{"TR#1", "NL-FREE#2", "NL-FREE#1", "JP-FREE#1"}},
		{"plus", 2, func(p *Proton) { p.Tier = "plus" }, []string{"NL#1"}},
		{"exit", 0, func(p *Proton) { p.Servers.ExitCountries = []string{"JP"} }, []string{"JP-FREE#1"}},
		{"smart routing entry", 0, func(p *Proton) { p.Servers.EntryCountries = []string{"RO"} }, []string{"TR#1"}},
		{"secure core entry", 0, func(p *Proton) {
			p.Servers.EntryCountries, p.Servers.ExcludeFeatures = []string{"CH"}, []string{"tor"}
		}, []string{"CH-NL#1"}},
		{"city", 0, func(p *Proton) { p.Servers.Cities = []string{"Rotterdam", "Tokyo"} }, []string{"NL-FREE#2", "JP-FREE#1"}},
		{"require ipv6", 0, func(p *Proton) { p.Servers.RequireFeatures = []string{"ipv6"} }, []string{"NL-FREE#2"}},
		{"max load", 0, func(p *Proton) { p.Servers.MaxLoad = 20 }, []string{"TR#1", "NL-FREE#2", "JP-FREE#1"}},
		{"max score", 0, func(p *Proton) { p.Servers.MaxScore = 4 }, []string{"TR#1", "NL-FREE#2"}},
		{"score 1000 even at maxLoad 100", 0, func(p *Proton) { p.Servers.MaxLoad = 100 }, []string{"NL-FREE#3", "TR#1", "NL-FREE#2", "NL-FREE#1", "JP-FREE#1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, cfg := newFake(t)
			f.maxTier, f.logicals = tc.maxTier, testList()
			p := newTick(t, cfg, &clock{time.Now()}, 10, tc.mutate)
			res, _ := p.Tick(t.Context(), signedIn(t, f))
			if got := servers(t, res.Lines); !slices.Equal(got, tc.want) {
				t.Fatalf("servers %q, want %q", got, tc.want)
			}
		})
	}
}

// A tier above the plan is a provider error; nothing is issued.
func TestTierAboveThePlan(t *testing.T) {
	f, cfg := newFake(t)
	f.logicals = testList()
	p := newTick(t, cfg, &clock{time.Now()}, 2, func(p *Proton) { p.Tier = "plus" })
	res, err := p.Tick(t.Context(), signedIn(t, f))
	if err == nil || !strings.Contains(err.Error(), "tier plus needs MaxTier 1, the account has MaxTier 0") || res.Lines != nil {
		t.Fatalf("%v %q", err, res.Lines)
	}
	if slices.Contains(paths(f.requests()), "POST /vpn/v1/certificate") {
		t.Error("certificates issued")
	}
}

// The physical server is the online one with the smallest ID, whatever the
// list order; one candidate per logical server.
func TestPhysicalServer(t *testing.T) {
	f, cfg := newFake(t)
	f.logicals = []map[string]any{srv("a", "CH#1", "Zurich", "CH", 0, 0, 10, 1,
		phys("p3", "192.0.2.3", 1), phys("p1", "192.0.2.1", 0), phys("p2", "192.0.2.2", 1))}
	st := signedIn(t, f)
	p := newTick(t, cfg, &clock{time.Now()}, 2, nil)
	// One logical server: the second candidate has none.
	if res := tickOK(t, p, st); res.Status.State != wgpstatus.StateError || !strings.Contains(res.Status.Error, "candidate 2: no free server matches the filters") {
		t.Fatalf("status %+v", res.Status)
	}
	f.mu.Lock()
	slices.Reverse(f.logicals[0]["Servers"].([]map[string]any))
	f.logicals = append(f.logicals, srv("b", "CH#2", "Zurich", "CH", 0, 0, 10, 2))
	f.mu.Unlock()
	for range 2 {
		c := parse(t, tickOK(t, p, st).Lines)
		want := phys("p2", "", 0)["X25519PublicKey"]
		if len(c) != 2 || c[0].Endpoint != "192.0.2.2:51820" || base64.StdEncoding.EncodeToString(c[0].PublicKey[:]) != want || c[1].Meta.Server != "CH#2" {
			t.Fatalf("lines %v", c)
		}
	}
}

// A healthy server is left only for one better by switchMarginPercent on
// switchConfirmChecks ticks in a row; a server that leaves the set is left
// at once.
func TestServerHysteresis(t *testing.T) {
	f, cfg := newFake(t)
	set := func(scores ...float64) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.logicals = nil
		for i, sc := range scores {
			f.logicals = append(f.logicals, srv(fmt.Sprint("s", i), fmt.Sprintf("CH#%d", i), "Zurich", "CH", 0, 0, 10, sc))
		}
	}
	st := signedIn(t, f)
	p := newTick(t, cfg, &clock{time.Now()}, 1, nil)
	step := func(want string, scores ...float64) {
		t.Helper()
		set(scores...)
		if got := servers(t, tickOK(t, p, st).Lines); !slices.Equal(got, []string{want}) {
			t.Fatalf("scores %v: %q, want %s", scores, got, want)
		}
	}
	step("CH#0", 10, 20)
	step("CH#0", 10, 8.5) // better, not by 20 %
	step("CH#0", 10, 8)   // 1 of 2
	step("CH#0", 10, 9)   // the series breaks
	step("CH#0", 10, 7)   // 1 of 2
	step("CH#1", 10, 7)   // 2 of 2
	step("CH#1", 1, 7)    // the counter starts again
	f.mu.Lock()
	f.logicals = f.logicals[:1] // CH#1 leaves the set
	f.mu.Unlock()
	if got := servers(t, tickOK(t, p, st).Lines); !slices.Equal(got, []string{"CH#0"}) {
		t.Fatalf("after CH#1 left: %q", got)
	}
}

func certBodies(rs []seen) []map[string]any {
	var out []map[string]any
	for _, r := range rs {
		if r.Method == http.MethodPost && r.Path == "/vpn/v1/certificate" {
			out = append(out, r.Body)
		}
	}
	return out
}

func sorted(a []string, b ...string) []string {
	out := slices.Concat(a, b)
	slices.Sort(out)
	return out
}

func deviceNames(f *fakeAPI) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var names []string
	for _, c := range f.certs {
		names = append(names, c.DeviceName)
	}
	slices.Sort(names)
	return names
}

// Issue, renewal, rotation at start and the revocation of the older
// generation by the exact template only.
func TestCertificateCycle(t *testing.T) {
	f, cfg := newFake(t)
	c := &clock{time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)}
	f.now, f.logicals = c.now, testList()[:3]
	f.certs = []fakeCert{
		{Serial: "foreign-1", DeviceName: "wgp-a-b-proton-1-1", Mode: "persistent"}, // another host's prefix
		{Serial: "neighbour-1", DeviceName: "wgp-a-other-1-1", Mode: "persistent"},  // another output
		{Serial: "loose-1", DeviceName: "wgp-a-proton-1-1-x", Mode: "persistent"},   // not the template
		{Serial: "session-1", DeviceName: "", Mode: "session"},                      // a session certificate
	}
	foreign := []string{"", "wgp-a-b-proton-1-1", "wgp-a-other-1-1", "wgp-a-proton-1-1-x"}
	st := signedIn(t, f)

	// Issue: generation 1, no Renew, the body in its order.
	p := newTick(t, cfg, c, 2, nil)
	res := tickOK(t, p, st)
	bodies := certBodies(f.requests())
	if len(bodies) != 2 || bodies[0]["DeviceName"] != "wgp-a-proton-1-1" || bodies[1]["DeviceName"] != "wgp-a-proton-1-2" {
		t.Fatalf("issued %v", bodies)
	}
	if b := bodies[0]; b["Mode"] != "persistent" || b["Duration"] != "1440 min" || b["Renew"] != nil ||
		fmt.Sprint(b["Features"]) != "map[NetShieldLevel:0 PortForwarding:false RandomNAT:true SplitTCP:true]" {
		t.Errorf("body %v", b)
	}
	if res.Status.Issued != 2 || !res.Status.ExpiresAt.Equal(c.t.Add(24*time.Hour)) || res.Status.Limit != 2 || res.Status.State != "" {
		t.Errorf("status %+v", res.Status)
	}
	first := parse(t, res.Lines)

	// No change before the renewal time.
	mark := len(f.requests())
	c.t = c.t.Add(17 * time.Hour)
	tickOK(t, p, st)
	if b := certBodies(f.requests()[mark:]); b != nil {
		t.Fatalf("early renewal %v", b)
	}
	// RefreshTime (75 % of 24 h) comes before renewBeforeMinutes (18 h).
	c.t = c.t.Add(time.Hour)
	res = tickOK(t, p, st)
	bodies = certBodies(f.requests()[mark:])
	if len(bodies) != 2 || bodies[0]["Renew"] != true || bodies[0]["DeviceName"] != "wgp-a-proton-1-1" {
		t.Fatalf("renewal %v", bodies)
	}
	renewed := parse(t, res.Lines)
	if renewed[0].PrivateKey != first[0].PrivateKey || renewed[0].Meta.Expires.Equal(first[0].Meta.Expires) {
		t.Error("the renewal changed the key or kept the expiry")
	}
	if got := deviceNames(f); !slices.Equal(got, sorted(foreign, "wgp-a-proton-1-1", "wgp-a-proton-1-2")) {
		t.Fatalf("certificates %q", got)
	}

	// Without rotateOnStart a restart keeps the keys.
	mark = len(f.requests())
	q := newTick(t, cfg, c, 2, func(p *Proton) { p.Keys.RotateOnStart = false })
	if res := tickOK(t, q, st); parse(t, res.Lines)[0].PrivateKey != first[0].PrivateKey || certBodies(f.requests()[mark:]) != nil {
		t.Fatal("restart without rotateOnStart changed the keys")
	}

	// A restart with rotateOnStart: generation 2, new keys without Renew.
	mark = len(f.requests())
	p = newTick(t, cfg, c, 2, nil)
	res = tickOK(t, p, st)
	bodies = certBodies(f.requests()[mark:])
	if len(bodies) != 2 || bodies[0]["DeviceName"] != "wgp-a-proton-2-1" || bodies[0]["Renew"] != nil {
		t.Fatalf("rotation %v", bodies)
	}
	rotated := parse(t, res.Lines)
	if rotated[0].PrivateKey == first[0].PrivateKey || rotated[0].Meta.Server != first[0].Meta.Server {
		t.Error("the rotation kept the key or moved the server")
	}
	// The older generation stays until revokeOldAfterMinutes.
	c.t = c.t.Add(29 * time.Minute)
	tickOK(t, p, st)
	if got := deviceNames(f); len(got) != 8 {
		t.Fatalf("revoked too early: %q", got)
	}
	mark = len(f.requests())
	c.t = c.t.Add(time.Minute)
	tickOK(t, p, st)
	if got := deviceNames(f); !slices.Equal(got, sorted(foreign, "wgp-a-proton-2-1", "wgp-a-proton-2-2")) {
		t.Fatalf("after revocation %q", got)
	}
	var revoked []string
	for _, r := range f.requests()[mark:] {
		if r.Method == http.MethodDelete {
			revoked = append(revoked, fmt.Sprint(r.Body["SerialNumber"]))
		}
	}
	if len(revoked) != 2 || slices.ContainsFunc(revoked, func(s string) bool { return !strings.HasPrefix(s, "serial-") }) {
		t.Errorf("revoked %q", revoked)
	}
	// Revoked once: the next tick does not list again.
	mark = len(f.requests())
	tickOK(t, p, st)
	if slices.ContainsFunc(paths(f.requests()[mark:]), func(s string) bool { return strings.HasPrefix(s, "GET /vpn/v1/certificate/all") }) {
		t.Error("listed again after the revocation")
	}

	// Logout revokes the current generation, still by the template only.
	if err := p.Logout(t.Context(), st); err != nil {
		t.Fatal(err)
	}
	if got := deviceNames(f); !slices.Equal(got, foreign) {
		t.Fatalf("after logout %q", got)
	}
	if rec, _ := loadRecord(st); rec[keysKey] != nil || rec[sessionKey] != nil {
		t.Errorf("state after logout %s", st.b)
	}
}

// A failed new key during the rotation keeps the old one published and out
// of the revocation.
func TestRotationFailure(t *testing.T) {
	f, cfg := newFake(t)
	c := &clock{time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)}
	f.now, f.logicals = c.now, testList()[:3]
	st := signedIn(t, f)
	first := parse(t, tickOK(t, newTick(t, cfg, c, 1, nil), st).Lines)
	f.mu.Lock()
	f.failCert = true
	f.mu.Unlock()
	p := newTick(t, cfg, c, 1, nil)
	res := tickOK(t, p, st)
	if got := parse(t, res.Lines); got[0].PrivateKey != first[0].PrivateKey || res.Status.State != wgpstatus.StateError ||
		!strings.Contains(res.Status.Error, "candidate 1: issue: status 422, code 2001") {
		t.Fatalf("status %+v", res.Status)
	}
	c.t = c.t.Add(time.Hour)
	tickOK(t, p, st)
	if got := deviceNames(f); !slices.Equal(got, []string{"wgp-a-proton-1-1"}) {
		t.Fatalf("certificates %q", got)
	}
	f.mu.Lock()
	f.failCert = false
	f.mu.Unlock()
	if got := parse(t, tickOK(t, p, st).Lines); got[0].PrivateKey == first[0].PrivateKey {
		t.Fatal("not rotated after the failure")
	}
}

// needs_login: no rotation, no certificate, the tick fails (the old file
// stays) with the expiry of the current certificates.
func TestTickNeedsLogin(t *testing.T) {
	f, cfg := newFake(t)
	c := &clock{time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)}
	f.now, f.logicals = c.now, testList()[:3]
	st := signedIn(t, f)
	tickOK(t, newTick(t, cfg, c, 1, nil), st)
	before, _ := st.Load()
	f.expire()
	f.mu.Lock()
	f.refreshFail = [2]int{422, codeRefreshInvalid}
	f.mu.Unlock()
	mark := len(f.requests())
	p := newTick(t, cfg, c, 1, nil)
	res, err := p.Tick(t.Context(), st)
	if !errors.Is(err, errNeedsLogin) || res.Status.State != wgpstatus.StateNeedsLogin || res.Status.Issued != 1 ||
		!res.Status.ExpiresAt.Equal(c.t.Add(24*time.Hour)) || res.Lines != nil {
		t.Fatalf("%v %+v", err, res)
	}
	if b, _ := st.Load(); !bytes.Equal(b, before) || !p.rotate || certBodies(f.requests()[mark:]) != nil {
		t.Error("needs_login changed the keys")
	}
}

// The X25519 key of a seed matches the frozen references, which were made
// with the Edwards to Montgomery map; the fake API checks the same map.
func TestX25519Reference(t *testing.T) {
	b, err := os.ReadFile("testdata/x25519.json")
	if err != nil {
		t.Fatal(err)
	}
	var refs []struct{ Seed, Ed25519PublicKey, X25519PublicKey, ClientKeyFingerprint string }
	if err := json.Unmarshal(b, &refs); err != nil || len(refs) == 0 {
		t.Fatal(err)
	}
	for _, r := range refs {
		seed, _ := hex.DecodeString(r.Seed)
		_, pub := wgKey(seed)
		fp := sha512.Sum512(pub[:])
		ed := ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey)
		if base64.StdEncoding.EncodeToString(ed) != r.Ed25519PublicKey || base64.StdEncoding.EncodeToString(pub[:]) != r.X25519PublicKey ||
			base64.StdEncoding.EncodeToString(fp[:]) != r.ClientKeyFingerprint || !bytes.Equal(montgomery(ed), pub[:]) {
			t.Errorf("seed %s", r.Seed[:8])
		}
	}
}

// The line fields of the profile table: address, IPv6 on an IPv6 server,
// DNS, MTU, allowed IPs, endpoint, peer key, metadata and label.
func TestLineFields(t *testing.T) {
	f, cfg := newFake(t)
	c := &clock{time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)}
	sc := srv("sc", "CH-NL#1", "Amsterdam", "NL", 0, featSecureCore|featIPv6, 12, 2.5)
	sc["EntryCountry"] = "CH"
	f.now, f.logicals = c.now, []map[string]any{srv("v4", "CH-FREE#7", "Zürich", "CH", 0, 0, 30, 3), sc}
	p := newTick(t, cfg, c, 2, func(p *Proton) { p.Servers.ExcludeFeatures = []string{} })
	got := parse(t, tickOK(t, p, signedIn(t, f)).Lines)
	exp := c.t.Add(24 * time.Hour)
	for i, w := range []struct{ server, entry, exit, address, allowed, label, endpoint string }{
		{"CH-NL#1", "CH", "NL", "[10.2.0.2/32 2a07:b944::2:2/128]", "[0.0.0.0/0 ::/0]", "🇳🇱 Amsterdam, Proton CH-NL#1", "192.0.2.12:51820"},
		{"CH-FREE#7", "CH", "CH", "[10.2.0.2/32]", "[0.0.0.0/0]", "🇨🇭 Zürich, Proton CH-FREE#7", "192.0.2.12:51820"},
	} {
		g := got[i]
		if g.Provider != "proton" || g.Meta.Server != w.server || g.Meta.Entry != w.entry || g.Meta.Exit != w.exit ||
			fmt.Sprint(g.Address) != w.address || fmt.Sprint(g.AllowedIPs) != w.allowed || g.Label != w.label ||
			g.Endpoint != w.endpoint || fmt.Sprint(g.DNS) != "[10.2.0.1]" || g.MTU != 1420 || g.Reserved != nil ||
			!g.Meta.Expires.Equal(exp) || g.Meta.Load == nil || g.Meta.Score == nil {
			t.Errorf("line %d: %+v", i, g)
		}
	}
	if *got[0].Meta.Score != 2.5 || *got[1].Meta.Load != 30 {
		t.Errorf("metadata %v %v", *got[0].Meta.Score, *got[1].Meta.Load)
	}

	// Explicit wireguard keys replace the defaults.
	f2, cfg2 := newFake(t)
	f2.logicals = f.logicals
	p = newTick(t, cfg2, c, 1, func(p *Proton) {
		p.Servers.ExcludeFeatures = []string{}
		p.WireGuard.Port, p.WireGuard.DNS, p.WireGuard.MTU, p.WireGuard.AllowedIPs, p.WireGuard.Keepalive = 443, []string{"10.2.0.1", "192.0.2.53"}, 1280, []string{"10.0.0.0/8"}, 25
		p.Label = "{flag} {name}"
	})
	g := parse(t, tickOK(t, p, signedIn(t, f2)).Lines)[0]
	if g.Endpoint != "192.0.2.12:443" || fmt.Sprint(g.DNS) != "[10.2.0.1 192.0.2.53]" || g.MTU != 1280 ||
		fmt.Sprint(g.AllowedIPs) != "[10.0.0.0/8]" || g.Keepalive != 25 || g.Label != "🇳🇱 CH-NL#1" {
		t.Errorf("explicit keys: %+v", g)
	}
}

// The persistent certificate, the list, the revocation and the tick's
// server list go out in the client's byte format: its header order, and
// JSON as json.dumps writes it.
func TestTickRequestBytes(t *testing.T) {
	ref := func(file string) string {
		b, err := os.ReadFile(filepath.Join("testdata/proton-requests", file))
		if err != nil {
			t.Fatal(err)
		}
		return strings.NewReplacer("{{arch}}", "aarch64", "{{uid}}", "fake-uid-0001", "{{access_token}}", "tok",
			"{{new_access_token}}", "tok", "{{netzone}}", "203.0.113.0").Replace(string(b))
	}
	withBody := func(head, line, body string) string {
		head, _, _ = strings.Cut(head, "\r\n\r\n")
		_, head, _ = strings.Cut(head, "\r\n")
		head = strings.Replace(head, "{{len}}", strconv.Itoa(len(body)), 1)
		return line + "\r\n" + head + "\r\n\r\n" + body
	}
	seed := make([]byte, 32)
	der, _ := x509.MarshalPKIXPublicKey(ed25519.NewKeyFromSeed(seed).Public())
	pemKey := strings.ReplaceAll(string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})), "\n", `\n`)
	p := newTick(t, transport.Config{}, &clock{}, 1, func(p *Proton) { p.Certificate.Features.NetShield = 2 })
	cert, _ := p.certificateCall(seed, "wgp-a-proton-3-1", false)
	renew, _ := p.certificateCall(seed, "wgp-a-proton-3-1", true)
	features := `"Features": {"RandomNAT": true, "SplitTCP": true, "PortForwarding": false, "NetShieldLevel": 2}`
	logicals := strings.Replace(strings.Replace(ref("10-logicals.http"), "X-PM-netzone: 203.0.113.0\r\n", "", 1),
		"If-Modified-Since: Thu, 01 Jan 1970 00:00:00 GMT\r\n", "", 1)
	for _, tc := range []struct {
		name string
		c    call
		want string
	}{
		{"certificate", cert, withBody(ref("15-certificate-features.http"), "POST /vpn/v1/certificate HTTP/1.1",
			`{"ClientPublicKey": "`+pemKey+`", "Duration": "1440 min", `+features+`, "Mode": "persistent", "DeviceName": "wgp-a-proton-3-1"}`)},
		{"renew", renew, withBody(ref("15-certificate-features.http"), "POST /vpn/v1/certificate HTTP/1.1",
			`{"ClientPublicKey": "`+pemKey+`", "Duration": "1440 min", `+features+`, "Mode": "persistent", "DeviceName": "wgp-a-proton-3-1", "Renew": true}`)},
		{"revoke", revokeCall("serial-7"), withBody(ref("15-certificate-features.http"), "DELETE /vpn/v1/certificate HTTP/1.1", `{"SerialNumber": "serial-7"}`)},
		{"list", listCall(""), strings.Replace(ref("05-vpn-v2.http"), "GET /vpn/v2 ", "GET /vpn/v1/certificate/all?Mode=persistent&Limit=50 ", 1)},
		{"list page", listCall("serial-50"), strings.Replace(ref("05-vpn-v2.http"), "GET /vpn/v2 ", "GET /vpn/v1/certificate/all?Mode=persistent&Limit=50&BeginID=serial-50 ", 1)},
		{"logicals", logicalsCall(), logicals},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, ch := newTestAPI(t)
			tc.c.uid, tc.c.token = "fake-uid-0001", "tok"
			if got := send(t, a, ch, tc.c).request; string(got) != tc.want {
				t.Errorf("request\n%q\nwant\n%q", got, tc.want)
			}
		})
	}
}

// Keys, seeds, tokens and the OpenVPN credentials never appear in the
// status, the errors, the warnings or the printed config.
func TestTickNoSecrets(t *testing.T) {
	f, cfg := newFake(t)
	c := &clock{time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)}
	f.now, f.logicals = c.now, testList()[:3]
	st := signedIn(t, f)
	p := newTick(t, cfg, c, 3, nil)
	res := tickOK(t, p, st)
	var texts []string
	add := func(v any) {
		b, _ := json.Marshal(v)
		texts = append(texts, string(b), fmt.Sprintf("%v %+v", v, v))
	}
	add(res.Status)
	add(res.Warnings)
	add(p)
	f.mu.Lock()
	f.failCert = true
	f.mu.Unlock()
	c.t = c.t.Add(23 * time.Hour)
	res, err := p.Tick(t.Context(), st)
	add(res.Status)
	texts = append(texts, fmt.Sprint(err))
	f.expire()
	f.mu.Lock()
	f.refreshFail = [2]int{422, codeRefreshInvalid}
	f.mu.Unlock()
	res, err = p.Tick(t.Context(), st)
	add(res.Status)
	texts = append(texts, fmt.Sprint(err))

	ks, _ := p.loadKeys(st)
	secrets := []string{fakeOVPNName, fakeOVPNPass, "tok-access", "tok-refresh"}
	for _, sl := range ks.Slots {
		sk, _ := wgKey(sl.Seed)
		secrets = append(secrets, base64.StdEncoding.EncodeToString(sl.Seed), hex.EncodeToString(sl.Seed), base64.StdEncoding.EncodeToString(sk[:]))
	}
	if len(secrets) != 13 {
		t.Fatalf("%d secrets", len(secrets))
	}
	for _, s := range texts {
		for _, secret := range secrets {
			if strings.Contains(s, secret) {
				t.Errorf("%q leaks %q", s, secret)
			}
		}
	}
	if bytes.Contains(st.b, []byte(fakeOVPNPass)) {
		t.Error("the OpenVPN password is in the state")
	}
}

func TestConfig(t *testing.T) {
	const head = `server: {listen: 127.0.0.1:8080}
state: {dir: /app/state, keyFile: /run/secrets/key}
deviceNamePrefix: wgp-a-
identification: {proton: {appVersion: "linux-vpn-gui@5.8.11+x86-64", userAgent: "ProtonVPN/5.8.11 (Linux; ubuntu/26.04)", timezone: Europe/Istanbul}}
outputs: [{id: proton, path: /app/out/proton, provider: proton, candidates: 3}]
`
	const keys = "keys: {revokeOldAfterMinutes: 30}\n"
	const prov = `providers:
  - id: proton
    type: proton
    schedule: "*/15 * * * *"
    tier: free
    keys: {rotateOnStart: true}
    certificate: {durationMinutes: 1440, renewBeforeMinutes: 360, features: {netShield: 0, vpnAccelerator: true, moderateNat: false}}
    servers: {exitCountries: [NL], excludeFeatures: [secureCore, tor], maxLoad: 80, switchMarginPercent: 20, switchConfirmChecks: 2}
`
	cases := []struct{ name, cfg, want string }{
		{name: "ok", cfg: keys + prov},
		{name: "no revokeOldAfterMinutes", cfg: prov, want: "keys.revokeOldAfterMinutes (top level) is required"},
		{name: "disabled without revokeOldAfterMinutes", cfg: strings.Replace(prov, "tier: free", "enabled: false\n    tier: free", 1)},
		{name: "tier", cfg: keys + strings.Replace(prov, "tier: free", "tier: pro", 1), want: "tier: must be one of free|plus"},
		{name: "no rotateOnStart", cfg: keys + strings.Replace(prov, "rotateOnStart: true", "", 1), want: "keys.rotateOnStart: required"},
		{name: "renew before", cfg: keys + strings.Replace(prov, "renewBeforeMinutes: 360", "renewBeforeMinutes: 1440", 1), want: "renewBeforeMinutes: must be less than durationMinutes"},
		{name: "duration", cfg: keys + strings.Replace(prov, "durationMinutes: 1440", "durationMinutes: 9", 1), want: "durationMinutes: must be at least 10"},
		{name: "netShield", cfg: keys + strings.Replace(prov, "netShield: 0", "netShield: 3", 1), want: "netShield: must be at most 2"},
		{name: "no moderateNat", cfg: keys + strings.Replace(prov, ", moderateNat: false", "", 1), want: "moderateNat: required"},
		{name: "no excludeFeatures", cfg: keys + strings.Replace(prov, "excludeFeatures: [secureCore, tor], ", "", 1), want: "excludeFeatures: required"},
		{name: "bad feature", cfg: keys + strings.Replace(prov, "[secureCore, tor]", "[secureCore, onion]", 1), want: "excludeFeatures.1: must be one of"},
		{name: "country", cfg: keys + strings.Replace(prov, "[NL]", "[nl]", 1), want: "exitCountries.0: must be an upper-case ISO2 code"},
		{name: "maxScore", cfg: keys + strings.Replace(prov, "maxLoad: 80", "maxLoad: 80, maxScore: 1000", 1), want: "maxScore: must be at most 999"},
		{name: "port", cfg: keys + prov + "    wireguard: {port: 2408}\n", want: "port: must be one of 443|88|1224|51820|500|4500"},
		{name: "dns", cfg: keys + prov + "    wireguard: {dns: [dns.example]}\n", want: "dns.0: must be an IP address"},
		{name: "allowedIPs", cfg: keys + prov + "    wireguard: {allowedIPs: [10.0.0.1]}\n", want: "allowedIPs.0: must be a CIDR prefix"},
		{name: "label", cfg: keys + prov + "    label: \"{city} {flag}\"\n", want: "label: must start with {flag}"},
		{name: "unknown key", cfg: keys + prov + "    candidates: 2\n", want: "candidates"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yml")
			if err := os.WriteFile(path, []byte(head+tc.cfg), 0o600); err != nil {
				t.Fatal(err)
			}
			l, err := config.Load(path)
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				pc, _ := l.Config.Provider("proton")
				p := pc.Impl.(*Proton)
				if p.WireGuard.Port != 51820 || p.WireGuard.MTU != 1420 || fmt.Sprint(p.WireGuard.DNS) != "[10.2.0.1]" ||
					p.Servers.MaxScore != 999 || p.Label != defaultLabel || p.out != (provider.Output{ID: "proton", Candidates: 3}) ||
					p.prefix != "wgp-a-" || p.api.id.Timezone != "Europe/Istanbul" || p.Servers.Cities == nil {
					t.Fatalf("defaults %+v", p.settings)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err %v, want %q", err, tc.want)
			}
		})
	}
}

// The list is read in pages of 50 after the last serial; every match on
// any page is revoked, and Count 0 is counted as already revoked, not an
// error.
func TestRevokePages(t *testing.T) {
	f, cfg := newFake(t)
	for i := range 120 {
		name := fmt.Sprintf("wgp-a-other-1-%d", i) // another output
		if i%12 == 11 {
			name = fmt.Sprintf("wgp-a-proton-1-%d", i)
		}
		f.certs = append(f.certs, fakeCert{Serial: fmt.Sprintf("s%03d", i), DeviceName: name, Mode: "persistent", Ghost: i == 119})
	}
	st := signedIn(t, f)
	p := newTick(t, cfg, &clock{time.Now()}, 1, nil)
	revoked, already, err := p.revoke(t.Context(), st, func(int, string) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	var lists, deletes []string
	for _, r := range f.requests() {
		switch r.Method {
		case http.MethodGet:
			lists = append(lists, r.Path)
		case http.MethodDelete:
			deletes = append(deletes, fmt.Sprint(r.Body["SerialNumber"]))
		}
	}
	base := "/vpn/v1/certificate/all?Mode=persistent&Limit=50"
	if !slices.Equal(lists, []string{"/tests/ping", base, base + "&BeginID=s049", base + "&BeginID=s099"}) {
		t.Errorf("lists %q", lists)
	}
	if len(deletes) != 10 || deletes[0] != "s011" || deletes[9] != "s119" {
		t.Errorf("deletes %q", deletes)
	}
	if revoked != 9 || already != 1 {
		t.Errorf("revoked %d, already %d", revoked, already)
	}
	if got := deviceNames(f); len(got) != 110 || slices.ContainsFunc(got, func(n string) bool { return strings.HasPrefix(n, "wgp-a-proton-") }) {
		t.Errorf("left %d", len(got))
	}
}
