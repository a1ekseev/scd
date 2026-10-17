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
