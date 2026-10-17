package cli_test

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// TestSyncCommandExitCode: with Xray unreachable, sync prints a pure JSON
// report with failed items for every kind on stdout, logs to stderr and
// exits 1 (03 §2.4, Review Focus 2).
func TestSyncCommandExitCode(t *testing.T) {
	raw, err := os.ReadFile(exampleConfig)
	if err != nil {
		t.Fatal(err)
	}
	fixture, err := filepath.Abs("../../../docker/fixtures/subscription.txt")
	if err != nil {
		t.Fatal(err)
	}
	cfg := strings.NewReplacer("xray:8080", "127.0.0.1:1", "../fixtures/subscription.txt", fixture).Replace(string(raw))
	path := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	code, out, errs := run("sync", "--config", path)

	if code != 1 {
		t.Fatalf("exit code = %d, want 1; stderr:\n%s", code, errs)
	}
	var report struct {
		Failed  int `json:"failed"`
		Targets []struct {
			TargetAddress string `json:"targetAddress"`
			Resources     []struct {
				Kind  string `json:"kind"`
				Items []struct {
					Message string `json:"message"`
				} `json:"items"`
			} `json:"resources"`
		} `json:"targets"`
	}
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("stdout is not one JSON report: %v\n%s", err, out)
	}
	if report.Failed == 0 || len(report.Targets) != 1 || len(report.Targets[0].Resources) != 3 {
		t.Fatalf("report = %+v", report)
	}
	for _, r := range report.Targets[0].Resources {
		if len(r.Items) == 0 || !strings.Contains(r.Items[len(r.Items)-1].Message, "Unable to connect to Xray API at 127.0.0.1:1") {
			t.Errorf("%s items = %+v", r.Kind, r.Items)
		}
	}
	for _, ev := range []string{"event=config_loaded", "event=sync_started", "event=apply_failed"} {
		if !strings.Contains(errs, ev) {
			t.Errorf("stderr lacks %s:\n%s", ev, errs)
		}
	}
	if strings.Contains(errs, "report has failed items") {
		t.Errorf("stderr has the ErrReportFailed text:\n%s", errs)
	}
}

// Final review M3: `scd sync` handles SIGTERM as a cancellation (the apply in
// flight would finish, nothing new starts) and still prints its report,
// instead of being killed by the default signal handling.
func TestSyncCommandStopsOnSignal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		_ = syscall.Kill(syscall.Getpid(), syscall.SIGTERM)
		<-r.Context().Done()
	}))
	defer srv.Close()
	body := `subscriptions:
  - id: s1
    input: ` + srv.URL + `/sub
    target: {address: 127.0.0.1:1}
  - id: s2
    input: ` + srv.URL + `/sub2
    target: {address: 127.0.0.1:2}
runtime: {}
logging: {}
resources: {}
`
	path := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	code, out, errs := run("sync", "--config", path)

	var report struct {
		Targets []struct {
			SubscriptionID string `json:"subscriptionId"`
		} `json:"targets"`
	}
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("stdout is not one JSON report: %v\n%s\n%s", err, out, errs)
	}
	for _, tg := range report.Targets {
		if tg.SubscriptionID == "s2" {
			t.Errorf("s2 was synced after the signal: %s", out)
		}
	}
	// Re-review R1: an interrupted run is not a success.
	assertInterrupted(t, code, errs, "load")
}

// assertInterrupted checks exit code 143 (SIGTERM) and exactly one warn
// sync_canceled event with the phase.
func assertInterrupted(t *testing.T, code int, errs, phase string) {
	t.Helper()
	if code != 143 {
		t.Errorf("exit code = %d, want 143\n%s", code, errs)
	}
	var events []map[string]any
	for line := range strings.Lines(errs) {
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) == nil && m["event"] == "sync_canceled" {
			events = append(events, m)
		}
	}
	if len(events) != 1 || events[0]["level"] != "WARN" || events[0]["phase"] != phase || events[0]["signal"] != "SIGTERM" {
		t.Errorf("sync_canceled events = %v, want one WARN with phase %q\n%s", events, phase, errs)
	}
}

// Re-review R1: SIGTERM while the first target is applied skips the second
// target, prints the report and exits 143 with sync_canceled phase targets.
func TestSyncCommandStopsOnSignalBetweenTargets(t *testing.T) {
	sub, err := filepath.Abs("../../../docker/fixtures/subscription.txt")
	if err != nil {
		t.Fatal(err)
	}
	// Target A's "Xray" sends SIGTERM on the first connection and closes it.
	lnA, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer lnA.Close()
	go func() {
		for {
			c, err := lnA.Accept()
			if err != nil {
				return
			}
			_ = syscall.Kill(syscall.Getpid(), syscall.SIGTERM)
			time.Sleep(100 * time.Millisecond)
			_ = c.Close()
		}
	}()
	lnB, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer lnB.Close()
	var dialedB atomic.Bool
	go func() {
		for {
			c, err := lnB.Accept()
			if err != nil {
				return
			}
			dialedB.Store(true)
			_ = c.Close()
		}
	}()
	body := `subscriptions:
  - id: s1
    input: ` + sub + `
    target: {address: "` + lnA.Addr().String() + `", timeoutMs: 1000}
  - id: s2
    input: ` + sub + `
    target: {address: "` + lnB.Addr().String() + `", timeoutMs: 1000}
runtime: {}
logging: {}
resources: {}
`
	path := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	code, out, errs := run("sync", "--config", path)

	if !json.Valid([]byte(out)) || !strings.Contains(out, `"subscriptionId": "s1"`) || strings.Contains(out, `"subscriptionId": "s2"`) {
		t.Errorf("report:\n%s", out)
	}
	if dialedB.Load() {
		t.Error("target B was contacted after the signal")
	}
	assertInterrupted(t, code, errs, "targets")
}
