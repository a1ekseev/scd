package config_test

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/a1ekseev/scd/internal/common/jsonout"
	"github.com/a1ekseev/scd/internal/wgp/config"
	"github.com/a1ekseev/scd/internal/wgp/provider/providertest"
)

var update = flag.Bool("update", false, "regenerate the golden print-config files")

const goldenDir = "../../../testdata/golden/wgp/"

// base is a valid config with both fake provider types; tests replace parts
// of it.
const base = `server:
  listen: 127.0.0.1:8080
state:
  dir: /app/state
  keyFile: /run/secrets/wgp_state_key
identification:
  fake:
    timezone: Europe/Istanbul
deviceNamePrefix: wgp-a-
providers:
  - id: one
    type: fake
    schedule: "*/15 * * * *"
    kind: masque
    lines: [a, b]
  - id: two
    type: fakedir
    enabled: false
    schedule: "@hourly"
    dir: /app/static
outputs:
  - id: one
    path: /app/out/one
    provider: one
    candidates: 2
`

func write(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// replace swaps one exact fragment of base.
func replace(t *testing.T, old, repl string) string {
	t.Helper()
	if !strings.Contains(base, old) {
		t.Fatalf("fragment %q not in base", old)
	}
	return strings.Replace(base, old, repl, 1)
}

func TestLoadBase(t *testing.T) {
	l, err := config.Load(write(t, base))
	if err != nil {
		t.Fatal(err)
	}
	c := l.Config
	if c.Logging.Level != "info" || c.Logging.Format != "json" || c.API.TimeoutMs != 15000 ||
		c.API.Retry.Attempts != 3 || c.API.Retry.MinDelayMs != 3000 || c.API.Retry.MaxDelayMs != 8000 {
		t.Fatalf("defaults %+v %+v", c.Logging, c.API)
	}
	p, ok := c.Provider("one")
	if !ok || !p.Enabled {
		t.Fatalf("provider %+v", p)
	}
	f := p.Impl.(*providertest.Fake)
	if f.Kind() != "masque" || f.Env.Output.ID != "one" || f.Env.Output.Candidates != 2 ||
		f.Env.Identification != (providertest.Identification{Timezone: "Europe/Istanbul"}) || f.Env.DeviceNamePrefix != "wgp-a-" {
		t.Fatalf("fake %+v", f)
	}
	if two, _ := c.Provider("two"); two.Enabled || two.Impl.(*providertest.Fake).Env.Output.ID != "" {
		t.Fatalf("provider two %+v", two)
	}
}

func TestValidationErrors(t *testing.T) {
	cases := []struct{ name, body, want string }{
		{"unknown root key", base + "extra: 1\n", `<root>: unknown key "extra"`},
		{"server required", replace(t, "server:\n  listen: 127.0.0.1:8080\n", ""), "server: required"},
		{"listen", replace(t, "127.0.0.1:8080", "nope"), "server.listen:"},
		{"logging level", base + "logging: {level: loud}\n", "logging.level: must be one of"},
		{"state required", replace(t, "state:\n  dir: /app/state\n  keyFile: /run/secrets/wgp_state_key\n", ""), "state: required"},
		{"state dir relative", replace(t, "dir: /app/state", "dir: state"), "state.dir: must be a clean absolute path"},
		{"key file missing", replace(t, "  keyFile: /run/secrets/wgp_state_key\n", ""), "state.keyFile: required"},
		{"key file unclean", replace(t, "/run/secrets/wgp_state_key", "/run/../key"), "state.keyFile: must be a clean absolute path"},
		{"prefix required", replace(t, "deviceNamePrefix: wgp-a-\n", ""), "deviceNamePrefix: required"},
		{"prefix form", replace(t, "wgp-a-", "WGP_a"), "deviceNamePrefix: must match [a-z0-9-]+"},
		{"keys", base + "keys: {revokeOldAfterMinutes: 0}\n", "keys.revokeOldAfterMinutes: must be at least 1"},
		{"api timeout", base + "api: {timeoutMs: 0}\n", "api.timeoutMs: must be at least 1"},
		{"api delays", base + "api: {retry: {minDelayMs: 9000}}\n", "api.retry.maxDelayMs: must be at least minDelayMs"},
		{"providers required", replace(t, "providers:\n", "old:\n"), "providers: required"},
		{"providers empty", "server: {listen: 127.0.0.1:8080}\nstate: {dir: /s, keyFile: /k}\ndeviceNamePrefix: p-\nproviders: []\noutputs: []\n", "providers:"},
		{"unknown type", replace(t, "type: fakedir", "type: nope"), `providers.1.type: unknown provider type "nope"`},
		{"provider id form", replace(t, "id: two", "id: Two"), "providers.1.id: must match [a-z0-9]+"},
		{"duplicate provider", replace(t, "id: two", "id: one"), `providers.1.id: duplicate provider id "one"`},
		{"schedule required", replace(t, "    schedule: \"@hourly\"\n", ""), "providers.1.schedule: required"},
		{"schedule broken", replace(t, "@hourly", "* * *"), "providers.1.schedule:"},
		{"provider subtree strict", replace(t, "    dir: /app/static\n", "    dir: /app/static\n    extra: 1\n"), `providers.1: unknown key "extra"`},
		{"provider subtree required", replace(t, "    kind: masque\n", ""), "providers.0.kind: required"},
		{"enabled without output", replace(t, "    enabled: false\n", ""), `providers.1: enabled provider "two" has no output`},
		{"identification required", replace(t, "identification:\n  fake:\n    timezone: Europe/Istanbul\n", ""), "identification: required"},
		{"identification type required", replace(t, "  fake:\n    timezone: Europe/Istanbul\n", "  {}\n"), "identification.fake: required"},
		{"identification unknown type", replace(t, "identification:\n", "identification:\n  fakedir: {}\n"), `identification: unknown key "fakedir"`},
		{"timezone required", replace(t, "    timezone: Europe/Istanbul\n", "    {}\n"), "identification.fake.timezone: required"},
		{"timezone broken", replace(t, "Europe/Istanbul", "Mars/Olympus"), "identification.fake.timezone: must be an IANA time zone name"},
		{"timezone local", replace(t, "Europe/Istanbul", "Local"), "identification.fake.timezone: must be an IANA time zone name"},
		{"outputs required", replace(t, "outputs:\n", "old2:\n"), "outputs: required"},
		{"output strict", base + "    extra: 1\n", `outputs.0: unknown key "extra"`},
		{"output id dash", replace(t, "  - id: one\n    path:", "  - id: one-a\n    path:"), "outputs.0.id: must match [a-z0-9]+"},
		{"output id required", replace(t, "  - id: one\n    path:", "  - path:"), "outputs.0.id: required"},
		{"duplicate output", base + "  - id: one\n    path: /app/out/b\n    provider: two\n", `outputs.1.id: duplicate output id "one"`},
		{"output path relative", replace(t, "/app/out/one", "out/one"), "outputs.0.path: must be a clean absolute path"},
		{"output path status", replace(t, "/app/out/one", "/app/out/status.json"), "outputs.0.path: must not be named status.json"},
		{"output dir", base + "  - id: two\n    path: /app/other/two\n    provider: two\n", "outputs.1.path: must be in /app/out, like the other outputs"},
		{"output path duplicate", base + "  - id: two\n    path: /app/out/one\n    provider: two\n", "outputs.1.path: duplicate output path"},
		{"output provider required", replace(t, "    provider: one\n", ""), "outputs.0.provider: required"},
		{"output provider unknown", replace(t, "provider: one", "provider: nope"), `outputs.0.provider: unknown provider "nope"`},
		{"provider in two outputs", base + "  - id: other\n    path: /app/out/other\n    provider: one\n    candidates: 1\n", `outputs.1.provider: provider "one" already feeds output "one"`},
		{"candidates required", replace(t, "    candidates: 2\n", ""), "outputs.0.candidates: required"},
		{"candidates min", replace(t, "candidates: 2", "candidates: 0"), "outputs.0.candidates: must be at least 1"},
		{"candidates not allowed", base + "  - id: two\n    path: /app/out/two\n    provider: two\n    candidates: 1\n", `outputs.1.candidates: not allowed for provider type "fakedir"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := config.Load(write(t, c.body))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want %q, got %v", c.want, err)
			}
		})
	}
}

func TestDisabledProviderMayHaveOutput(t *testing.T) {
	body := base + "  - id: two\n    path: /app/out/two\n    provider: two\n"
	if _, err := config.Load(write(t, body)); err != nil {
		t.Fatal(err)
	}
}

// print-config prints the common provider keys, then the provider's own,
// and only the path of the state key.
func TestPrintGolden(t *testing.T) {
	l, err := config.Load(write(t, base))
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	if err := jsonout.Write(&b, l.Config); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(goldenDir, "print-config-fake.json")
	if *update {
		if err := os.MkdirAll(goldenDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, b.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(b.Bytes(), want) {
		t.Fatalf("%s differs (run with -update to regenerate):\n%s", path, b.String())
	}
}
