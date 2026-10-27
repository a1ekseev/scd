package configload

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func env(vars map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		v, ok := vars[name]
		return v, ok
	}
}

func rootMap(t *testing.T, d Doc) map[string]any {
	t.Helper()
	m, ok := d.Root.(map[string]any)
	if !ok {
		t.Fatalf("Root = %#v, want map[string]any", d.Root)
	}
	return m
}

func TestLoadRawStrictInterpolatesValues(t *testing.T) {
	p := writeConfig(t, "a: ${X}\nb: x-${lower_1}-y\nc: \"${X}\"\nl:\n  - ${X}\nf: [\"${X}\"]\n")
	d, err := Load(p, EnvRawStrict, env(map[string]string{"X": "true", "lower_1": "v"}))
	if err != nil {
		t.Fatal(err)
	}
	m := rootMap(t, d)
	if m["a"] != true {
		t.Fatalf("a = %#v, want bool true (plain scalar re-resolved)", m["a"])
	}
	if m["b"] != "x-v-y" {
		t.Fatalf("b = %#v", m["b"])
	}
	if m["c"] != "true" {
		t.Fatalf("c = %#v, want string (quoted scalar)", m["c"])
	}
	if l, ok := m["l"].([]any); !ok || len(l) != 1 || l[0] != true {
		t.Fatalf("l = %#v", m["l"])
	}
	// In flow collections ${...} must be quoted (YAML: "{" is an indicator).
	if f, ok := m["f"].([]any); !ok || len(f) != 1 || f[0] != "true" {
		t.Fatalf("f = %#v", m["f"])
	}
	if d.Path != p || d.Dir != filepath.Dir(p) {
		t.Fatalf("Path/Dir = %q/%q", d.Path, d.Dir)
	}

	t.Run("comments are not expanded", func(t *testing.T) {
		p := writeConfig(t, "a: 1 # ${NOPE}\n# ${NOPE2}\n")
		if _, err := Load(p, EnvRawStrict, env(nil)); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("missing value variable", func(t *testing.T) {
		p := writeConfig(t, "a: 1 # ${NOPE}\nb: ${ALSO_MISSING}\nc: ${THIRD}\n")
		_, err := Load(p, EnvRawStrict, env(nil))
		if err == nil || err.Error() != `missing environment variable "ALSO_MISSING" required by config` {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("keys are not expanded", func(t *testing.T) {
		p := writeConfig(t, "${K}: 1\n")
		d, err := Load(p, EnvRawStrict, env(nil))
		if err != nil {
			t.Fatal(err)
		}
		if got := rootMap(t, d)["${K}"]; got != 1 {
			t.Fatalf("root = %#v", d.Root)
		}
	})
	t.Run("value cannot change structure", func(t *testing.T) {
		p := writeConfig(t, "a: ${V}\nb: 2\n")
		d, err := Load(p, EnvRawStrict, env(map[string]string{"V": "x: y\nb: 3\n"}))
		if err != nil {
			t.Fatal(err)
		}
		m := rootMap(t, d)
		if m["a"] != "x: y\nb: 3\n" || m["b"] != 2 || len(m) != 2 {
			t.Fatalf("root = %#v", m)
		}
	})
	t.Run("empty value allowed", func(t *testing.T) {
		p := writeConfig(t, "a: x${E}\n")
		d, err := Load(p, EnvRawStrict, env(map[string]string{"E": ""}))
		if err != nil {
			t.Fatal(err)
		}
		if got := rootMap(t, d)["a"]; got != "x" {
			t.Fatalf("a = %#v", got)
		}
	})
}

func TestLoadLenientInterpolatesStringsOnly(t *testing.T) {
	p := writeConfig(t, "a: \"${X}\"\nn: 5\n\"${X}\": k\nl: [\"p-${Y}\", 7]\no: {s: \"${Y}\"}\n")
	d, err := Load(p, EnvStringsLenient, env(map[string]string{"Y": "yv"}))
	if err != nil {
		t.Fatal(err)
	}
	m := rootMap(t, d)
	if m["a"] != "" {
		t.Fatalf("a = %#v, want empty string", m["a"])
	}
	if m["n"] != 5 {
		t.Fatalf("n = %#v", m["n"])
	}
	if m["${X}"] != "k" {
		t.Fatalf("key ${X} was touched: %#v", m)
	}
	l := m["l"].([]any)
	if l[0] != "p-yv" || l[1] != 7 {
		t.Fatalf("l = %#v", l)
	}
	if s := m["o"].(map[string]any)["s"]; s != "yv" {
		t.Fatalf("o.s = %#v", s)
	}
}

func TestLoadNoneKeepsLiteral(t *testing.T) {
	p := writeConfig(t, "a: ${X}\n")
	d, err := Load(p, EnvNone, env(map[string]string{"X": "v"}))
	if err != nil {
		t.Fatal(err)
	}
	if got := rootMap(t, d)["a"]; got != "${X}" {
		t.Fatalf("a = %#v", got)
	}
}

func TestLoadDuplicateKeyFails(t *testing.T) {
	p := writeConfig(t, "a: 1\na: 2\n")
	_, err := Load(p, EnvNone, nil)
	if err == nil || !strings.Contains(err.Error(), `mapping key "a" already defined`) {
		t.Fatalf("err = %v", err)
	}
}

func TestLoadEmptyFileIsEmptyMap(t *testing.T) {
	for name, content := range map[string]string{"empty": "", "comment": "# nothing\n", "null": "null\n"} {
		t.Run(name, func(t *testing.T) {
			d, err := Load(writeConfig(t, content), EnvNone, nil)
			if err != nil {
				t.Fatal(err)
			}
			if m := rootMap(t, d); len(m) != 0 {
				t.Fatalf("Root = %#v", m)
			}
		})
	}
}

func TestLoadMultiDocFails(t *testing.T) {
	p := writeConfig(t, "a: 1\n---\nb: 2\n")
	_, err := Load(p, EnvNone, nil)
	if err == nil || err.Error() != "config must contain a single YAML document" {
		t.Fatalf("err = %v", err)
	}
}

func TestLoadMissingFileError(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "nope.yml"), EnvNone, nil)
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("err = %v, want fs.ErrNotExist", err)
	}
}

func TestLoadRelativePathIsAbsolute(t *testing.T) {
	p := writeConfig(t, "a: 1\n")
	t.Chdir(filepath.Dir(p))
	d, err := Load("config.yml", EnvNone, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(d.Path) || filepath.Base(d.Path) != "config.yml" || d.Dir != filepath.Dir(d.Path) {
		t.Fatalf("Path/Dir = %q/%q", d.Path, d.Dir)
	}
}

func TestLoadNonStringKeysBecomeStrings(t *testing.T) {
	d, err := Load(writeConfig(t, "m: {1: a, true: b}\n"), EnvNone, nil)
	if err != nil {
		t.Fatal(err)
	}
	m, ok := rootMap(t, d)["m"].(map[string]any)
	if !ok || m["1"] != "a" || m["true"] != "b" {
		t.Fatalf("m = %#v", rootMap(t, d)["m"])
	}
}

func TestResolveInput(t *testing.T) {
	dir := "/etc/app"
	tests := []struct{ in, want string }{
		{"-", "-"},
		{"HTTP://x", "HTTP://x"},
		{"https://h/p?q=1", "https://h/p?q=1"},
		{"/abs", "/abs"},
		{"rel/a.txt", "/etc/app/rel/a.txt"},
		{"ftp://x/y", "/etc/app/ftp:/x/y"},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			if got := ResolveInput(dir, tc.in); got != tc.want {
				t.Fatalf("ResolveInput(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestLoadLeadingZeroIsOctal pins the yaml.v3 behaviour documented in
// CONFIG-MIGRATION.md: an integer with a leading 0 is octal (TS yaml@2: 17).
func TestLoadLeadingZeroIsOctal(t *testing.T) {
	d, err := Load(writeConfig(t, "a: 017\nb: 17\n"), EnvNone, nil)
	if err != nil {
		t.Fatal(err)
	}
	m := rootMap(t, d)
	if m["a"] != 15 || m["b"] != 17 {
		t.Fatalf("a, b = %#v, %#v; want 15, 17", m["a"], m["b"])
	}
}
