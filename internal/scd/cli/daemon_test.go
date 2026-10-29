package cli_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDaemonCommandRejectsRunOnceConfig: `scd daemon` needs runtime.mode
// daemon and fails before starting anything otherwise (03 §3.1).
func TestDaemonCommandRejectsRunOnceConfig(t *testing.T) {
	raw, err := os.ReadFile(exampleConfig)
	if err != nil {
		t.Fatal(err)
	}
	fixture, err := filepath.Abs("../../../docker/fixtures/subscription.txt")
	if err != nil {
		t.Fatal(err)
	}
	cfg := strings.NewReplacer("mode: daemon", "mode: run-once", "../fixtures/subscription.txt", fixture).Replace(string(raw))
	path := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	code, out, errs := run("daemon", "--config", path)

	if code != 1 {
		t.Fatalf("exit code = %d, want 1; stderr:\n%s", code, errs)
	}
	if out != "" {
		t.Errorf("stdout = %q, want empty", out)
	}
	if want := "config at " + path + " is not in daemon mode"; !strings.Contains(errs, want) {
		t.Errorf("stderr lacks %q:\n%s", want, errs)
	}
	// Logged once as service_failed, not printed again as text.
	if !strings.Contains(errs, "service_failed") || strings.Count(errs, "is not in daemon mode") != 1 {
		t.Errorf("stderr:\n%s", errs)
	}
	if strings.Contains(errs, "status_server_started") {
		t.Errorf("status server started:\n%s", errs)
	}
}
