package proton

import (
	"bytes"
	"context"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/a1ekseev/scd/internal/wgp/config"
	"github.com/a1ekseev/scd/internal/wgp/daemon"
	"github.com/a1ekseev/scd/internal/wgp/provider"
	"github.com/a1ekseev/scd/internal/wgp/server"
)

// logDaemon returns a wgp daemon of p logging JSON at debug into buf and a
// function that runs one tick and returns /api/status and status.json.
func logDaemon(t *testing.T, p *Proton, s provider.Store, dir string, buf *bytes.Buffer) func() []string {
	t.Helper()
	cfg := config.Config{
		Providers: []config.Provider{{ID: "proton", Type: Type, Enabled: true, Schedule: "@yearly", Impl: p}},
		Outputs:   []config.Output{{ID: "proton", Path: filepath.Join(dir, "proton"), Provider: "proton"}},
	}
	d, err := daemon.New(cfg, func(string) provider.Store { return s }, slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	if err != nil {
		t.Fatal(err)
	}
	return func() []string {
		t.Helper()
		var before time.Time
		if st := d.Status(); len(st.Providers) > 0 {
			before = st.Providers[0].LastTickAt
		}
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() { d.Run(ctx); close(done) }()
		for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
			if st := d.Status(); len(st.Providers) > 0 && st.Providers[0].LastTickAt != before {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("no tick")
			}
		}
		cancel()
		<-done
		rec := httptest.NewRecorder()
		server.New(d.Status).ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), "GET", "/api/status", nil))
		file, _ := os.ReadFile(filepath.Join(dir, "status.json"))
		return []string{rec.Body.String(), string(file)}
	}
}

// logRecords decodes the JSON log lines of buf.
func logRecords(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for line := range strings.Lines(buf.String()) {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatal(err)
		}
		out = append(out, m)
	}
	return out
}

// TestLogEvents runs 401 → refresh → issue → rotation → revoke → server
// change → renew → needs_login through the wgp daemon. It checks the
// provider events, that needs_login is one error line and then debug, and
// that no secret of the fake API reaches the log, status.json or
// /api/status.
func TestLogEvents(t *testing.T) {
	f, cfg := newFake(t)
	start := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	c := &clock{start}
	f.now = c.now
	f.logicals = []map[string]any{
		srv("la", "CH#1", "Zurich", "CH", 0, 0, 10, 1, phys("phys-alpha", "192.0.2.11", 1)),
		srv("lb", "CH#2", "Zurich", "CH", 0, 0, 10, 2, phys("phys-bravo", "192.0.2.12", 1)),
		srv("lc", "CH#3", "Zurich", "CH", 0, 0, 10, 3, phys("phys-charlie", "192.0.2.13", 1)),
	}
	st := signedIn(t, f)
	dir := t.TempDir()
	var buf bytes.Buffer
	var seen []string

	f.expire() // the first call gets a 401 and refreshes the session
	tick := logDaemon(t, newTick(t, cfg, c, 2, nil), st, dir, &buf)
	seen = append(seen, tick()...) // issue
	c.t = start.Add(time.Minute)
	tick = logDaemon(t, newTick(t, cfg, c, 2, nil), st, dir, &buf)
	seen = append(seen, tick()...) // rotation
	c.t = start.Add(32 * time.Minute)
	f.mu.Lock()
	f.logicals[0]["Status"] = 0                  // CH#1 goes offline
	f.logicals = slices.Delete(f.logicals, 1, 2) // CH#2 leaves the list
	f.mu.Unlock()
	seen = append(seen, tick()...) // revoke, server change
	c.t = start.Add(19 * time.Hour)
	seen = append(seen, tick()...) // renew
	f.expire()
	f.mu.Lock()
	f.refreshFail = [2]int{422, codeRefreshInvalid}
	f.mu.Unlock()
	seen = append(seen, tick()...) // needs_login
	seen = append(seen, tick()...) // needs_login again

	recs := logRecords(t, &buf)
	var got []string
	for _, r := range recs {
		ev := r["event"].(string)
		switch ev {
		case "provider_warning", "provider_error", "output_published", "provider_recovered":
			continue
		}
		var s strings.Builder
		fmt.Fprintf(&s, "%s %s", r["level"], ev)
		for _, k := range []string{"slot", "mode", "gen", "fromServer", "toServer", "reasonCode", "detail", "count", "alreadyRevoked", "state"} {
			if v, ok := r[k]; ok {
				fmt.Fprintf(&s, " %s=%v", k, v)
			}
		}
		got = append(got, s.String())
		if r["providerId"] != "proton" || r["providerType"] != Type || r["outputId"] != "proton" {
			t.Errorf("ids of %v", r)
		}
	}
	want := []string{
		"INFO session_refreshed",
		"INFO certificate_issued slot=1 mode=new gen=1",
		"INFO certificate_issued slot=2 mode=new gen=1",
		"INFO candidate_server_changed slot=1 fromServer= toServer=CH#1 reasonCode=vacant",
		"INFO candidate_server_changed slot=2 fromServer= toServer=CH#2 reasonCode=vacant",
		"INFO keys_rotated gen=2",
		"INFO certificate_issued slot=1 mode=rotation gen=2",
		"INFO certificate_issued slot=2 mode=rotation gen=2",
		"INFO candidate_server_changed slot=1 fromServer=CH#1 toServer=CH#3 reasonCode=gone detail=offline",
		"INFO candidate_server_changed slot=2 fromServer= toServer= reasonCode=gone detail=missing",
		"INFO certificates_revoked gen=2 count=2 alreadyRevoked=0",
		"INFO certificate_issued slot=1 mode=renew gen=2",
		"INFO certificate_issued slot=2 mode=renew gen=2",
		"ERROR provider_tick_failed state=needs_login",
		"DEBUG provider_tick_failed state=needs_login",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("log\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if r := recs[len(recs)-1]; r["error"] != errNeedsLogin.Error() {
		t.Errorf("needs_login error %q", r["error"])
	}

	ks, err := newTestProton(t, cfg).loadKeys(st)
	if err != nil {
		t.Fatal(err)
	}
	secrets := []string{
		fakeOVPNName, fakeOVPNPass, "fake-uid-1", "tok-access-", "tok-refresh-", "serial-",
		"phys-alpha", "phys-bravo", "phys-charlie", "192.0.2.11", "192.0.2.12", "192.0.2.13",
	}
	for _, sl := range ks.Slots {
		sk, pub := wgKey(sl.Seed)
		fp := sha512.Sum512(pub[:])
		secrets = append(secrets, base64.StdEncoding.EncodeToString(sl.Seed), hex.EncodeToString(sl.Seed),
			base64.StdEncoding.EncodeToString(sk[:]), base64.StdEncoding.EncodeToString(fp[:]))
	}
	all := buf.String() + strings.Join(seen, "\n")
	for _, sec := range secrets {
		if strings.Contains(all, sec) {
			t.Errorf("secret %q in the log or status", sec)
		}
	}
}
