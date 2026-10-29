package proton

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a1ekseev/scd/internal/common/wgpstatus"
	"github.com/a1ekseev/scd/internal/wgp/provider"
	"github.com/a1ekseev/scd/internal/wgp/provider/proton/srp/srptest"
	"github.com/a1ekseev/scd/internal/wgp/transport"
)

const (
	fakeUser     = "test-user"
	fakePassword = "pw-Secret-7731"
	fakeTOTP     = "135790"
	fakeHV       = "hv-challenge-42"
)

// seen is one request the fake API got.
type seen struct {
	Method, Path, Bearer, UID string
	Header                    http.Header
	Body                      map[string]any
	// Conn is the number of the request's TCP connection in dial order.
	Conn int
}

// fakeAPI is the Proton API end of a login, a session and a logout. It
// issues tokens tok-access-N and tok-refresh-N, accepts only the current
// access token, and rotates the refresh token on each refresh.
type fakeAPI struct {
	t      *testing.T
	signed string
	salt   []byte

	mu       sync.Mutex
	conns    []string // client addresses of the connections in dial order
	srp      map[string]*srptest.Server
	n        int
	uid      string
	access   string // the current access token
	refresh  string // the current refresh token
	issued   map[string]bool
	accessOK bool
	seen     []seen
	// refreshes counts the refresh requests.
	refreshes int

	// Knobs set by tests before the requests.
	twoFA       bool
	hv          string // a human verification token /auth/info requires
	badProof    bool
	failBatch   bool
	refreshFail [2]int // status and code of every refresh, zero for success
	logout401   bool   // DELETE /auth answers 401
	// The VPN side (tick_test.go): the account, the server list and the
	// certificates; now is the clock of the certificate times.
	maxTier, maxConnect int
	logicals            []map[string]any
	certs               []fakeCert
	serials             int
	failCert            bool
	now                 func() time.Time
	// hold401 delays every 401 until that many are pending.
	hold401 int
	held    int
	release chan struct{}
}

func newFake(t *testing.T) (*fakeAPI, transport.Config) {
	t.Helper()
	signed, err := os.ReadFile("srp/testdata/modulus-1.asc")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeAPI{
		t: t, signed: string(signed), salt: []byte("0123456789"),
		srp: map[string]*srptest.Server{}, issued: map[string]bool{}, release: make(chan struct{}),
		maxConnect: 2, now: time.Now,
	}
	pool, cert := testCert(t)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(f.serve))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	addr := srv.Listener.Addr().String()
	var api provider.API
	api.TimeoutMs = 10000
	api.Retry.Attempts = 1
	return f, transport.Config{
		RootCAs: pool,
		API:     api,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			c, err := new(net.Dialer).DialContext(ctx, network, addr)
			if err == nil {
				f.mu.Lock()
				f.conns = append(f.conns, c.LocalAddr().String())
				f.mu.Unlock()
			}
			return c, err
		},
	}
}

func (f *fakeAPI) reply(w http.ResponseWriter, status int, v any) {
	b, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

func (f *fakeAPI) fail(w http.ResponseWriter, status, code int) {
	f.reply(w, status, map[string]any{"Code": code, "Error": "fake error"})
}

// newTokens issues a new access and refresh token. f.mu must be held.
func (f *fakeAPI) newTokens() {
	f.n++
	f.access, f.refresh = fmt.Sprintf("tok-access-%d", f.n), fmt.Sprintf("tok-refresh-%d", f.n)
	f.issued[f.access], f.accessOK = true, true
}

// expire makes the current access token answer 401.
func (f *fakeAPI) expire() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.accessOK = false
}

func (f *fakeAPI) requests() []seen {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.seen)
}

