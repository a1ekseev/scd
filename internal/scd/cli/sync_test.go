package cli_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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
