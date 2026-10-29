package warp

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"log/slog"
	"net/http"
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

// logRun runs a daemon of w until its first tick is in the status (or until
// stop is called by the fake), logging JSON at debug into buf; it returns
// status.json and /api/status.
func logRun(t *testing.T, w *WARP, s provider.Store, dir string, buf *bytes.Buffer, f *fakeAPI) []string {
	t.Helper()
	cfg := config.Config{
		Providers: []config.Provider{{ID: "warp", Type: Type, Enabled: true, Schedule: "@yearly", Impl: w}},
		Outputs:   []config.Output{{ID: "warp", Path: filepath.Join(dir, "warp"), Provider: "warp"}},
	}
	d, err := daemon.New(cfg, func(string) provider.Store { return s }, slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	if err != nil {
		t.Fatal(err)
	}
	before := time.Time{}
	if st := d.Status(); len(st.Providers) > 0 {
		before = st.Providers[0].LastTickAt
	}
	ctx, cancel := context.WithCancel(t.Context())
	f.with(func() {
		if f.onReg != nil {
			f.onReg = cancel
		}
	})
	done := make(chan struct{})
	go func() { d.Run(ctx); close(done) }()
	for deadline := time.Now().Add(5 * time.Second); ctx.Err() == nil; time.Sleep(5 * time.Millisecond) {
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

// recordSecrets returns every secret of the state record of s.
func recordSecrets(t *testing.T, s *memStore) []string {
	t.Helper()
	var rec record
	if err := json.Unmarshal(s.b, &rec); err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, d := range rec.Devices {
		if d.ID == "" {
			continue
		}
		out = append(out, d.ID, d.Token, d.Profile.ClientID, d.Profile.PeerKey, d.Profile.V4, d.Profile.V6)
		if raw, err := base64.StdEncoding.DecodeString(d.Profile.ClientID); err == nil {
			out = append(out, string(raw))
		}
		if d.PrivateKey != nil {
			out = append(out, base64.StdEncoding.EncodeToString(d.PrivateKey), base64.RawURLEncoding.EncodeToString(d.PrivateKey))
		}
		if d.MasqueKey != nil {
			pemKey := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: d.MasqueKey}))
			out = append(out, base64.StdEncoding.EncodeToString(d.MasqueKey), base64.RawURLEncoding.EncodeToString(d.MasqueKey), strings.Split(pemKey, "\n")[1])
		}
	}
	return out
}

// TestLogEvents runs register → rotate (X25519 and P-256) → lost →
// re-register → cancel through the wgp daemon and checks the provider
// events and that no secret of the fake API reaches the log, status.json
// or /api/status.
func TestLogEvents(t *testing.T) {
	f := newFake(t)
	c := &clock{time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)}
	dir := t.TempDir()
	var buf bytes.Buffer
	var seen []string
	secrets := map[string]bool{
		"172.16.0.2": true, "2606:4700:110:8a36::2": true, "162.159.192.1": true, "162.159.198.2": true,
		"2606:4700:103::2": true, "bmXOC+F1FxEMF9dyiK2H5/1SUtzH0JuVo51h2wPfgyo=": true, strings.Split(dummyPEM, "\n")[1]: true,
	}
	run := func(w *WARP, s *memStore) {
		seen = append(seen, logRun(t, w, s, dir, &buf, f)...)
		for _, sec := range recordSecrets(t, s) {
			secrets[sec] = true
		}
	}

	wg := &memStore{}
	rot := func(w *WARP) { w.Keys.RotateOnStart = true; w.DeviceLostAfterChecks = 1 }
	run(newTest(f, c, 1, rot), wg) // register
	run(newTest(f, c, 1, rot), wg) // rotate X25519
	f.with(func() { f.get = []int{404} })
	c.t = c.t.Add(2 * time.Hour)
	run(newTest(f, c, 1, rot), wg) // lost, re-register
	f.with(func() { f.reg = []int{429} })
	f.with(func() { f.get = []int{404} })
	c.t = c.t.Add(2 * time.Hour)
	run(newTest(f, c, 1, rot), wg) // lost, registration fails
	c.t = c.t.Add(2 * time.Hour)
	f.with(func() { f.onReg = func() {} })
	run(newTest(f, c, 1, rot), wg) // cancelled during POST /reg
	f.with(func() { f.onReg = nil })

	mq := &memStore{}
	run(masqueTest(f, c, 1, nil), mq) // register, P-256
	run(masqueTest(f, c, 1, func(w *WARP) { w.Keys.RotateOnStart = true }), mq)

	var events []string
	for line := range strings.Lines(buf.String()) {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatal(err)
		}
		if ev := m["event"].(string); strings.HasPrefix(ev, "device_") {
			events = append(events, ev+" "+m["level"].(string))
			if m["providerId"] != "warp" || m["slot"] != 1.0 {
				t.Errorf("ids of %s", line)
			}
		}
		if m["event"] == "device_key_rotated" {
			events = append(events, m["keyType"].(string)+" "+m["reasonCode"].(string))
		}
	}
	want := []string{
		"device_registered INFO",
		"device_key_rotated INFO", "curve25519 rotate_on_start",
		"device_check_lost WARN", "device_lost WARN", "device_registered INFO",
		"device_check_lost WARN", "device_lost WARN", "device_register_failed WARN",
		// cancelled: nothing to check (the slot is empty), the POST /reg is cut
		"device_registered INFO", "device_key_rotated INFO", "secp256r1 new_device",
		"device_key_rotated INFO", "secp256r1 rotate_on_start",
	}
	if !slices.Equal(events, want) {
		t.Fatalf("events\n%q\nwant\n%q", events, want)
	}
	all := buf.String() + strings.Join(seen, "\n")
	for sec := range secrets {
		if sec != "" && strings.Contains(all, sec) {
			t.Errorf("secret %q in the log or status", sec)
		}
	}
	if len(secrets) < 20 {
		t.Fatalf("too few secrets: %d", len(secrets))
	}
}

