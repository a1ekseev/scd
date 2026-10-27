package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func mustFail(t *testing.T, body string) string {
	t.Helper()
	_, err := Load(write(t, body))
	if err == nil {
		t.Fatal("expected error")
	}
	return err.Error()
}

func TestLoadValid(t *testing.T) {
	cfg, err := Load(write(t, "server:\n  listen: 127.0.0.1:8080\nload:\n  path: /load\n  maxSizeKb: 64\nlogging:\n  level: debug\n  format: pretty\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.Listen != "127.0.0.1:8080" || cfg.Load.Path != "/load" || cfg.Load.MaxSizeKb != 64 ||
		cfg.Logging.Level != "debug" || cfg.Logging.Format != "pretty" {
		t.Fatalf("unexpected config: %+v", cfg)
	}
}

func TestLoadDefaults(t *testing.T) {
	cfg, err := Load(write(t, "server:\n  listen: :8080\nload:\n  path: /a/b\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Load.MaxSizeKb != 10240 || cfg.Logging.Level != "info" || cfg.Logging.Format != "json" {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
}

func TestLoadBadListen(t *testing.T) {
	msg := mustFail(t, "server:\n  listen: localhost\n")
	for _, want := range []string{"  - server.listen: ", "  - load: required"} {
		if !strings.Contains(msg, want) {
			t.Errorf("missing %q in %q", want, msg)
		}
	}
}

func TestLoadMaxSizeZero(t *testing.T) {
	msg := mustFail(t, "server:\n  listen: :8080\nload:\n  path: /load\n  maxSizeKb: 0\n")
	if !strings.Contains(msg, "  - load.maxSizeKb: must be at least 1") {
		t.Fatal(msg)
	}
}

func TestLoadMaxSizeBounds(t *testing.T) {
	// MaxInt64/1024 is the largest value whose byte size fits int64.
	const head = "server:\n  listen: :8080\nload:\n  path: /load\n  maxSizeKb: "
	cfg, err := Load(write(t, head+"9007199254740991\n"))
	if err != nil || cfg.Load.MaxSizeKb != 9007199254740991 {
		t.Fatalf("cfg=%+v err=%v", cfg, err)
	}
	msg := mustFail(t, head+"9007199254740992\n")
	if !strings.Contains(msg, "  - load.maxSizeKb: must be at most 9007199254740991") {
		t.Fatal(msg)
	}
}

func TestLoadMissingPath(t *testing.T) {
	msg := mustFail(t, "server:\n  listen: :8080\nload: {}\n")
	if !strings.Contains(msg, "  - load.path: required") {
		t.Fatal(msg)
	}
}

func TestLoadPathRules(t *testing.T) {
	tests := []struct {
		path string
		want []string
	}{
		{"/", []string{"must not equal /"}},
		{"load", []string{"must start with /"}},
		{"/load/", []string{"must not end with /"}},
		{"/load?x=1", []string{"must not contain query or hash"}},
		{"/load#x", []string{"must not contain query or hash"}},
		{"lo?ad/", []string{"must start with /", "must not end with /", "must not contain query or hash"}},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			msg := mustFail(t, "server:\n  listen: :8080\nload:\n  path: '"+tt.path+"'\n")
			if n := strings.Count(msg, "  - load.path: "); n != len(tt.want) {
				t.Fatalf("got %d issues: %s", n, msg)
			}
			for _, w := range tt.want {
				if !strings.Contains(msg, "  - load.path: "+w) {
					t.Errorf("missing %q in %s", w, msg)
				}
			}
		})
	}
}

func TestLoadUnknownKey(t *testing.T) {
	msg := mustFail(t, "server:\n  listen: :8080\n  foo: 1\nload:\n  path: /load\n")
	if !strings.Contains(msg, `  - server: unknown key "foo"`) {
		t.Fatal(msg)
	}
}

func TestLoadBadLoggingEnums(t *testing.T) {
	msg := mustFail(t, "server:\n  listen: :8080\nload:\n  path: /load\nlogging:\n  level: loud\n  format: xml\n")
	for _, w := range []string{"  - logging.level: must be one of", "  - logging.format: must be one of json|pretty"} {
		if !strings.Contains(msg, w) {
			t.Errorf("missing %q in %s", w, msg)
		}
	}
}

func TestLoadNoEnvInterpolation(t *testing.T) {
	t.Setenv("X", "boom")
	cfg, err := Load(write(t, "server:\n  listen: :8080\nload:\n  path: /${X}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Load.Path != "/${X}" {
		t.Fatalf("path = %q", cfg.Load.Path)
	}
}

func TestLoadMissingFile(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "nope.yml"))
	if err == nil || !strings.Contains(err.Error(), "failed to read config") {
		t.Fatalf("err = %v", err)
	}
}

func TestGTLRealShapeGoFormValid(t *testing.T) {
	cfg, err := Load("../../../docker/gtl/config.real-shape.go.yml")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.Listen != "172.31.3.5:8080" {
		t.Fatalf("listen = %q", cfg.Server.Listen)
	}
}
