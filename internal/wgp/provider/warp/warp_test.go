package warp

import (
	"context"
	"crypto/ecdh"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a1ekseev/scd/internal/common/candidate"
	"github.com/a1ekseev/scd/internal/wgp/config"
	"github.com/a1ekseev/scd/internal/wgp/provider"
	"github.com/a1ekseev/scd/internal/wgp/transport"
)

// memStore is an in-memory provider.Store; a new WARP on the same memStore
// is a restart.
type memStore struct{ b []byte }

func (m *memStore) Load() ([]byte, error) { return m.b, nil }
func (m *memStore) Save(b []byte) error   { m.b = slices.Clone(b); return nil }

// fakeDevice is a device of the fake API.
type fakeDevice struct {
	token, key string
	tunnel     string // "" is wireguard
	clientID   byte   // reserved is {clientID, clientID+1, clientID+2}
}

// fakeAPI is an httptest fake of the WARP API. get queues the statuses of
// the next GET answers (0 = normal); reg the same for POST /reg.
type fakeAPI struct {
	t       *testing.T
	mu      sync.Mutex
	devices map[string]*fakeDevice
	seq     byte
	calls   []string // "POST /reg", "GET", "PATCH"
	bodies  [][]byte // POST /reg bodies
	patches []string // PATCH bodies
	get     []int
	reg     []int
	patch   []int
	enabled bool
	cfg     transport.Config
	// peerPEM is the MASQUE endpoint key of the answers.
	peerPEM string
	// onReg, when set, runs on a POST /reg, which then fails with 503.
	onReg func()
}

func newFake(t *testing.T) *fakeAPI {
	t.Helper()
	f := &fakeAPI{t: t, devices: map[string]*fakeDevice{}, enabled: true, peerPEM: dummyPEM}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(f.serve))
	srv.TLS, f.cfg.RootCAs = testTLS(t)
	srv.StartTLS()
	t.Cleanup(srv.Close)
	addr := srv.Listener.Addr().String()
	f.cfg.API.TimeoutMs, f.cfg.API.Retry.Attempts = 5000, 1
	f.cfg.Dial = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return new(net.Dialer).DialContext(ctx, network, addr)
	}
	return f
}

