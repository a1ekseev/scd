package cli_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestGenerateRejectsNoValidEntries(t *testing.T) { // P25
	dir := t.TempDir()
	in := filepath.Join(dir, "in.txt")
	if err := os.WriteFile(in, []byte("trojan://example\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, log := filepath.Join(dir, "m.json"), filepath.Join(dir, "l.json")
	code, stdout, stderr := run("generate", "--input", in, "--output", out, "--log", log)
	if code != 1 || stdout != "" || !strings.Contains(stderr, "produced no valid outbound entries") ||
		!strings.Contains(stderr, `"`+in+`"`) {
		t.Fatalf("code %d stdout %q stderr %q", code, stdout, stderr)
	}
	for _, p := range []string{out, log} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("%s was created: %v", p, err)
		}
	}
}

func TestGenerateRequiresFlags(t *testing.T) {
	code, _, stderr := run("generate", "--input", "x")
	if code != 1 || !strings.Contains(stderr, "required flag") {
		t.Fatalf("code %d stderr %q", code, stderr)
	}
}

func TestGenerateDockerFixture(t *testing.T) {
	dir := t.TempDir()
	out, log := filepath.Join(dir, "m.json"), filepath.Join(dir, "l.json")
	in := "../../../docker/fixtures/subscription.txt"
	code, stdout, stderr := run("generate", "--input", in, "--output", out, "--log", log)
	if code != 0 || stderr != "" {
		t.Fatalf("code %d stderr %q", code, stderr)
	}
	want := readJSON(t, "../../../testdata/reference/scd/generate/docker-fixture.summary.json")
	var got map[string]any
	if err := json.Unmarshal([]byte(stdout), &got); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("summary %s (%v), want %v", stdout, err, want)
	}
	if !strings.HasPrefix(stdout, "{\n  \"totalLines\": 8,\n") || !strings.HasSuffix(stdout, "}\n") {
		t.Fatalf("layout %q", stdout)
	}

	m, l := readJSON(t, out), readJSON(t, log)
	abs, _ := filepath.Abs(in)
	if m["version"] != 1.0 || m["sourceFile"] != abs || len(m["entries"].([]any)) != 8 || len(m["skipped"].([]any)) != 0 {
		t.Fatalf("manifest version %v sourceFile %v", m["version"], m["sourceFile"])
	}
	if l["generatedAt"] != m["generatedAt"] || !reflect.DeepEqual(l["summary"], want) || !reflect.DeepEqual(l["summary"], m["summary"]) {
		t.Fatalf("parse log %v", l)
	}
	if ref := readJSON(t, "../../../testdata/reference/scd/generate/docker-fixture.manifest.json"); !sameTags(m, ref) {
		t.Fatal("tags differ from the TS reference")
	}
}

func TestGenerateKeepsHTMLCharacters(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "in.txt")
	line := `vless://11111111-1111-4111-8111-111111111111@h.example.test:443?type=xhttp&security=tls` +
		`&extra=%7B%22xPaddingBytes%22%3A%22100-1000%22%7D#a<b>&c`
	if err := os.WriteFile(in, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	out, log := filepath.Join(dir, "m.json"), filepath.Join(dir, "l.json")
	if code, _, stderr := run("generate", "--input", in, "--output", out, "--log", log); code != 0 {
		t.Fatalf("code %d stderr %q", code, stderr)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"label": "a<b>&c"`) || strings.Contains(string(b), "\\u003c") ||
		!strings.Contains(string(b), `"xPaddingBytes": "100-1000"`) {
		t.Fatalf("manifest:\n%s", b)
	}
}

func sameTags(a, b map[string]any) bool {
	tags := func(m map[string]any) []any {
		var out []any
		for _, e := range m["entries"].([]any) {
			out = append(out, e.(map[string]any)["tag"])
		}
		return out
	}
	return reflect.DeepEqual(tags(a), tags(b))
}
