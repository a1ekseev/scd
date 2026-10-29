package cli_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/a1ekseev/scd/internal/common/cliutil"
	"github.com/a1ekseev/scd/internal/sfc/cli"
)

// syncBuf is a concurrency-safe log sink.
type syncBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

// writeServeConfig writes a config serving the base64 fixture under /s in a
// temp dir (the source cache lands there too) and returns its path.
func writeServeConfig(t *testing.T, addr, schedule, include string) string {
	t.Helper()
	fixture, err := filepath.Abs("../../../docker/fixtures/subscription.b64")
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "config.yml")
	body := `subscriptions:
  - id: test-fixture
    input: ` + fixture + `
    pathRoute: /s
    outputs:
      - id: test-all
        labelIncludeRegex: '` + include + `'
runtime:
  refreshSchedule: "` + schedule + `"
server:
  listen: ` + addr + `
logging:
  level: info
  format: json
`
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func waitFor(t *testing.T, logs *syncBuf, needle string, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for strings.Count(logs.String(), needle) < n {
		if time.Now().After(deadline) {
			t.Fatalf("%d× %s not logged:\n%s", n, needle, logs.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func httpGet(t *testing.T, url string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(body)
}

func startServe(ctx context.Context, t *testing.T, cfg string) (chan error, *syncBuf) {
	t.Helper()
	logs := &syncBuf{}
	done := make(chan error, 1)
	go func() { done <- cli.RunServe(ctx, cfg, logs) }()
	return done, logs
}

func waitDone(t *testing.T, done chan error, limit time.Duration) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunServe: %v", err)
		}
	case <-time.After(limit):
		t.Fatalf("RunServe did not return within %s", limit)
	}
}

func TestServeInitialRefreshThenListen(t *testing.T) {
	addr := freeAddr(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done, logs := startServe(ctx, t, writeServeConfig(t, addr, "0 0 1 1 *", "/.*/"))

	waitFor(t, logs, `"event":"server_started"`, 1)
	code, body := httpGet(t, "http://"+addr+"/s/test-all")
	if code != http.StatusOK || body == "" {
		t.Fatalf("GET /s/test-all = %d %q", code, body)
	}
	time.Sleep(300 * time.Millisecond) // a wrongly immediate cron tick would show up here
	cancel()
	waitDone(t, done, 5*time.Second)

	out := logs.String()
	if n := strings.Count(out, `"event":"subscription_refreshed"`); n != 1 {
		t.Fatalf("refresh ran %d times, want 1:\n%s", n, out)
	}
	if strings.Contains(out, `"event":"refresh_tick"`) {
		t.Fatalf("unexpected cron tick:\n%s", out)
	}
	last := -1
	for _, ev := range []string{`"event":"config_loaded"`, `"event":"subscription_refreshed"`, `"event":"server_started"`} {
		i := strings.Index(out, ev)
		if i <= last {
			t.Fatalf("event %s missing or out of order:\n%s", ev, out)
		}
		last = i
	}
	for _, want := range []string{
		`"msg":"Config loaded.","event":"config_loaded","configPath":"`,
		`"msg":"Subscription server started.","event":"server_started","listen":"` + addr + `"`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %s in:\n%s", want, out)
		}
	}
}

func TestServeRefreshesOnSchedule(t *testing.T) {
	addr := freeAddr(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done, logs := startServe(ctx, t, writeServeConfig(t, addr, "@every 1s", "/.*/"))

	waitFor(t, logs, `"event":"server_started"`, 1)
	waitFor(t, logs, `"event":"subscription_refreshed"`, 2)
	cancel()
	waitDone(t, done, 5*time.Second)
	out := logs.String()
	// refresh_tick is debug; the scheduled refresh is the second
	// subscription_refreshed, after the server started.
	if strings.Contains(out, `"event":"refresh_tick"`) {
		t.Fatalf("refresh_tick at info:\n%s", out)
	}
	if strings.LastIndex(out, `"event":"subscription_refreshed"`) < strings.Index(out, `"event":"server_started"`) {
		t.Fatalf("cron tick before server start:\n%s", out)
	}
	for _, want := range []string{`"version":"dev"`, `"event":"server_stopping"`, `"signal":"context canceled"`} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %s in:\n%s", want, out)
		}
	}
}