func (f *fakeAPI) serve(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	bearer, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	f.mu.Lock()
	f.seen = append(f.seen, seen{Method: r.Method, Path: r.URL.RequestURI(), Bearer: bearer, UID: r.Header.Get("x-pm-uid"), Header: r.Header.Clone(), Body: body, Conn: slices.Index(f.conns, r.RemoteAddr)})
	route := r.Method + " " + r.URL.Path
	switch route {
	case "GET /tests/ping":
		f.mu.Unlock()
		f.reply(w, 200, map[string]any{"Code": 1000})
		return
	case "POST /auth/info", "POST /auth":
		if f.hv != "" {
			switch tok := r.Header.Get("x-pm-human-verification-token"); {
			case tok == "":
				f.mu.Unlock()
				f.reply(w, 422, map[string]any{
					"Code": codeHumanVerify, "Error": "Human verification required",
					"Details": map[string]any{"HumanVerificationToken": fakeHV, "HumanVerificationMethods": []string{"captcha"}},
				})
				return
			case tok != f.hv || r.Header.Get("x-pm-human-verification-token-type") != "captcha":
				f.mu.Unlock()
				f.fail(w, 422, codeBadHVToken)
				return
			}
		}
		if route == "POST /auth/info" {
			f.authInfo(w, body)
		} else {
			f.auth(w, body)
		}
		f.mu.Unlock()
		return
	case "POST /auth/refresh":
		f.refreshes++
		switch {
		case f.refreshFail[0] != 0:
			f.mu.Unlock()
			f.fail(w, f.refreshFail[0], f.refreshFail[1])
		case !f.issued[bearer] || r.Header.Get("x-pm-uid") != f.uid || body["RefreshToken"] != f.refresh:
			f.mu.Unlock()
			f.fail(w, 422, codeRefreshInvalid)
		default:
			f.newTokens()
			out := map[string]any{"Code": 1000, "UID": f.uid, "AccessToken": f.access, "RefreshToken": f.refresh, "ExpiresIn": 86400}
			f.mu.Unlock()
			f.reply(w, 200, out)
		}
		return
	}
	if bearer != f.access || !f.accessOK || r.Header.Get("x-pm-uid") != f.uid {
		f.held++
		hold := f.hold401 > 0
		if f.held == f.hold401 {
			close(f.release)
		}
		f.mu.Unlock()
		if hold {
			select {
			case <-f.release:
			case <-time.After(5 * time.Second):
			}
		}
		f.fail(w, 401, 401)
		return
	}
	defer f.mu.Unlock()
	switch route {
	case "POST /auth/2fa":
		if body["TwoFactorCode"] != fakeTOTP {
			f.fail(w, 422, codeWrongPassword)
			return
		}
		f.reply(w, 200, map[string]any{"Code": 1000, "Scopes": []string{"vpn"}})
	case "DELETE /auth":
		if f.logout401 {
			f.fail(w, 401, 401)
			return
		}
		f.accessOK = false
		f.reply(w, 200, map[string]any{"Code": 1000})
	case "GET /vpn/v1/location":
		if f.failBatch {
			f.fail(w, 500, 2500)
			return
		}
		f.reply(w, 200, map[string]any{"Code": 1000, "IP": "203.0.113.77"})
	case "GET /vpn/v2", "POST /vpn/v1/certificate", "GET /vpn/v2/clientconfig", "GET /feature/v2/frontend",
		"GET /vpn/v1/logicals", "GET /core/v4/notifications", "GET /vpn/v1/certificate/all", "DELETE /vpn/v1/certificate":
		if f.failBatch {
			f.fail(w, 500, 2500)
			return
		}
		f.vpn(w, r, route, body)
	default:
		f.fail(w, 404, 2501)
	}
}

// authInfo answers /auth/info. f.mu is held.
func (f *fakeAPI) authInfo(w http.ResponseWriter, body map[string]any) {
	if body["Username"] != fakeUser {
		f.fail(w, 422, 2001)
		return
	}
	s, err := srptest.NewServer(f.signed, f.salt, []byte(fakePassword))
	if err != nil {
		f.t.Error(err)
		f.fail(w, 500, 0)
		return
	}
	session := fmt.Sprintf("srp-session-%d", len(f.srp))
	f.srp[session] = s
	f.reply(w, 200, map[string]any{
		"Code": 1000, "Version": 4, "Modulus": f.signed, "Salt": base64.StdEncoding.EncodeToString(f.salt),
		"ServerEphemeral": base64.StdEncoding.EncodeToString(s.Ephemeral()), "SRPSession": session,
	})
}