func (f *fakeAPI) answer(w http.ResponseWriter, id string, d *fakeDevice, token string) {
	r := map[string]any{
		"id": id, "key": d.key, "enabled": f.enabled, "warp_enabled": false,
		"account": map[string]any{"account_type": "free"},
		"config": map[string]any{
			"client_id": base64.StdEncoding.EncodeToString([]byte{d.clientID, d.clientID + 1, d.clientID + 2}),
			"interface": map[string]any{"addresses": map[string]string{"v4": "172.16.0.2", "v6": "2606:4700:110:8a36::2"}},
			"peers": []any{map[string]any{
				"public_key": "bmXOC+F1FxEMF9dyiK2H5/1SUtzH0JuVo51h2wPfgyo=",
				"endpoint":   map[string]any{"host": "engage.cloudflareclient.com:2408", "v4": "162.159.192.1:0"},
			}},
		},
	}
	if d.tunnel == "masque" {
		r["config"].(map[string]any)["peers"] = []any{map[string]any{
			"public_key": f.peerPEM,
			"endpoint":   map[string]any{"host": "engage.cloudflareclient.com:2408", "v4": "162.159.198.2:0", "v6": "[2606:4700:103::2]:0"},
		}}
	}
	if token != "" {
		r["token"] = token
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(r)
}

func fail(w http.ResponseWriter, status int) {
	codes := map[int]int{404: 1006, 401: 1012}
	if status == http.StatusTooManyRequests {
		w.Header().Set("Retry-After", "1")
	}
	w.WriteHeader(status)
	if c := codes[status]; c != 0 {
		_, _ = fmt.Fprintf(w, `{"success":false,"errors":[{"code":%d,"message":"x"}]}`, c)
	}
}

func pop(q *[]int) int {
	if len(*q) == 0 {
		return 0
	}
	s := (*q)[0]
	*q = (*q)[1:]
	return s
}

func (f *fakeAPI) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	body, _ := io.ReadAll(r.Body)
	id, isDevice := strings.CutPrefix(r.URL.Path, "/v0a5641/reg/")
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/v0a5641/reg":
		f.calls = append(f.calls, "POST /reg")
		f.bodies = append(f.bodies, body)
		if f.onReg != nil {
			f.onReg()
			fail(w, http.StatusServiceUnavailable)
			return
		}
		if s := pop(&f.reg); s != 0 {
			fail(w, s)
			return
		}
		var b map[string]string
		if err := json.Unmarshal(body, &b); err != nil || r.Header.Get("Authorization") != "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f.seq++
		id := fmt.Sprintf("00000000-0000-4000-8000-%012d", f.seq)
		d := &fakeDevice{token: fmt.Sprintf("secret-token-%d", f.seq), key: b["key"], clientID: f.seq * 10}
		f.devices[id] = d
		f.answer(w, id, d, d.token)
	case isDevice && (r.Method == http.MethodGet || r.Method == http.MethodPatch):
		f.calls = append(f.calls, r.Method)
		if s := pop(&f.get); s != 0 && r.Method == http.MethodGet {
			fail(w, s)
			return
		}
		d := f.devices[id]
		switch {
		case d == nil:
			fail(w, http.StatusNotFound)
			return
		case r.Header.Get("Authorization") != "Bearer "+d.token:
			fail(w, http.StatusUnauthorized)
			return
		}
		if r.Method == http.MethodPatch {
			if s := pop(&f.patch); s != 0 {
				fail(w, s)
				return
			}
			var b map[string]string
			f.patches = append(f.patches, string(body))
			kinds := map[string]string{"curve25519": "wireguard", "secp256r1": "masque"}
			if err := json.Unmarshal(body, &b); err != nil || b["tunnel_type"] == "" || kinds[b["key_type"]] != b["tunnel_type"] {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			d.key, d.tunnel = b["key"], b["tunnel_type"]
			d.clientID++
		}
		f.answer(w, id, d, "")
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// with runs fn under the fake's lock.
func (f *fakeAPI) with(fn func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn()
}

// takeCalls returns and clears the calls seen so far.
func (f *fakeAPI) takeCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.calls
	f.calls = nil
	return c
}

// clock is a settable test clock.
type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

// newTest returns a WARP with n devices against f, as Factory.New would.
func newTest(f *fakeAPI, c *clock, n int, mutate func(*WARP)) *WARP {
	w := &WARP{id: "warp", out: provider.Output{ID: "warp", Candidates: n}, ident: testID, cfg: f.cfg, now: c.now}
	w.Tunnel, w.Label = "wireguard", "WARP"
	w.DeviceLostAfterChecks, w.ReRegisterMinIntervalMinutes = 2, 60
	w.WireGuard = WireGuard{DNS: cloudflareDNS, MTU: 1280, AllowedIPs: []string{"0.0.0.0/0", "::/0"}}
	if mutate != nil {
		mutate(w)
	}
	return w
}

func tick(t *testing.T, w *WARP, s provider.Store) provider.Result {
	t.Helper()
	res, err := w.Tick(t.Context(), s)
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	return res
}

// parse checks every line with the scd parser and returns them.
func parse(t *testing.T, lines []string) []candidate.WireGuard {
	t.Helper()
	var out []candidate.WireGuard
	for _, l := range lines {
		c, err := candidate.ParseWireGuard(l)
		if err != nil {
			t.Fatalf("ParseWireGuard: %v", err)
		}
		if c.Provider != Type {
			t.Fatalf("provider %q", c.Provider)
		}
		out = append(out, c)
	}
	return out
}

func pub(c candidate.WireGuard) string {
	k, _ := ecdh.X25519().NewPrivateKey(c.PrivateKey[:])
	return base64.StdEncoding.EncodeToString(k.PublicKey().Bytes())
}

func TestRegister(t *testing.T) {
	f := newFake(t)
	c := &clock{time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)}
	s := &memStore{}
	w := newTest(f, c, 2, nil)
	res := tick(t, w, s)
	if got := f.takeCalls(); !slices.Equal(got, []string{"POST /reg", "POST /reg"}) {
		t.Fatalf("calls %v", got)
	}
	got := parse(t, res.Lines)
	if len(got) != 2 || res.Status.Issued != 2 || res.Status.State != "ok" {
		t.Fatalf("result %+v", res)
	}
	for i, g := range got {
		d := f.devices[fmt.Sprintf("00000000-0000-4000-8000-%012d", i+1)]
		r := byte(10 * (i + 1))
		if pub(g) != d.key || *g.Reserved != [3]byte{r, r + 1, r + 2} {
			t.Errorf("line %d: key or reserved differ from the API", i)
		}
	}
	want := "@engage.cloudflareclient.com:2408?provider=warp&publickey=bmXOC%2BF1FxEMF9dyiK2H5/1SUtzH0JuVo51h2wPfgyo=" +
		"&address=172.16.0.2/32,2606:4700:110:8a36::2/128&dns=1.1.1.1,1.0.0.1,2606:4700:4700::1111,2606:4700:4700::1001" +
		"&mtu=1280&allowedips=0.0.0.0/0,::/0&reserved=10,11,12#WARP"
	if !strings.HasSuffix(res.Lines[0], want) {
		t.Errorf("line %s, want suffix %s", got[0].Redacted(), want)
	}

	// The POST /reg body: the sorted-key JSON wgp sends to the WARP API.
	var body map[string]string
	if err := json.Unmarshal(f.bodies[0], &body); err != nil {
		t.Fatal(err)
	}
	wantBody := regBody(testID, body["key"], c.t)
	if !strings.HasSuffix(string(f.bodies[0]), "\n") || fmt.Sprint(body) != fmt.Sprint(wantBody) {
		t.Errorf("body %s, want %v", f.bodies[0], wantBody)
	}
	if b, err := base64.StdEncoding.DecodeString(body["key"]); err != nil || len(b) != 32 || body["tunnel_type"] != "wireguard" {
		t.Errorf("key %q", body["key"])
	}

	// The next tick (and a restart) only checks.
	for _, w := range []*WARP{w, newTest(f, c, 2, nil)} {
		again := tick(t, w, s)
		if calls := f.takeCalls(); !slices.Equal(calls, []string{"GET", "GET"}) || !slices.Equal(again.Lines, res.Lines) {
			t.Fatalf("calls %v, lines changed %v", calls, !slices.Equal(again.Lines, res.Lines))
		}
	}
}

