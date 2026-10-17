package cli_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
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

	_, out, errs := run("sync", "--config", path)

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
}