// auth answers /auth. f.mu is held.
func (f *fakeAPI) auth(w http.ResponseWriter, body map[string]any) {
	s := f.srp[fmt.Sprint(body["SRPSession"])]
	a, _ := base64.StdEncoding.DecodeString(fmt.Sprint(body["ClientEphemeral"]))
	m1, _ := base64.StdEncoding.DecodeString(fmt.Sprint(body["ClientProof"]))
	if s == nil {
		f.fail(w, 422, 2001)
		return
	}
	m2, ok := s.Verify(a, m1)
	if !ok {
		f.fail(w, 422, codeWrongPassword)
		return
	}
	if f.badProof {
		m2[0] ^= 1
	}
	f.uid = "fake-uid-1"
	f.newTokens()
	twoFA := 0
	if f.twoFA {
		twoFA = totpBit
	}
	f.reply(w, 200, map[string]any{
		"Code": 1000, "UID": f.uid, "AccessToken": f.access, "RefreshToken": f.refresh, "ExpiresIn": 1800,
		"ServerProof": base64.StdEncoding.EncodeToString(m2), "2FA": map[string]any{"Enabled": twoFA, "TOTP": twoFA},
	})
}

// memStore is an in-memory provider.Store.
type memStore struct {
	mu sync.Mutex
	b  []byte
}

func (s *memStore) Load() ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return bytes.Clone(s.b), nil
}

func (s *memStore) Save(b []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.b = bytes.Clone(b)
	return nil
}

func (s *memStore) session(t *testing.T) tokens {
	t.Helper()
	var rec struct{ Session tokens }
	if b, _ := s.Load(); json.Unmarshal(b, &rec) != nil {
		t.Fatalf("state %q", b)
	}
	return rec.Session
}

func newTestProton(t *testing.T, cfg transport.Config) *Proton {
	t.Helper()
	p, err := newProton("pv", testID, cfg)
	if err != nil {
		t.Fatal(err)
	}
	p.out, p.prefix = provider.Output{ID: "proton", Candidates: 2}, "wgp-a-"
	return p
}

// login runs Login with input lines; it returns the terminal output.
func login(t *testing.T, p *Proton, s provider.Store, hv string, lines ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := p.Login(t.Context(), provider.TTY{In: strings.NewReader(strings.Join(lines, "\n") + "\n"), Out: &out, HVToken: hv}, s)
	return out.String(), err
}

func paths(rs []seen) []string {
	var p []string
	for _, r := range rs {
		p = append(p, r.Method+" "+r.Path)
	}
	return p
}

