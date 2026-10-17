package cli_test

import (
	"bytes"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	rootcli "github.com/a1ekseev/scd/internal/cli"
)

const (
	exampleConfig = "../../../docker/gtl/config.example.yml"
	referenceJSON = "../../../testdata/reference/gtl/print-config-docker-example.json"
)

func run(args ...string) (code int, stdout, stderr string) {
	var out, errb bytes.Buffer
	code = rootcli.Execute(append([]string{"gtl"}, args...), &out, &errb)
	return code, out.String(), errb.String()
}

func TestPrintConfigMatchesReference(t *testing.T) {
	code, out, errs := run("print-config", "--config", exampleConfig)
	if code != 0 {
		t.Fatalf("code %d: %s", code, errs)
	}
	raw, err := os.ReadFile(referenceJSON)
	if err != nil {
		t.Fatal(err)
	}
	var got, want any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %s want %s", out, raw)
	}
	if !strings.HasPrefix(out, "{\n  \"server\": {") || !strings.HasSuffix(out, "}\n") {
		t.Fatalf("unexpected layout: %q", out)
	}
}

func TestValidateConfigOutput(t *testing.T) {
	code, out, errs := run("validate-config", "--config", exampleConfig)
	if code != 0 || out != "Config is valid\n" || errs != "" {
		t.Fatalf("code %d out %q err %q", code, out, errs)
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
