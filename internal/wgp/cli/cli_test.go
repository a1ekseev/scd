package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/a1ekseev/scd/internal/common/cliutil"
	"github.com/a1ekseev/scd/internal/common/wgpstatus"
	"github.com/a1ekseev/scd/internal/wgp/config"
	_ "github.com/a1ekseev/scd/internal/wgp/provider/providertest"
	"github.com/a1ekseev/scd/internal/wgp/store"
)

const (
	exampleConfig = "../../../docker/wgp/config.example.yml"
	exampleGolden = "../../../testdata/golden/wgp/print-config-docker-example.json"
)

const fakeConfig = `server: {listen: 127.0.0.1:8080}
state: {dir: /app/state, keyFile: /run/secrets/key}
identification: {fake: {timezone: UTC}}
deviceNamePrefix: wgp-a-
providers:
  - {id: one, type: fake, schedule: "@hourly", kind: wireguard}
outputs:
  - {id: one, path: /app/out/one, provider: one, candidates: 1}
`

func run(args ...string) (code int, stdout, stderr string) {
	var out, errb bytes.Buffer
	cmd := New()
	cmd.SetArgs(args)
	cmd.SetOut(&out)
	cmd.SetErr(&errb)
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	if err := cmd.Execute(); err != nil {
		return 1, out.String(), err.Error()
	}
	return 0, out.String(), errb.String()
}

func TestExampleValidates(t *testing.T) {
	code, out, errs := run("validate-config", "--config", exampleConfig)
	if code != 0 || !strings.Contains(out, `"valid": true`) {
		t.Fatalf("code %d out %q err %q", code, out, errs)
	}
}

func TestPrintConfigExampleGolden(t *testing.T) {
	code, out, errs := run("print-config", "--config", exampleConfig)
	if code != 0 {
		t.Fatalf("code %d: %s", code, errs)
	}
	want, err := os.ReadFile(exampleGolden)
	if err != nil {
		t.Fatal(err)
	}
	if out != string(want) {
		t.Fatalf("print-config differs from %s:\n%s", exampleGolden, out)
	}
}