// The login runs ping, SRP and 2FA in the order of the client, saves only
// the session, then sends the after-login batch in its order.
func TestLogin(t *testing.T) {
	f, cfg := newFake(t)
	f.twoFA = true
	p, st := newTestProton(t, cfg), &memStore{}
	out, err := login(t, p, st, "", fakeUser, fakePassword, fakeTOTP)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Signed in.") {
		t.Errorf("output %q", out)
	}
	if tk := st.session(t); tk != (tokens{UID: "fake-uid-1", AccessToken: "tok-access-1", RefreshToken: "tok-refresh-1"}) {
		t.Errorf("session %+v", tk)
	}
	var rec map[string]json.RawMessage
	_ = json.Unmarshal(st.b, &rec)
	if len(rec) != 1 {
		t.Errorf("state has more than the session: %s", st.b)
	}

	rs := f.requests()
	got := paths(rs)
	want := []string{"GET /tests/ping", "POST /auth/info", "POST /auth", "POST /auth/2fa"}
	if !slices.Equal(got[:4], want) {
		t.Fatalf("login requests %q", got)
	}
	// The parallel batch: its connections are opened in the client's order.
	parallel := slices.Clone(rs[4:8])
	slices.SortFunc(parallel, func(a, b seen) int { return a.Conn - b.Conn })
	if p := paths(parallel); !slices.Equal(p, []string{"GET /vpn/v2", "POST /vpn/v1/certificate", "GET /vpn/v1/location", "GET /vpn/v2/clientconfig"}) {
		t.Errorf("parallel batch in connection order %q", p)
	}
	if !slices.Equal(got[8:], []string{"GET /feature/v2/frontend", "GET /vpn/v1/logicals?SecureCoreFilter=all&WithState=true", "GET /core/v4/notifications"}) {
		t.Errorf("sequential batch %q", got[8:])
	}
	for _, r := range rs[4:] {
		if r.Bearer != "tok-access-1" || r.UID != "fake-uid-1" {
			t.Errorf("%s %s without the session", r.Method, r.Path)
		}
		if r.Path == "/vpn/v1/logicals?SecureCoreFilter=all&WithState=true" &&
			(r.Header.Get("X-PM-netzone") != "203.0.113.0" || r.Header.Get("If-Modified-Since") != "Thu, 01 Jan 1970 00:00:00 GMT") {
			t.Errorf("logicals headers %v", r.Header)
		}
		if r.Header.Get("x-pm-human-verification-token") != "" {
			t.Errorf("%s: HV header without --hv-token", r.Path)
		}
	}

	// The session certificate: a fresh Ed25519 key, the body of the client,
	// and nothing of it in the state.
	i := slices.Index(got, "POST /vpn/v1/certificate")
	cb := rs[i].Body
	key, _ := cb["ClientPublicKey"].(string)
	if len(cb) != 2 || cb["Duration"] != "10080 min" {
		t.Errorf("certificate body %v", cb)
	}
	blk, _ := pem.Decode([]byte(key))
	if blk == nil {
		t.Fatalf("certificate key %q", key)
	}
	if pub, err := x509.ParsePKIXPublicKey(blk.Bytes); err != nil || fmt.Sprintf("%T", pub) != "ed25519.PublicKey" {
		t.Errorf("certificate key %T %v", pub, err)
	}
	if bytes.Contains(st.b, []byte(strings.Split(key, "\n")[1])) || bytes.Contains(st.b, []byte("session-serial-1")) {
		t.Error("the session certificate is in the state")
	}

	// Logout lists the certificates, revokes nothing of the session
	// certificate and closes the session.
	if err := p.Logout(t.Context(), st); err != nil {
		t.Fatal(err)
	}
	rs = f.requests()[len(rs):]
	if got := paths(rs); !slices.Equal(got, []string{"GET /vpn/v1/certificate/all?Mode=persistent&Limit=50", "DELETE /auth"}) || rs[1].Bearer != "tok-access-1" {
		t.Errorf("logout requests %q", got)
	}
	if tk := st.session(t); tk != (tokens{}) {
		t.Errorf("session after logout %+v", tk)
	}
	if err := p.Logout(t.Context(), st); err != nil {
		t.Errorf("second logout: %v", err)
	}
}

// Failed logins save nothing and say why.
func TestLoginFailures(t *testing.T) {
	for _, tc := range []struct {
		name   string
		setup  func(*fakeAPI)
		lines  []string
		hv     string
		want   string
		output string
	}{
		{"wrong password", nil, []string{fakeUser, "wrong"}, "", "code 8002", ""},
		{"wrong totp", func(f *fakeAPI) { f.twoFA = true }, []string{fakeUser, fakePassword, "000000"}, "", "code 8002", ""},
		{"bad server proof", func(f *fakeAPI) { f.badProof = true; f.twoFA = true }, []string{fakeUser, fakePassword, fakeTOTP}, "", "server proof does not match", ""},
		{
			"captcha", func(f *fakeAPI) { f.hv = fakeHV },
			[]string{fakeUser, fakePassword},
			"", "code 9001): run login again with --hv-token",
			"https://verify.proton.me/?methods=captcha&token=" + fakeHV,
		},
		{"bad hv token", func(f *fakeAPI) { f.hv = fakeHV }, []string{fakeUser, fakePassword}, "stale", "code 12087", ""},
		{"empty password", nil, []string{fakeUser, ""}, "", "empty password", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, cfg := newFake(t)
			if tc.setup != nil {
				tc.setup(f)
			}
			st := &memStore{}
			out, err := login(t, newTestProton(t, cfg), st, tc.hv, tc.lines...)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err %v, want %q", err, tc.want)
			}
			if !strings.Contains(out, tc.output) {
				t.Errorf("output %q, want %q", out, tc.output)
			}
			if st.b != nil {
				t.Errorf("state saved: %s", st.b)
			}
			got := paths(f.requests())
			if slices.Contains(got, "POST /auth/2fa") && tc.name == "bad server proof" {
				t.Errorf("2fa after a bad server proof: %q", got)
			}
		})
	}
}