func TestServeStopsImmediately(t *testing.T) {
	addr := freeAddr(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done, logs := startServe(ctx, t, writeServeConfig(t, addr, "0 0 1 1 *", "/.*/"))
	waitFor(t, logs, `"event":"server_started"`, 1)

	start := time.Now()
	cancel()
	waitDone(t, done, 5*time.Second)
	if d := time.Since(start); d >= time.Second {
		t.Fatalf("stop took %s", d)
	}
}

func TestServeStopCancelsAndWaitsForActiveRefresh(t *testing.T) {
	fixture, err := os.ReadFile("../../../docker/fixtures/subscription.b64")
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) > 1 {
			<-r.Context().Done() // the scheduled refresh hangs until cancelled
			return
		}
		_, _ = w.Write(fixture)
	}))
	defer upstream.Close()

	addr := freeAddr(t)
	cfg := writeServeConfig(t, addr, "@every 1s", "/.*/")
	raw, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	abs, err := filepath.Abs("../../../docker/fixtures/subscription.b64")
	if err != nil {
		t.Fatal(err)
	}
	body := strings.Replace(string(raw), abs, upstream.URL, 1)
	body = strings.Replace(body, "level: info", "level: debug", 1)
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done, logs := startServe(ctx, t, cfg)
	waitFor(t, logs, `"event":"refresh_tick"`, 1)
	for calls.Load() < 2 {
		time.Sleep(10 * time.Millisecond)
	}
	start := time.Now()
	cancel()
	waitDone(t, done, 5*time.Second)
	if d := time.Since(start); d >= time.Second {
		t.Fatalf("stop took %s", d)
	}
	// The cancelled refresh finished before RunServe returned.
	if !strings.Contains(logs.String(), `"event":"refresh_tick_canceled"`) {
		t.Fatalf("active refresh not awaited:\n%s", logs.String())
	}
}

func TestServeBindErrorAfterInitialRefresh(t *testing.T) {
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	logs := &syncBuf{}
	err = cli.RunServe(t.Context(), writeServeConfig(t, ln.Addr().String(), "0 0 1 1 *", "/.*/"), logs)
	if err == nil || !strings.Contains(err.Error(), "address already in use") {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(logs.String(), `"event":"subscription_refreshed"`) ||
		strings.Contains(logs.String(), `"event":"server_started"`) || !strings.Contains(logs.String(), `"event":"service_failed"`) ||
		!errors.Is(err, cliutil.ErrLogged) {
		t.Fatalf("logs:\n%s", logs.String())
	}
}

func TestServeConfigError(t *testing.T) {
	logs := &syncBuf{}
	err := cli.RunServe(t.Context(), "nope.yml", logs)
	if err == nil || !strings.Contains(err.Error(), "failed to read config nope.yml") || logs.String() != "" {
		t.Fatalf("err = %v, logs %q", err, logs.String())
	}
}

func TestRefreshCommandExitCode(t *testing.T) {
	for _, tc := range []struct {
		name, include string
		code          int
		failed        int
	}{
		{"ok", "/.*/", 0, 0},
		{"empty filter", "/no-such-label/", 1, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := writeServeConfig(t, freeAddr(t), "0 0 1 1 *", tc.include)
			code, out, errs := run("refresh", "--config", cfg)
			if code != tc.code {
				t.Fatalf("code %d, want %d; stderr %s", code, tc.code, errs)
			}
			var rep struct {
				SourceCount int `json:"sourceCount"`
				Failed      int `json:"failed"`
				Outputs     []struct {
					ID string `json:"id"`
				} `json:"outputs"`
			}
			if err := json.Unmarshal([]byte(out), &rep); err != nil {
				t.Fatalf("stdout is not JSON: %v\n%s", err, out)
			}
			if rep.Failed != tc.failed || rep.SourceCount != 1 || len(rep.Outputs) != 1 || rep.Outputs[0].ID != "<redacted>" {
				t.Fatalf("report %s", out)
			}
			if !strings.HasPrefix(out, "{\n  \"refreshedAt\": ") || !strings.HasSuffix(out, "}\n") {
				t.Fatalf("layout %q", out)
			}
			// Logs go to stderr; the report-failed sentinel prints nothing extra.
			if !strings.Contains(errs, `"event":"subscription_refreshed"`) || strings.Contains(errs, cliutil.ErrReportFailed.Error()) {
				t.Fatalf("stderr %s", errs)
			}
		})
	}
}

func TestRefreshReturnsErrReportFailed(t *testing.T) {
	cfg := writeServeConfig(t, freeAddr(t), "0 0 1 1 *", "/no-such-label/")
	var out, errb syncBuf
	err := cli.RunRefresh(t.Context(), cfg, &out, &errb)
	if !errors.Is(err, cliutil.ErrReportFailed) {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(out.String(), `"failed": 1`) {
		t.Fatalf("stdout %s", out.String())
	}
}