func TestPort(t *testing.T) {
	f := newFake(t)
	w := newTest(f, &clock{time.Now()}, 1, func(w *WARP) { w.WireGuard.Port = 4500 })
	if l := tick(t, w, &memStore{}).Lines[0]; !strings.Contains(l, "@engage.cloudflareclient.com:4500?") {
		t.Fatalf("line %s", l)
	}
}

func TestRotateOnStart(t *testing.T) {
	f := newFake(t)
	c := &clock{time.Now()}
	s := &memStore{}
	rot := func(w *WARP) { w.Keys.RotateOnStart = true }
	first := parse(t, tick(t, newTest(f, c, 1, rot), s).Lines)[0]
	if calls := f.takeCalls(); !slices.Equal(calls, []string{"POST /reg"}) {
		t.Fatalf("a new device is not rotated: %v", calls)
	}

	w := newTest(f, c, 1, rot) // restart
	second := parse(t, tick(t, w, s).Lines)[0]
	if calls := f.takeCalls(); !slices.Equal(calls, []string{"GET", "PATCH"}) {
		t.Fatalf("calls %v", calls)
	}
	if pub(second) == pub(first) || *second.Reserved == *first.Reserved {
		t.Fatal("key and reserved must change together")
	}
	if d := f.devices["00000000-0000-4000-8000-000000000001"]; pub(second) != d.key || *second.Reserved != [3]byte{11, 12, 13} {
		t.Fatal("line does not match the PATCH answer")
	}
	// Once per process; the GET answer for the new key keeps the line.
	third := parse(t, tick(t, w, s).Lines)[0]
	if calls := f.takeCalls(); !slices.Equal(calls, []string{"GET"}) || pub(third) != pub(second) || *third.Reserved != *second.Reserved {
		t.Fatalf("calls %v", calls)
	}
	// Without rotateOnStart a restart keeps the key.
	tick(t, newTest(f, c, 1, nil), s)
	if calls := f.takeCalls(); !slices.Equal(calls, []string{"GET"}) {
		t.Fatalf("calls %v", calls)
	}
}

