package cli_test

import (
	"bytes"
	"flag"
	"os"
	"strings"
	"testing"

	rootcli "github.com/a1ekseev/scd/internal/cli"
)

const (
	exampleConfig = "../../../docker/gtl/config.example.yml"
	goldenJSON    = "../../../testdata/golden/gtl/print-config-docker-example.json"
)

func run(args ...string) (code int, stdout, stderr string) {
	var out, errb bytes.Buffer
	code = rootcli.Execute(append([]string{"gtl"}, args...), &out, &errb)
	return code, out.String(), errb.String()
}

var update = flag.Bool("update", false, "regenerate the golden print-config file")

// The print-config output of the example equals the golden file (written
// with -update).
func TestPrintConfigDockerExample(t *testing.T) {
	code, out, errs := run("print-config", "--config", exampleConfig)
	if code != 0 {
		t.Fatalf("code %d: %s", code, errs)
	}
	if !strings.HasPrefix(out, "{\n  \"server\": {") || !strings.HasSuffix(out, "}\n") {
		t.Fatalf("unexpected layout: %q", out)
	}
	if *update {
		if err := os.WriteFile(goldenJSON, []byte(out), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(goldenJSON)
	if err != nil {
		t.Fatal(err)
	}
	if out != string(want) {
		t.Fatalf("print-config differs from %s (run with -update to regenerate):\n%s", goldenJSON, out)
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