// A tick cancelled during POST /reg still returns the events of the slot.
func TestEventsOnCancel(t *testing.T) {
	f := newFake(t)
	c := &clock{time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)}
	s := &memStore{}
	tick(t, newTest(f, c, 1, func(w *WARP) { w.DeviceLostAfterChecks = 1 }), s)
	ctx, cancel := context.WithCancel(t.Context())
	f.with(func() { f.get, f.onReg = []int{404}, cancel })
	res, err := newTest(f, c, 1, func(w *WARP) { w.DeviceLostAfterChecks = 1; w.ReRegisterMinIntervalMinutes = 0 }).Tick(ctx, s)
	if err == nil || ctx.Err() == nil || !slices.Equal(eventNames(res.Events), []string{"device_check_lost", "device_lost"}) {
		t.Fatalf("err %v, events %v", err, res.Events)
	}
}

// A failing POST /reg gives one provider_tick_failed error line per
// reRegisterMinIntervalMinutes: the ticks between two attempts report the
// same text as the attempt, and the next attempt a new one.
func TestRegisterFailureOneLinePerInterval(t *testing.T) {
	f := newFake(t)
	start := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	c := &clock{start}
	f.reg = []int{http.StatusTooManyRequests, http.StatusTooManyRequests}
	w := newTest(f, c, 1, nil)
	dir := t.TempDir()
	var buf bytes.Buffer
	cfg := config.Config{
		Providers: []config.Provider{{ID: "warp", Type: Type, Enabled: true, Schedule: "@yearly", Impl: w}},
		Outputs:   []config.Output{{ID: "warp", Path: filepath.Join(dir, "warp"), Provider: "warp"}},
	}
	s := &memStore{}
	d, err := daemon.New(cfg, func(string) provider.Store { return s }, slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	if err != nil {
		t.Fatal(err)
	}
	var texts []string
	for _, m := range []int{0, 10, 20, 59, 60, 70} { // attempts at 0 and 60
		c.t = start.Add(time.Duration(m) * time.Minute)
		var before time.Time
		if st := d.Status(); len(st.Providers) > 0 {
			before = st.Providers[0].LastTickAt
		}
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() { d.Run(ctx); close(done) }()
		for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
			if st := d.Status(); len(st.Providers) > 0 && st.Providers[0].LastTickAt != before {
				texts = append(texts, st.Providers[0].Error)
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("no tick")
			}
		}
		cancel()
		<-done
	}
	first := "device 1: register: status 429 (next attempt not before 2026-10-29T13:00:00Z)"
	second := "device 1: register: status 429 (next attempt not before 2026-10-29T14:00:00Z)"
	if want := []string{first, first, first, first, second, second}; !slices.Equal(texts, want) {
		t.Fatalf("errors\n%q\nwant\n%q", texts, want)
	}
	var levels []string
	for line := range strings.Lines(buf.String()) {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatal(err)
		}
		if m["event"] == "provider_tick_failed" || m["event"] == "provider_error" {
			levels = append(levels, m["level"].(string))
		}
	}
	if want := []string{"ERROR", "DEBUG", "DEBUG", "DEBUG", "ERROR", "DEBUG"}; !slices.Equal(levels, want) {
		t.Fatalf("levels %v, want %v", levels, want)
	}
}