// The CAPTCHA token of --hv-token goes in the human verification headers.
func TestLoginHVToken(t *testing.T) {
	f, cfg := newFake(t)
	f.hv = fakeHV
	st := &memStore{}
	if _, err := login(t, newTestProton(t, cfg), st, fakeHV, fakeUser, fakePassword); err != nil {
		t.Fatal(err)
	}
	for _, r := range f.requests()[1:3] {
		if r.Header.Get("x-pm-human-verification-token") != fakeHV || r.Header.Get("x-pm-human-verification-token-type") != "captcha" {
			t.Errorf("%s: headers %v", r.Path, r.Header)
		}
	}
	if st.session(t).AccessToken == "" {
		t.Error("no session")
	}
}

// Errors on the whole batch do not fail the login.
func TestLoginBatchErrors(t *testing.T) {
	f, cfg := newFake(t)
	f.failBatch = true
	st := &memStore{}
	if _, err := login(t, newTestProton(t, cfg), st, "", fakeUser, fakePassword); err != nil {
		t.Fatal(err)
	}
	if n := len(f.requests()); n != 10 {
		t.Errorf("%d requests, want 10", n)
	}
	if st.session(t).RefreshToken != "tok-refresh-1" {
		t.Error("no session")
	}
}

// After a restart the first request goes with the old Bearer from the
// state; its 401 refreshes with that Bearer, the rotated tokens are saved,
// and the old refresh token no longer works.
func TestRefreshAfterRestart(t *testing.T) {
	f, cfg := newFake(t)
	st := &memStore{}
	if _, err := login(t, newTestProton(t, cfg), st, "", fakeUser, fakePassword); err != nil {
		t.Fatal(err)
	}
	old, _ := st.Load()
	f.expire()
	mark := len(f.requests())

	p := newTestProton(t, cfg) // a new process
	resp, err := p.authed(t.Context(), st, call{method: http.MethodGet, path: "/vpn/v2"})
	if err != nil || resp.Status != 200 {
		t.Fatalf("%d %v", resp.Status, err)
	}
	rs := f.requests()[mark:]
	if got := paths(rs); !slices.Equal(got, []string{"GET /tests/ping", "GET /vpn/v2", "POST /auth/refresh", "GET /vpn/v2"}) {
		t.Fatalf("requests %q", got)
	}
	if rs[1].Bearer != "tok-access-1" || rs[2].Bearer != "tok-access-1" || rs[2].Body["RefreshToken"] != "tok-refresh-1" || rs[3].Bearer != "tok-access-2" {
		t.Errorf("tokens: %q %q %v %q", rs[1].Bearer, rs[2].Bearer, rs[2].Body["RefreshToken"], rs[3].Bearer)
	}
	if tk := st.session(t); tk != (tokens{UID: "fake-uid-1", AccessToken: "tok-access-2", RefreshToken: "tok-refresh-2"}) {
		t.Errorf("saved %+v", tk)
	}

	// The state from before the refresh holds the rotated-out token.
	stale := &memStore{b: old}
	_, err = newTestProton(t, cfg).authed(t.Context(), stale, call{method: http.MethodGet, path: "/vpn/v2"})
	if !errors.Is(err, errNeedsLogin) || !strings.Contains(err.Error(), "code 10013") {
		t.Fatalf("stale refresh token: %v", err)
	}
	if !bytes.Equal(stale.b, old) {
		t.Error("a rejected refresh changed the state")
	}
}

// Concurrent 401s of one token make one refresh, then every call repeats
// with the new token.
func TestRefreshRace(t *testing.T) {
	f, cfg := newFake(t)
	st := &memStore{}
	p := newTestProton(t, cfg)
	if _, err := login(t, p, st, "", fakeUser, fakePassword); err != nil {
		t.Fatal(err)
	}
	const n = 8
	f.mu.Lock()
	f.accessOK, f.hold401, f.held = false, n, 0
	f.mu.Unlock()
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		wg.Go(func() {
			_, errs[i] = p.authed(t.Context(), st, call{method: http.MethodGet, path: "/vpn/v2"})
		})
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.refreshes != 1 || f.held != n {
		t.Fatalf("%d refreshes for %d 401s", f.refreshes, f.held)
	}
	if st.session(t).AccessToken != f.access {
		t.Error("the new tokens are not saved")
	}
}