func TestDeviceLost(t *testing.T) {
	cases := []struct {
		name     string
		gets     []int // GET answers per tick after the registration; 0 = ok, -1 = enabled false
		lostTick int   // tick (1-based) that re-registers, 0 = never
	}{
		{name: "404 twice", gets: []int{404, 404}, lostTick: 2},
		{name: "401 twice", gets: []int{401, 401}, lostTick: 2},
		{name: "401 then 404", gets: []int{401, 404}, lostTick: 2},
		{name: "5xx does not reset", gets: []int{404, 500, 404}, lostTick: 3},
		{name: "429 does not count", gets: []int{404, 429, 429, 404}, lostTick: 4},
		{name: "ok resets", gets: []int{404, 0, 404}},
		{name: "5xx only", gets: []int{500, 500, 500}},
		{name: "enabled false", gets: []int{-1, -1}, lostTick: 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(t)
			c := &clock{time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)}
			s := &memStore{}
			first := tick(t, newTest(f, c, 1, nil), s).Lines[0]
			f.takeCalls()
			for i, g := range tc.gets {
				c.t = c.t.Add(time.Hour) // past reRegisterMinIntervalMinutes
				f.with(func() {
					f.enabled = g != -1
					if g > 0 {
						f.get = []int{g}
					}
				})
				// Each tick is a restart: the counter lives in the state.
				res := tick(t, newTest(f, c, 1, nil), s)
				calls := f.takeCalls()
				f.with(func() { f.enabled = true })
				if i+1 == tc.lostTick {
					if !slices.Equal(calls, []string{"GET", "POST /reg"}) || res.Lines[0] == first || len(res.Warnings) != 0 ||
						!slices.Equal(eventNames(res.Events), []string{"device_check_lost", "device_lost", "device_registered"}) {
						t.Fatalf("tick %d: calls %v, warnings %v, events %v", i+1, calls, res.Warnings, res.Events)
					}
					if lost := res.Events[1]; lost.Slot != 1 || lost.Level != slog.LevelWarn || lost.Attrs[0].Value.Int64() != 2 {
						t.Fatalf("device_lost %+v", lost)
					}
					return
				}
				if counts := g == 404 || g == 401 || g == -1; counts != slices.Equal(eventNames(res.Events), []string{"device_check_lost"}) {
					t.Fatalf("tick %d: events %v", i+1, res.Events)
				}
				if strings.Contains(res.Status.Error, "/") {
					t.Fatalf("tick %d: a counter in the error %q", i+1, res.Status.Error)
				}
				if !slices.Equal(calls, []string{"GET"}) || res.Lines[0] != first {
					t.Fatalf("tick %d: calls %v; the candidate must stay until the device is lost", i+1, calls)
				}
				if g != 0 && res.Status.State != "error" {
					t.Fatalf("tick %d: state %q", i+1, res.Status.State)
				}
			}
			if tc.lostTick != 0 {
				t.Fatal("not re-registered")
			}
		})
	}
}

// eventNames are the names of events in order.
func eventNames(events []provider.Event) []string {
	var out []string
	for _, e := range events {
		out = append(out, e.Name)
	}
	return out
}

// noLines ticks and wants no lines, no error and a status error holding want.
func noLines(t *testing.T, w *WARP, s provider.Store, want string) {
	t.Helper()
	res, err := w.Tick(t.Context(), s)
	if err != nil || len(res.Lines) != 0 || res.Status.State != "error" || !strings.Contains(res.Status.Error, want) {
		t.Fatalf("err %v, result %+v, want %q", err, res, want)
	}
}

