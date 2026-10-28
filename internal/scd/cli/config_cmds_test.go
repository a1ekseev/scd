package cli_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
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
// a TS reference and converts it to the sources form with one source
// "vless" (sourcesForm).
func addNewKeys(want map[string]any) {
	for _, s := range want["subscriptions"].([]any) {
		sub := s.(map[string]any)
		sub["allowInsecure"] = "deny"
		sub["target"].(map[string]any)["balancerSelection"] = map[string]any{"enabled": false, "observatory": true}
		sourcesForm(sub)
	}
}

// sourceKeys are the TS subscription keys that moved to the source.
var sourceKeys = []string{"input", "format", "fetchTimeoutMs", "allowInsecure", "fetchUserAgent", "fetchHeaders", "filters"}

// sourcesForm converts a printed TS subscription to the sources form: its
// source keys and target.inboundSocks.portRange move to one enabled source
// {id: vless, type: vless} with the default maxInputBytes.
func sourcesForm(sub map[string]any) {
	src := map[string]any{"id": "vless", "type": "vless", "enabled": true, "maxInputBytes": float64(64 << 20)}
	for _, k := range sourceKeys {
		if v, ok := sub[k]; ok {
			src[k] = v
			delete(sub, k)
		}
	}
	if socks, ok := sub["target"].(map[string]any)["inboundSocks"].(map[string]any); ok {
		src["portRange"] = socks["portRange"]
		delete(socks, "portRange")
	}
	sub["sources"] = []any{src}
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
	gsrc := gs["sources"].([]any)[0].(map[string]any)
	wsrc := ws["sources"].([]any)[0].(map[string]any)
	gotInput, wantInput := gsrc["input"].(string), wsrc["input"].(string)
	if !filepath.IsAbs(gotInput) || !strings.HasSuffix(gotInput, string(filepath.Separator)+wantInput) {
		t.Fatalf("input %q, want suffix %q", gotInput, wantInput)
	}
	gsrc["input"] = wantInput
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
	out := comparePrintConfig(t, refDir+"minimal.sources.yml", refDir+"minimal.json")
	if strings.Contains(out, "fetchUserAgent") || strings.Contains(out, "fetchHeaders") || strings.Contains(out, "portRange") {
		t.Fatalf("optional keys printed: %s", out)
	}
	// The Go-side reference, byte for byte, with the input made repo-relative.
	abs, err := filepath.Abs(refDir)
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(abs))))
	want, err := os.ReadFile(refDir + "minimal.sources.json")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Replace(out, root+string(filepath.Separator), "", 1); got != string(want) {
		t.Fatalf("got %s\nwant %s", got, want)
	}
	// Key order of a subscription, its source and its target (01 §2.6 + new keys).
	order := []string{
		`"id"`, `"enabled"`, `"sources"`, `"id"`, `"type"`, `"input"`, `"enabled"`, `"format"`, `"fetchTimeoutMs"`,
		`"allowInsecure"`, `"maxInputBytes"`, `"target"`,
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
	for _, p := range []string{exampleConfig, realShapeGo, refDir + "minimal.sources.yml"} {
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
	// The TS form (no sources) is rejected: its subscription keys are unknown.
	for _, p := range []string{"../../../docker/scd/config.real-shape.yml", refDir + "minimal.yml"} {
		code, _, errs = run("validate-config", "--config", p)
		if code != 1 || !strings.Contains(errs, `subscriptions.0: unknown key "input"`) {
			t.Fatalf("%s: code %d err %q", p, code, errs)
		}
	}
}

func TestValidateConfigWarnings(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yml")
	body := `subscriptions:
  - id: s1
    sources: [{id: vless, type: vless, input: ./sub.txt}]
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
    sources:
      - id: vless
        type: vless
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
	if code != 0 || !strings.Contains(out, "\"fetchHeaders\": {\n            \"x-hwid\": \"<redacted>\"\n          }") ||
		strings.Contains(out+errs, "s3cr3t") {
		t.Fatalf("code %d out %s err %s", code, out, errs)
	}
	code, out, errs = run("validate-config", "--config", p)
	if code != 0 || strings.Contains(out+errs, "s3cr3t") {
		t.Fatalf("validate: code %d out %s err %s", code, out, errs)
	}
}

// Final review I3: print-config redacts the subscription input, the monitor,
// balancer monitor and remote ping URLs and the fetchHeaders values unless
// --include-secrets is given.
const secretConfig = `
subscriptions:
  - id: s1
    sources:
      - id: vless
        type: vless
        input: https://sub.example.com/sub/INPUTTOKEN?k=INPUTQUERY
        fetchHeaders: {x-hwid: "${SCD_TEST_HWID}"}
        portRange: {start: 20000, end: 20010}
    target:
      address: 127.0.0.1:8080
      inboundSocks: {listen: 127.0.0.1}
      monitor:
        enabled: true
        schedule: "* * * * *"
        request: {url: "https://probe.example.com/p/MONTOKEN?q=MONQUERY", expectedStatus: 204}
      balancerMonitor:
        enabled: true
        schedule: "* * * * *"
        socks5: {host: 127.0.0.1, port: 1080}
        request: {url: "https://bprobe.example.com/b/BALTOKEN", expectedStatus: 204}
        remotePing: {enabled: true, url: "https://kuma.example.com/api/push/PUSHTOKEN?status=up"}
runtime: {mode: daemon, schedule: "* * * * *"}
logging: {}
resources: {outbounds: {enabled: true}, inbounds: {enabled: true}, routing: {enabled: true}}
`

var secretTokens = []string{"INPUTTOKEN", "INPUTQUERY", "HEADERSECRET", "MONTOKEN", "MONQUERY", "BALTOKEN", "PUSHTOKEN"}

func writeSecretConfig(t *testing.T) string {
	t.Helper()
	t.Setenv("SCD_TEST_HWID", "HEADERSECRET")
	p := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(p, []byte(secretConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPrintConfigRedactsSecretsByDefault(t *testing.T) {
	code, out, errs := run("print-config", "--config", writeSecretConfig(t))
	if code != 0 {
		t.Fatalf("code %d err %s", code, errs)
	}
	for _, s := range secretTokens {
		if strings.Contains(out+errs, s) {
			t.Errorf("print-config leaks %s:\n%s", s, out)
		}
	}
	for _, want := range []string{
		`"input": "https://sub.example.com/<redacted>"`,
		`"url": "https://probe.example.com/<redacted>"`,
		`"url": "https://bprobe.example.com/<redacted>"`,
		`"url": "https://kuma.example.com/<redacted>"`,
		`"x-hwid": "<redacted>"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("print-config lacks %s:\n%s", want, out)
		}
	}
}

func TestPrintConfigIncludeSecrets(t *testing.T) {
	p := writeSecretConfig(t)
	code, out, errs := run("print-config", "--config", p, "--include-secrets")
	if code != 0 {
		t.Fatalf("code %d err %s", code, errs)
	}
	for _, s := range secretTokens {
		if !strings.Contains(out, s) {
			t.Errorf("print-config --include-secrets lacks %s:\n%s", s, out)
		}
	}
	_, red, _ := run("print-config", "--config", p)
	if got, want := jsonKeys(out), jsonKeys(red); !slices.Equal(got, want) {
		t.Errorf("key order differs from the redacted form:\n%v\n%v", got, want)
	}
}

var keyRe = regexp.MustCompile(`(?m)^\s*"([^"]+)":`)

// jsonKeys lists the object keys of an indented JSON document in order.
func jsonKeys(doc string) []string {
	var out []string
	for _, m := range keyRe.FindAllStringSubmatch(doc, -1) {
		out = append(out, m[1])
	}
	return out
}
