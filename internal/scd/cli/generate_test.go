package cli_test

import (
	"bytes"
	"encoding/base64"
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
	code, stdout, stderr := run("generate", "--source-id", "vless", "--input", in, "--output", out, "--log", log)
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
	code, _, stderr = run("generate", "--input", "x", "--output", "m", "--log", "l")
	if code != 1 || !strings.Contains(stderr, `required flag(s) "source-id" not set`) {
		t.Fatalf("code %d stderr %q", code, stderr)
	}
	for _, args := range [][]string{{"--source-id", "-bad"}, {"--source-id", "vless", "--type", "wireguard"}} {
		code, _, stderr = run(append([]string{"generate", "--input", "x", "--output", "m", "--log", "l"}, args...)...)
		if code != 1 || !strings.Contains(stderr, "--") {
			t.Fatalf("%v: code %d stderr %q", args, code, stderr)
		}
	}
}

// --source-id is the tag prefix.
func TestGenerateSourceIDPrefixesTags(t *testing.T) {
	dir := t.TempDir()
	out, log := filepath.Join(dir, "m.json"), filepath.Join(dir, "l.json")
	code, _, stderr := run("generate", "--source-id", "sub2", "--input", "../../../docker/fixtures/subscription.txt", "--output", out, "--log", log)
	if code != 0 {
		t.Fatalf("code %d stderr %q", code, stderr)
	}
	ref := readJSON(t, "../../../testdata/reference/scd/generate/docker-fixture.manifest.json")
	got := readJSON(t, out)
	for i, e := range got["entries"].([]any) {
		tag := e.(map[string]any)["tag"].(string)
		want := ref["entries"].([]any)[i].(map[string]any)["tag"].(string)
		if tag != "sub2-"+strings.TrimPrefix(want, "vless-") {
			t.Errorf("tag %s, reference %s", tag, want)
		}
	}
}

func TestGenerateDockerFixture(t *testing.T) {
	dir := t.TempDir()
	out, log := filepath.Join(dir, "m.json"), filepath.Join(dir, "l.json")
	in := "../../../docker/fixtures/subscription.txt"
	code, stdout, stderr := run("generate", "--source-id", "vless", "--input", in, "--output", out, "--log", log)
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
	if code, _, stderr := run("generate", "--source-id", "vless", "--input", in, "--output", out, "--log", log); code != 0 {
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

func TestGenerateWireGuard(t *testing.T) {
	b, err := os.ReadFile("../../../docker/fixtures/wireguard/candidates.txt")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	in := filepath.Join(dir, "wg")
	if err := os.WriteFile(in, b, 0o600); err != nil {
		t.Fatal(err)
	}
	out, log := filepath.Join(dir, "m.json"), filepath.Join(dir, "l.json")
	code, stdout, stderr := run("generate", "--type", "wireguard", "--source-id", "wg", "--endpoint-domain-strategy", "UseIPv4",
		"--input", in, "--output", out, "--log", log)
	if code != 0 {
		t.Fatalf("code %d stderr %q", code, stderr)
	}
	m := readJSON(t, out)
	entries := m["entries"].([]any)
	if len(entries) != 7 || m["sourceFile"] != in {
		t.Fatalf("manifest %v", m)
	}
	ob := entries[0].(map[string]any)["jsonOutbound"].(map[string]any)
	if s := ob["settings"].(map[string]any); s["secretKey"] != "<redacted>" || s["noKernelTun"] != true {
		t.Fatalf("outbound %v", ob)
	}
	texts := []string{stdout, stderr}
	for _, p := range []string{out, log} {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		texts = append(texts, string(b))
	}
	for _, by := range []byte{0x11, 0x12, 0x14, 0x15, 0x16, 0x17, 0x18, 0x19, 0x35} {
		k := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{by}, 32))
		for _, s := range texts {
			if strings.Contains(s, k) {
				t.Fatalf("generate output holds the key %s", k)
			}
		}
	}

	if code, _, stderr := run("generate", "--type", "wireguard", "--source-id", "wg", "--input", in, "--output", out, "--log", log); code != 1 ||
		!strings.Contains(stderr, "--endpoint-domain-strategy must be one of") {
		t.Fatalf("code %d stderr %q", code, stderr)
	}
	if err := os.Chmod(in, 0o644); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := run("generate", "--type", "wireguard", "--source-id", "wg", "--endpoint-domain-strategy", "AsIs",
		"--input", in, "--output", out, "--log", log); code != 1 || !strings.Contains(stderr, "group and others must have no access") {
		t.Fatalf("code %d stderr %q", code, stderr)
	}
}

func TestGenerateVLESSMasksCandidateLines(t *testing.T) {
	sub, err := os.ReadFile("../../../docker/fixtures/subscription.txt")
	if err != nil {
		t.Fatal(err)
	}
	wg, err := os.ReadFile("../../../docker/fixtures/wireguard/candidates.txt")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	in := filepath.Join(dir, "in.txt")
	if err := os.WriteFile(in, append(sub, wg...), 0o600); err != nil {
		t.Fatal(err)
	}
	out, log := filepath.Join(dir, "m.json"), filepath.Join(dir, "l.json")
	code, stdout, stderr := run("generate", "--source-id", "vless", "--input", in, "--output", out, "--log", log)
	if code != 0 {
		t.Fatalf("code %d stderr %q", code, stderr)
	}
	texts := []string{stdout, stderr}
	for _, p := range []string{out, log} {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		texts = append(texts, string(b))
	}
	for _, by := range []byte{0x11, 0x12, 0x14, 0x15, 0x16, 0x17, 0x18, 0x19, 0x35} {
		k := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{by}, 32))
		for _, s := range texts {
			if strings.Contains(s, k) {
				t.Fatalf("generate output holds the key %s", k)
			}
		}
	}
	if !strings.Contains(texts[3], `"raw": "wireguard://<redacted>"`) {
		t.Fatalf("log %s", texts[3])
	}
}
