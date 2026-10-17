package cli_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"

	rootcli "github.com/a1ekseev/scd/internal/cli"
)

const (
	exampleConfig = "../../../docker/sfc/config.example.yml"
	realShapeGo   = "../../../docker/sfc/config.real-shape.go.yml"
	referenceJSON = "../../../testdata/reference/sfc/print-config-docker-example.json"
)

func run(args ...string) (code int, stdout, stderr string) {
	var out, errb bytes.Buffer
	code = rootcli.Execute(append([]string{"sfc"}, args...), &out, &errb)
	return code, out.String(), errb.String()
}

func TestPrintConfigDockerExample(t *testing.T) {
	code, out, errs := run("print-config", "--config", exampleConfig)
	if code != 0 || errs != "" {
		t.Fatalf("code %d: %s", code, errs)
	}
	repo, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	sub := got["subscriptions"].([]any)[0].(map[string]any)
	sub["input"] = strings.TrimPrefix(sub["input"].(string), repo+string(filepath.Separator))

	raw, err := os.ReadFile(referenceJSON)
	if err != nil {
		t.Fatal(err)
	}
	var want map[string]any
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	// D29/D31: TS printed labelIncludeRegex as a string, Go prints a list.
	for _, o := range want["subscriptions"].([]any)[0].(map[string]any)["outputs"].([]any) {
		m := o.(map[string]any)
		m["labelIncludeRegex"] = []any{m["labelIncludeRegex"]}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %s\nwant %s", out, raw)
	}
	if !strings.HasPrefix(out, "{\n  \"subscriptions\": [") || !strings.HasSuffix(out, "}\n") {
		t.Fatalf("unexpected layout: %q", out)
	}
}

func TestValidateConfigOutput(t *testing.T) {
	for _, p := range []string{exampleConfig, realShapeGo} {
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

func TestMissingConfigFlag(t *testing.T) {
	for _, cmd := range []string{"validate-config", "print-config"} {
		code, out, errs := run(cmd)
		if code != 1 || out != "" || !strings.Contains(errs, "config") {
			t.Fatalf("%s: code %d out %q err %q", cmd, code, out, errs)
		}
	}
}

func TestInvalidConfigFails(t *testing.T) {
	code, out, errs := run("validate-config", "--config", "nope.yml")
	if code != 1 || out != "" || !strings.Contains(errs, "failed to read config nope.yml") {
		t.Fatalf("code %d out %q err %q", code, out, errs)
	}
}

func TestPrintConfigRedactsFetchHeaders(t *testing.T) {
	t.Setenv("HW", "s3cr3t")
	p := filepath.Join(t.TempDir(), "config.yml")
	body := `subscriptions:
  - id: main
    input: https://sub.example.com/x
    pathRoute: /s
    fetchHeaders: {x-hwid: "${HW}"}
    outputs:
      - id: out
        labelExcludeRegex: x
runtime:
  refreshSchedule: "@hourly"
server:
  listen: :8080
`
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, errs := run("print-config", "--config", p)
	if code != 0 || !strings.Contains(out, "\"fetchHeaders\": {\n        \"x-hwid\": \"<redacted>\"\n      }") ||
		strings.Contains(out, "s3cr3t") {
		t.Fatalf("code %d out %s err %s", code, out, errs)
	}
}

// Final review I2: `scd sfc refresh` prints no subscription URL token to
// stdout (report) or stderr (logs).
func TestRefreshDoesNotPrintSubscriptionURL(t *testing.T) {
	const secret, qsecret = "PATHSECRET123", "QUERYSECRET456"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	p := filepath.Join(t.TempDir(), "config.yml")
	body := `subscriptions:
  - id: main
    input: ` + srv.URL + `/sub/` + secret + `?token=` + qsecret + `
    pathRoute: /s
    outputs:
      - id: out
        labelExcludeRegex: x
runtime:
  refreshSchedule: "@hourly"
server:
  listen: :8080
`
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, errs := run("refresh", "--config", p)
	if code == 0 || !strings.Contains(out, "/<redacted>") {
		t.Fatalf("code %d out %s err %s", code, out, errs)
	}
	for _, s := range []string{secret, qsecret} {
		if strings.Contains(out, s) || strings.Contains(errs, s) {
			t.Errorf("output contains %s:\nstdout %s\nstderr %s", s, out, errs)
		}
	}
}

// Final review I3: print-config redacts the subscription input and the
// fetchHeaders values unless --include-secrets is given.
func TestPrintConfigSecrets(t *testing.T) {
	t.Setenv("HW", "HEADERSECRET")
	p := filepath.Join(t.TempDir(), "config.yml")
	body := `subscriptions:
  - id: main
    input: https://sub.example.com/sub/PATHTOKEN?token=QUERYTOKEN
    pathRoute: /s
    fetchHeaders: {x-hwid: "${HW}"}
    outputs:
      - id: out
        labelExcludeRegex: x
runtime:
  refreshSchedule: "@hourly"
server:
  listen: :8080
`
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	secrets := []string{"PATHTOKEN", "QUERYTOKEN", "HEADERSECRET"}

	code, out, errs := run("print-config", "--config", p)
	if code != 0 || !strings.Contains(out, `"input": "https://sub.example.com/<redacted>"`) {
		t.Fatalf("code %d out %s err %s", code, out, errs)
	}
	for _, s := range secrets {
		if strings.Contains(out+errs, s) {
			t.Errorf("print-config leaks %s:\n%s", s, out)
		}
	}

	code, raw, errs := run("print-config", "--config", p, "--include-secrets")
	if code != 0 {
		t.Fatalf("code %d err %s", code, errs)
	}
	for _, s := range secrets {
		if !strings.Contains(raw, s) {
			t.Errorf("print-config --include-secrets lacks %s:\n%s", s, raw)
		}
	}
	keyRe := regexp.MustCompile(`(?m)^\s*"([^"]+)":`)
	if a, b := keyRe.FindAllString(out, -1), keyRe.FindAllString(raw, -1); !slices.Equal(a, b) {
		t.Errorf("key order differs:\n%v\n%v", a, b)
	}
}

// Final review M3: `scd sfc refresh` handles SIGTERM as a cancellation and
// still prints its report, without refreshing the remaining subscriptions
// or failing the one cut short.
func TestRefreshStopsOnSignal(t *testing.T) {
	var loadedB atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/b" {
			loadedB.Store(true)
		}
		_ = syscall.Kill(syscall.Getpid(), syscall.SIGTERM)
		<-r.Context().Done()
	}))
	defer srv.Close()
	p := filepath.Join(t.TempDir(), "config.yml")
	body := `subscriptions:
  - id: a
    input: ` + srv.URL + `/a
    pathRoute: /a
    outputs:
      - id: out-a
        labelExcludeRegex: x
  - id: b
    input: ` + srv.URL + `/b
    pathRoute: /b
    outputs:
      - id: out-b
        labelExcludeRegex: x
runtime:
  refreshSchedule: "@hourly"
server:
  listen: :8080
`
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	_, out, errs := run("refresh", "--config", p)
	var rep struct {
		Outputs []struct {
			ID string `json:"id"`
		} `json:"outputs"`
	}
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("stdout is not one JSON report: %v\n%s\n%s", err, out, errs)
	}
	if len(rep.Outputs) != 0 || loadedB.Load() {
		t.Errorf("outputs = %+v, b loaded %v; want none", rep.Outputs, loadedB.Load())
	}
	if !strings.Contains(errs, "subscription_refresh_canceled") {
		t.Errorf("stderr lacks subscription_refresh_canceled:\n%s", errs)
	}
}