// The interval runs from the last POST /reg attempt, failed ones too, and
// survives restarts; only a never registered slot goes at once.
func TestReRegisterRateLimit(t *testing.T) {
	f := newFake(t)
	start := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	c := &clock{start}
	s := &memStore{}
	lost1 := func(w *WARP) { w.DeviceLostAfterChecks = 1 }
	tick(t, newTest(f, c, 1, lost1), s)
	f.with(func() { clear(f.devices) }) // the device is deleted on the Cloudflare side
	f.takeCalls()

	// Lost: every device gone, so no lines and no error (the output empties).
	c.t = start.Add(10 * time.Minute)
	noLines(t, newTest(f, c, 1, lost1), s, "re-registration not before 2026-10-29T13:00:00Z")
	c.t = start.Add(59 * time.Minute)
	noLines(t, newTest(f, c, 1, lost1), s, "re-registration not before 2026-10-29T13:00:00Z")
	if calls := f.takeCalls(); !slices.Equal(calls, []string{"GET"}) {
		t.Fatalf("calls %v", calls)
	}

	// A failed attempt at 13:00 moves the limit to 14:00, across a restart.
	f.with(func() { f.reg = []int{http.StatusTooManyRequests} })
	c.t = start.Add(60 * time.Minute)
	noLines(t, newTest(f, c, 1, lost1), s, "register: status 429")
	c.t = start.Add(119 * time.Minute)
	noLines(t, newTest(f, c, 1, lost1), s, "re-registration not before 2026-10-29T14:00:00Z")
	if calls := f.takeCalls(); !slices.Equal(calls, []string{"POST /reg"}) {
		t.Fatalf("calls %v", calls)
	}
	c.t = start.Add(120 * time.Minute)
	tick(t, newTest(f, c, 1, lost1), s)
	if calls := f.takeCalls(); !slices.Equal(calls, []string{"POST /reg"}) {
		t.Fatalf("calls %v", calls)
	}

	// A failed first registration also counts for the next one.
	s2 := &memStore{}
	f.with(func() { f.reg = []int{http.StatusTooManyRequests} })
	noLines(t, newTest(f, c, 1, nil), s2, "register: status 429")
	noLines(t, newTest(f, c, 1, nil), s2, "re-registration not before")
}

func TestRetryAfter(t *testing.T) {
	f := newFake(t)
	f.reg = []int{http.StatusTooManyRequests}
	f.cfg.API.Retry.Attempts = 2
	begin := time.Now()
	res := tick(t, newTest(f, &clock{time.Now()}, 1, nil), &memStore{})
	if el := time.Since(begin); el < 900*time.Millisecond || len(res.Lines) != 1 {
		t.Fatalf("elapsed %v, lines %d", el, len(res.Lines))
	}
	if calls := f.takeCalls(); !slices.Equal(calls, []string{"POST /reg", "POST /reg"}) {
		t.Fatalf("calls %v", calls)
	}
	// Out of attempts: an error with the status only.
	f.with(func() { f.reg = []int{http.StatusTooManyRequests} })
	f.cfg.API.Retry.Attempts = 1
	noLines(t, newTest(f, &clock{time.Now()}, 1, nil), &memStore{}, "device 1: register: status 429")
}

func TestBadReserved(t *testing.T) {
	f := newFake(t)
	s := &memStore{}
	w := newTest(f, &clock{time.Now()}, 1, nil)
	tick(t, w, s)
	var rec record
	if err := json.Unmarshal(s.b, &rec); err != nil {
		t.Fatal(err)
	}
	rec.Devices[0].Profile.ClientID = "AAAAAA==" // 4 bytes
	s.b, _ = json.Marshal(rec)
	f.with(func() { f.devices["00000000-0000-4000-8000-000000000001"].key = "other" }) // the GET answer does not refresh the profile
	if _, err := w.Tick(t.Context(), s); err == nil || err.Error() != "device 1: client_id: reserved must be exactly 3 bytes" {
		t.Fatalf("err %v", err)
	}
}