func TestLoginErrors(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(p, []byte(fakeConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, cmd := range []string{"login", "logout"} {
		for id, want := range map[string]string{
			"nope": `unknown provider "nope"`,
			"one":  `provider "one" (type "fake") does not support login`,
		} {
			if code, _, errs := run(cmd, "--config", p, "--provider", id); code != 1 || errs != want {
				t.Errorf("%s --provider %s: code %d err %q", cmd, id, code, errs)
			}
		}
		if code, _, errs := run(cmd, "--config", exampleConfig, "--provider", "warp"); code != 1 || errs != `provider "warp" (type "warp") does not support login` {
			t.Errorf("%s --provider warp: code %d err %q", cmd, code, errs)
		}
		if code, _, errs := run(cmd, "--config", p); code != 1 || !strings.Contains(errs, `"provider" not set`) {
			t.Errorf("%s without --provider: code %d err %q", cmd, code, errs)
		}
	}
}

// stateConfig writes a config whose state dir and key are in a temp dir and
// returns its path and the state dir.
func stateConfig(t *testing.T) (cfgPath, stateDir string) {
	t.Helper()
	root := t.TempDir()
	stateDir = filepath.Join(root, "state")
	if err := os.Mkdir(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	keyFile := filepath.Join(root, "key")
	if err := os.WriteFile(keyFile, []byte(base64.StdEncoding.EncodeToString(make([]byte, 32))), 0o600); err != nil {
		t.Fatal(err)
	}
	line, err := os.ReadFile("../../../docker/fixtures/wireguard/candidates.txt")
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(root, "out")
	if err := os.Mkdir(out, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := strings.NewReplacer(
		"state: {dir: /app/state, keyFile: /run/secrets/key}", "state: {dir: "+stateDir+", keyFile: "+keyFile+"}",
		"127.0.0.1:8080", freeAddr(t),
		"kind: wireguard}", "kind: wireguard, lines: ["+strconv.Quote(strings.SplitN(string(line), "\n", 2)[0])+"]}",
		"/app/out/", out+"/",
	).Replace(fakeConfig)
	cfg += "  - {id: two, path: " + out + "/two, provider: two}\n"
	cfg = strings.Replace(cfg, "outputs:", "  - {id: two, type: fakelogin, schedule: \"@hourly\"}\noutputs:", 1)
	cfgPath = filepath.Join(root, "config.yml")
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfgPath, stateDir
}

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

// TestServe runs serve until both providers ticked: "one" publishes its
// line, "two" returns no lines and no error, so its output is an empty
// file (no live candidate); the page answers.
func TestServe(t *testing.T) {
	p, _ := stateConfig(t)
	l, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- serve(ctx, l, io.Discard) }()
	base := "http://" + l.Config.Server.Listen
	var st wgpstatus.Status
	for deadline := time.Now().Add(5 * time.Second); len(st.Providers) < 2; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("status %+v", st)
		}
		if resp, err := http.Get(base + "/api/status"); err == nil { //nolint:noctx // test against a local server
			_ = json.NewDecoder(resp.Body).Decode(&st)
			_ = resp.Body.Close()
		}
	}
	if one, two := st.Providers[0], st.Providers[1]; one.State != "ok" || two.State != "ok" || two.Issued != 0 {
		t.Fatalf("status %+v", st)
	}
	dir := filepath.Dir(l.Config.Outputs[0].Path)
	if fromFile, err := wgpstatus.Read(dir, 1<<16); err != nil || len(fromFile.Providers) != 2 {
		t.Fatalf("status.json %+v, %v", fromFile, err)
	}
	if info, err := os.Stat(filepath.Join(dir, "two")); err != nil || info.Size() != 0 {
		t.Fatalf("empty output: %v %v", info, err)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestServeStopsOnCancel(t *testing.T) {
	p, _ := stateConfig(t)
	l, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	var errb bytes.Buffer
	if err := serve(ctx, l, &errb); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"event":"config_loaded"`, `"enabledProviders":2`, `"version":"dev"`, `"event":"state_opened"`, `"keySha8":"66687aad"`, `"event":"server_stopping"`, `"signal":"context canceled"`} {
		if !strings.Contains(errb.String(), want) {
			t.Fatalf("log %q lacks %s", errb.String(), want)
		}
	}
}

func TestServeStateErrors(t *testing.T) {
	p, dir := stateConfig(t)
	l, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	unlock, err := store.Lock(dir)
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	if err := serve(t.Context(), l, &logs); err == nil || !strings.Contains(err.Error(), "in use by another wgp process") ||
		!errors.Is(err, cliutil.ErrLogged) || !strings.Contains(logs.String(), `"event":"service_failed"`) {
		t.Fatalf("serve with a held lock: %v\n%s", err, logs.String())
	}
	if code, _, errs := run("login", "--config", p, "--provider", "two"); code != 1 || !strings.Contains(errs, "in use by another wgp process") {
		t.Fatalf("login with a held lock: code %d err %q", code, errs)
	}
	if err := unlock(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := serve(t.Context(), l, io.Discard); err == nil || !strings.Contains(err.Error(), "has mode 0755") {
		t.Fatalf("serve with an open state dir: %v", err)
	}
}

func TestLoginLogout(t *testing.T) {
	p, dir := stateConfig(t)
	if code, _, errs := run("logout", "--config", p, "--provider", "two"); code != 1 || errs != "not logged in" {
		t.Fatalf("logout before login: code %d err %q", code, errs)
	}
	cmd := New()
	cmd.SetArgs([]string{"login", "--config", p, "--provider", "two"})
	cmd.SetIn(strings.NewReader("fake-session\n"))
	cmd.SetOut(io.Discard)
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "two.state"))
	if err != nil || bytes.Contains(raw, []byte("fake-session")) {
		t.Fatalf("record %q, %v", raw, err)
	}
	if code, _, errs := run("logout", "--config", p, "--provider", "two"); code != 0 {
		t.Fatalf("logout: %s", errs)
	}
}