// A refresh rejected with 400, 422 or code 10013 means needs_login: no
// rotation, the state stays, and later calls do not refresh again.
func TestRefreshNeedsLogin(t *testing.T) {
	for _, rf := range [][2]int{{422, 2001}, {400, 2001}, {409, codeRefreshInvalid}} {
		t.Run(fmt.Sprint(rf), func(t *testing.T) {
			f, cfg := newFake(t)
			st := &memStore{}
			p := newTestProton(t, cfg)
			if _, err := login(t, p, st, "", fakeUser, fakePassword); err != nil {
				t.Fatal(err)
			}
			before, _ := st.Load()
			f.expire()
			f.mu.Lock()
			f.refreshFail = rf
			f.mu.Unlock()
			for range 2 {
				if _, err := p.authed(t.Context(), st, call{method: http.MethodGet, path: "/vpn/v2"}); !errors.Is(err, errNeedsLogin) {
					t.Fatalf("err %v", err)
				}
			}
			if f.refreshes != 1 {
				t.Errorf("%d refreshes", f.refreshes)
			}
			if !bytes.Equal(st.b, before) {
				t.Error("state changed")
			}
		})
	}
	// No session at all is needs_login too.
	_, cfg := newFake(t)
	if _, err := newTestProton(t, cfg).authed(t.Context(), &memStore{}, call{method: http.MethodGet, path: "/vpn/v2"}); !errors.Is(err, errNeedsLogin) {
		t.Fatalf("no session: %v", err)
	}
}

// A 401 on logout means the session is already closed.
func TestLogoutUnauthorized(t *testing.T) {
	f, cfg := newFake(t)
	st := &memStore{}
	p := newTestProton(t, cfg)
	if _, err := login(t, p, st, "", fakeUser, fakePassword); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.logout401 = true
	f.mu.Unlock()
	if err := p.Logout(t.Context(), st); err != nil {
		t.Fatal(err)
	}
	if st.session(t) != (tokens{}) {
		t.Error("session kept")
	}
	for _, r := range f.requests() {
		if r.Method == http.MethodDelete && r.Path != "/auth" {
			t.Errorf("%s %s", r.Method, r.Path)
		}
	}
}

// The password, the TOTP code and the tokens never appear in the terminal
// output, in errors or in a needs_login status.
func TestNoSecretsLeak(t *testing.T) {
	var texts []string
	f, cfg := newFake(t)
	f.twoFA = true
	st := &memStore{}
	p := newTestProton(t, cfg)
	out, err := login(t, p, st, "", fakeUser, fakePassword, fakeTOTP)
	if err != nil {
		t.Fatal(err)
	}
	texts = append(texts, out)
	for _, lines := range [][]string{{fakeUser, fakePassword + "x"}, {fakeUser, fakePassword, "999999"}} {
		out, err := login(t, newTestProton(t, cfg), &memStore{}, "", lines...)
		texts = append(texts, out, fmt.Sprint(err))
	}
	f.expire()
	f.mu.Lock()
	f.refreshFail = [2]int{422, codeRefreshInvalid}
	f.mu.Unlock()
	_, err = p.authed(t.Context(), st, call{method: http.MethodGet, path: "/vpn/v2"})
	status, _ := json.Marshal(wgpstatus.Provider{ID: "pv", State: wgpstatus.StateNeedsLogin, Error: err.Error()})
	texts = append(texts, err.Error(), string(status), fmt.Sprintf("%v %+v", p, p))
	for _, s := range texts {
		for _, secret := range []string{fakePassword, fakeTOTP, "999999", "tok-access", "tok-refresh"} {
			if strings.Contains(s, secret) {
				t.Errorf("%q leaks %q", s, secret)
			}
		}
	}
}