// TestNoSecrets runs failing and working ticks and checks that no token or
// private key reaches warnings, status, errors or print-config.
func TestNoSecrets(t *testing.T) {
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
	rot := func(w *WARP) { w.Keys.RotateOnStart = true; w.DeviceLostAfterChecks = 2 }
	run(newTest(f, c, 2, rot))
	f.with(func() { f.get = []int{404, 401} })
	run(newTest(f, c, 2, rot))
	f.with(func() { f.get = []int{404, 401} })
	c.t = c.t.Add(2 * time.Hour)
	run(newTest(f, c, 2, rot))
	f.with(func() { f.reg = []int{429, 429}; clear(f.devices) })
	c.t = c.t.Add(2 * time.Hour)
	run(newTest(f, c, 2, func(w *WARP) { w.DeviceLostAfterChecks = 1 }))

	var rec record
	if err := json.Unmarshal(s.b, &rec); err != nil {
		t.Fatal(err)
	}
	var secrets []string
	for _, d := range f.devices {
		secrets = append(secrets, d.token)
	}
	for i := range f.seq {
		secrets = append(secrets, fmt.Sprintf("secret-token-%d", i+1))
	}
	for _, d := range rec.Devices {
		if d.PrivateKey != nil {
			secrets = append(secrets, base64.StdEncoding.EncodeToString(d.PrivateKey))
		}
	}
	all := strings.Join(seen, "\n")
	for _, sec := range secrets {
		if strings.Contains(all, sec) {
			t.Fatalf("secret in output:\n%s", all)
		}
	}
	if !strings.Contains(all, "status 404, code 1006") || !strings.Contains(all, "status 429") {
		t.Fatalf("errors missing:\n%s", all)
	}
}

func TestConfig(t *testing.T) {
	const head = `server: {listen: 127.0.0.1:8080}
state: {dir: /app/state, keyFile: /run/secrets/key}
deviceNamePrefix: wgp-a-
outputs: [{id: warp, path: /app/out/warp, provider: warp, candidates: 2}]
`
	const ident = "identification: {warp: {userAgent: ua, clientVersion: cv, apiVersion: v0a5641, model: PC, locale: en_US, osVersion: \"16\"}}\n"
	const prov = "providers:\n  - {id: warp, type: warp, schedule: \"@hourly\", tunnel: wireguard, keys: {rotateOnStart: true}, reRegisterMinIntervalMinutes: 60, label: WARP"
	cases := []struct{ name, cfg, want string }{
		{name: "ok", cfg: ident + prov + "}"},
		{name: "no identification string", cfg: strings.Replace(ident, "model: PC, ", "", 1) + prov + "}", want: "identification.warp.model: required"},
		{name: "bad apiVersion", cfg: strings.Replace(ident, "v0a5641", "v0/x", 1) + prov + "}", want: "apiVersion: must match"},
		{name: "masque keys", cfg: ident + prov + ", masque: {port: 443}}", want: "masque: not allowed with tunnel wireguard"},
		{name: "no rotateOnStart", cfg: ident + strings.Replace(prov, "rotateOnStart: true", "", 1) + "}", want: "keys.rotateOnStart: required"},
		{name: "no interval", cfg: ident + strings.Replace(prov, "reRegisterMinIntervalMinutes: 60, ", "", 1) + "}", want: "reRegisterMinIntervalMinutes: required"},
		{name: "bad port", cfg: ident + prov + ", wireguard: {port: 51820}}", want: "port: must be one of 2408|500|1701|4500"},
		{name: "dns domain", cfg: ident + prov + ", wireguard: {dns: [one.one.one.one]}}", want: "wireguard: dns: each must be an IP address"},
		{name: "lost checks 0", cfg: ident + prov + ", deviceLostAfterChecks: 0}", want: "deviceLostAfterChecks: must be at least 1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "config.yml")
			if err := os.WriteFile(p, []byte(head+tc.cfg+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			l, err := config.Load(p)
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				pc, _ := l.Config.Provider("warp")
				w := pc.Impl.(*WARP)
				if w.DeviceLostAfterChecks != 2 || w.WireGuard.MTU != 1280 || len(w.WireGuard.DNS) != 4 || w.out.Candidates != 2 || w.ident.OSVersion != "16" {
					t.Fatalf("defaults %+v", w)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err %v, want %q", err, tc.want)
			}
		})
	}
}
