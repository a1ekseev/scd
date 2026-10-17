package cli_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	rootcli "github.com/a1ekseev/scd/internal/cli"
)

const (
	exampleConfig = "../../../docker/scd/config.example.yml"
	realShapeGo   = "../../../docker/scd/config.real-shape.go.yml"
	refDir        = "../../../testdata/reference/scd/print-config/"
)

func run(args ...string) (code int, stdout, stderr string) {
	var out, errb bytes.Buffer
	code = rootcli.Execute(args, &out, &errb)
	return code, out.String(), errb.String()
}

func readJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// addNewKeys adds the Go-only defaults (D15, D22) to every subscription of
// a TS reference.
func addNewKeys(want map[string]any) {
	for _, s := range want["subscriptions"].([]any) {
		sub := s.(map[string]any)
		sub["allowInsecure"] = "deny"
		sub["target"].(map[string]any)["balancerSelection"] = map[string]any{"enabled": false, "observatory": true}
	}
}

// comparePrintConfig runs print-config on config and compares it with the
// TS reference (plus the new keys, then edit applied to the first
// subscription); input is compared by suffix.
func comparePrintConfig(t *testing.T, config, reference string, edit ...func(sub map[string]any)) string {
	t.Helper()
	code, out, errs := run("print-config", "--config", config)
	if code != 0 || errs != "" {
		t.Fatalf("code %d: %s", code, errs)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	want := readJSON(t, reference)
	addNewKeys(want)
	gs := got["subscriptions"].([]any)[0].(map[string]any)
	ws := want["subscriptions"].([]any)[0].(map[string]any)
	for _, e := range edit {
		e(ws)
	}
	gotInput, wantInput := gs["input"].(string), ws["input"].(string)
	if !filepath.IsAbs(gotInput) || !strings.HasSuffix(gotInput, string(filepath.Separator)+wantInput) {
		t.Fatalf("input %q, want suffix %q", gotInput, wantInput)
	}
	gs["input"] = wantInput
	if !reflect.DeepEqual(got, want) {
		w, _ := json.MarshalIndent(want, "", "  ")
		t.Fatalf("got %s\nwant %s", out, w)
	}
	if !strings.HasPrefix(out, "{\n  \"subscriptions\": [") || !strings.HasSuffix(out, "}\n") {
		t.Fatalf("unexpected layout: %q", out)
	}
	return out
}

func TestPrintConfigMinimal(t *testing.T) {
	out := comparePrintConfig(t, refDir+"minimal.yml", refDir+"minimal.json")
	if strings.Contains(out, "fetchUserAgent") || strings.Contains(out, "fetchHeaders") {
		t.Fatalf("optional keys printed: %s", out)
	}
	// Key order of a subscription and its target (01 §2.6 + new keys).
	order := []string{
		`"id"`, `"input"`, `"enabled"`, `"format"`, `"fetchTimeoutMs"`, `"allowInsecure"`, `"target"`,
		`"address"`, `"timeoutMs"`, `"fixedOutbounds"`, `"visionUdp443Override"`, `"monitor"`, `"balancerMonitor"`, `"balancerSelection"`,
		`"runtime"`, `"logging"`, `"resources"`, `"statusServer"`,
	}
	pos := 0
	for _, k := range order {
		i := strings.Index(out[pos:], k)
		if i < 0 {
			t.Fatalf("key %s out of order in %s", k, out)
		}
		pos += i
	}
}

func TestPrintConfigDockerExample(t *testing.T) {
	// The example enables the balancer selection (task 6.6, D22).
	comparePrintConfig(t, exampleConfig, refDir+"docker-example.json", func(sub map[string]any) {
		sub["target"].(map[string]any)["balancerSelection"] = map[string]any{
			"enabled": true, "balancerTag": "x-balancer", "schedule": "*/15 * * * * *", "observatory": true,
		}
	})
}

func TestValidateConfigJSON(t *testing.T) {
	for _, p := range []string{exampleConfig, realShapeGo, refDir + "minimal.yml"} {
		abs, err := filepath.Abs(p)
		if err != nil {
			t.Fatal(err)
		}
		code, out, errs := run("validate-config", "--config", p)
		want := "{\n  \"configPath\": \"" + abs + "\",\n  \"valid\": true\n}\n"
		if code != 0 || out != want || errs != "" {
			t.Fatalf("%s: code %d out %q err %q", p, code, out, errs)
		}
	}
}

func TestSCDRealShapeGoFormValid(t *testing.T) {
	code, out, errs := run("validate-config", "--config", realShapeGo)
	if code != 0 || !strings.Contains(out, `"valid": true`) || errs != "" {
		t.Fatalf("code %d out %q err %q", code, out, errs)
	}
	// The TS form is rejected with an ordinary regex error (D29, D31).
	code, _, errs = run("validate-config", "--config", "../../../docker/scd/config.real-shape.yml")
	if code != 1 || !strings.Contains(errs, "subscriptions.0.filters.labelIncludeRegex: lookahead") {
		t.Fatalf("TS form: code %d err %q", code, errs)
	}
}

func TestValidateConfigWarnings(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yml")
	body := `subscriptions:
  - id: s1
    input: ./sub.txt
    target:
      address: 127.0.0.1:8080
      balancerSelection: {enabled: true, balancerTag: x-balancer, schedule: "*/15 * * * * *"}
runtime: {}
logging: {}
resources: {}
`
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, errs := run("validate-config", "--config", p)
	want := "warning: subscriptions.0.target.balancerSelection: balancerSelection is enabled without observatorySubjectSelectorPrefix; scd tunnels are unlikely to match the balancer selector\n"
	if code != 0 || !strings.Contains(out, `"valid": true`) || errs != want {
		t.Fatalf("code %d out %q err %q", code, out, errs)
	}
}

func TestConfigCommandErrors(t *testing.T) {
	for _, cmd := range []string{"validate-config", "print-config"} {
		code, out, errs := run(cmd)
		if code != 1 || out != "" || !strings.Contains(errs, "config") {
			t.Fatalf("%s: code %d out %q err %q", cmd, code, out, errs)
		}
	}
	code, out, errs := run("validate-config", "--config", "nope.yml")
	if code != 1 || out != "" || !strings.HasPrefix(errs, "failed to read config nope.yml: ") {
		t.Fatalf("code %d out %q err %q", code, out, errs)
	}
	p := filepath.Join(t.TempDir(), "bad.yml")
	if err := os.WriteFile(p, []byte("runtime: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, errs = run("validate-config", "--config", p)
	if code != 1 || out != "" || !strings.HasPrefix(errs, "invalid config "+p+":\n  - subscriptions: required") {
		t.Fatalf("code %d out %q err %q", code, out, errs)
	}
}

func TestPrintConfigRedactsFetchHeaders(t *testing.T) {
	t.Setenv("SCD_TEST_HWID", "s3cr3t")
	p := filepath.Join(t.TempDir(), "config.yml")
	body := `subscriptions:
  - id: s1
    input: https://sub.example.com/x
    fetchHeaders: {x-hwid: "${SCD_TEST_HWID}"}
    target:
      address: 127.0.0.1:8080
runtime: {}
logging: {}
resources: {}
`
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, errs := run("print-config", "--config", p)
	if code != 0 || !strings.Contains(out, "\"fetchHeaders\": {\n        \"x-hwid\": \"<redacted>\"\n      }") ||
		strings.Contains(out+errs, "s3cr3t") {
		t.Fatalf("code %d out %s err %s", code, out, errs)
	}
	code, out, errs = run("validate-config", "--config", p)
	if code != 0 || strings.Contains(out+errs, "s3cr3t") {
		t.Fatalf("validate: code %d out %s err %s", code, out, errs)
	}
}
