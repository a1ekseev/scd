package cli_test

import (
	"crypto/x509"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/a1ekseev/scd/internal/common/candidate"
)

func TestGenerateMasque(t *testing.T) {
	b, err := os.ReadFile("../../../docker/fixtures/masque/candidates.txt")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	in := filepath.Join(dir, "mq")
	if err := os.WriteFile(in, b, 0o600); err != nil {
		t.Fatal(err)
	}
	out, log := filepath.Join(dir, "m.json"), filepath.Join(dir, "l.json")
	code, stdout, stderr := run("generate", "--type", "masque", "--source-id", "mq", "--input", in, "--output", out, "--log", log)
	if code != 0 {
		t.Fatalf("code %d stderr %q", code, stderr)
	}
	m := readJSON(t, out)
	entries := m["entries"].([]any)
	if len(entries) != 5 {
		t.Fatalf("manifest %v", m)
	}
	ob := entries[0].(map[string]any)["jsonOutbound"].(map[string]any)
	ss := ob["streamSettings"].(map[string]any)
	if ss["security"] != "tls" || ss["masqueSettings"].(map[string]any)["warp"].(map[string]any)["privateKey"] != "<redacted>" {
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
	if !strings.Contains(texts[3], `"masqueInvalid": 1`) {
		t.Fatalf("log %s", texts[3])
	}
	for l := range strings.SplitSeq(string(b), "\n") {
		c, err := candidate.ParseMasque(l)
		if err != nil {
			continue
		}
		p8, _ := x509.MarshalPKCS8PrivateKey(c.PrivateKey)
		sec1, _ := x509.MarshalECPrivateKey(c.PrivateKey)
		for _, k := range []string{base64.RawURLEncoding.EncodeToString(p8), base64.StdEncoding.EncodeToString(p8), base64.StdEncoding.EncodeToString(sec1)} {
			for _, s := range texts {
				if strings.Contains(s, k) {
					t.Fatal("generate output holds a device key")
				}
			}
		}
	}
	if code, _, stderr := run("generate", "--type", "masque", "--source-id", "mq", "--endpoint-domain-strategy", "AsIs",
		"--input", in, "--output", out, "--log", log); code != 1 || !strings.Contains(stderr, "--endpoint-domain-strategy is only for --type wireguard") {
		t.Fatalf("code %d stderr %q", code, stderr)
	}
}
