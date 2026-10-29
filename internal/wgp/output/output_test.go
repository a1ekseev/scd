package output

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/a1ekseev/scd/internal/common/candidate"
)

// fixtureLines returns the first n lines of a candidate fixture.
func fixtureLines(t *testing.T, kind string, n int) []string {
	t.Helper()
	b, err := os.ReadFile("../../../docker/fixtures/" + kind + "/candidates.txt")
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(string(b), "\n")[:n]
}

func TestPublish(t *testing.T) {
	wg := fixtureLines(t, "wireguard", 2)
	path := filepath.Join(t.TempDir(), "wg")
	out := Output{ID: "wg", Path: path, Kind: candidate.KindWireGuard}

	changed, err := Publish(out, wg)
	if err != nil || !changed {
		t.Fatalf("first publish: %v, %v", changed, err)
	}
	want := wg[0] + "\n" + wg[1] + "\n"
	if b, err := os.ReadFile(path); err != nil || string(b) != want {
		t.Fatalf("file %q, %v", b, err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v, %v", info, err)
	}

	// Unchanged: not rewritten, mtime kept.
	old := time.Unix(1_000_000_000, 0)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	if changed, err := Publish(out, wg); err != nil || changed {
		t.Fatalf("same lines: %v, %v", changed, err)
	}
	if info, _ := os.Stat(path); !info.ModTime().Equal(old) {
		t.Fatalf("mtime changed to %v", info.ModTime())
	}

	// A wider mode is fixed by a rewrite even with the same bytes.
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if changed, err := Publish(out, wg); err != nil || !changed {
		t.Fatalf("same lines, mode 0644: %v, %v", changed, err)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", info.Mode())
	}

	// Order is the given order; the bytes depend only on the lines.
	if changed, err := Publish(out, []string{wg[1], wg[0]}); err != nil || !changed {
		t.Fatalf("reordered: %v, %v", changed, err)
	}
	if b, _ := os.ReadFile(path); string(b) != wg[1]+"\n"+wg[0]+"\n" {
		t.Fatalf("reordered file %q", b)
	}
	other := Output{ID: "wg", Path: filepath.Join(t.TempDir(), "wg"), Kind: candidate.KindWireGuard}
	if _, err := Publish(other, []string{wg[1], wg[0]}); err != nil {
		t.Fatal(err)
	}
	a, _ := os.ReadFile(path)
	b, _ := os.ReadFile(other.Path)
	if string(a) != string(b) {
		t.Fatal("same lines gave different bytes")
	}

	// No temp files are left behind.
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Fatalf("dir holds %v", entries)
	}
}

func TestPublishMasque(t *testing.T) {
	mq := fixtureLines(t, "masque", 1)
	out := Output{ID: "mq", Path: filepath.Join(t.TempDir(), "mq"), Kind: candidate.KindMasque}
	if changed, err := Publish(out, mq); err != nil || !changed {
		t.Fatalf("%v, %v", changed, err)
	}
}

// A failed publish leaves the previous file byte for byte, and its error
// never holds the key.
func TestPublishErrorsKeepFile(t *testing.T) {
	wg := fixtureLines(t, "wireguard", 1)
	mq := fixtureLines(t, "masque", 1)
	path := filepath.Join(t.TempDir(), "wg")
	out := Output{ID: "wg", Path: path, Kind: candidate.KindWireGuard}
	if _, err := Publish(out, wg); err != nil {
		t.Fatal(err)
	}
	key := strings.TrimPrefix(strings.SplitN(wg[0], "@", 2)[0], "wireguard://")
	for name, tc := range map[string]struct {
		lines []string
		want  string
	}{
		"empty":      {nil, `output "wg": no candidates`},
		"other kind": {[]string{wg[0], mq[0]}, `output "wg": line 2: candidate field uri:`},
		"bad field":  {[]string{strings.Replace(wg[0], "mtu=", "mtu=9", 1)}, `output "wg": line 1: candidate field mtu:`},
		"line break": {[]string{wg[0] + "\n" + wg[0]}, `output "wg": line 1: line break inside a candidate line`},
	} {
		changed, err := Publish(out, tc.lines)
		if err == nil || changed || !strings.HasPrefix(err.Error(), tc.want) || strings.Contains(err.Error(), key) {
			t.Errorf("%s: %v, %v", name, changed, err)
		}
		if b, _ := os.ReadFile(path); string(b) != wg[0]+"\n" {
			t.Errorf("%s: file changed to %q", name, b)
		}
	}
	if _, err := Publish(Output{ID: "x", Path: path, Kind: "vless"}, wg); err == nil {
		t.Error("unknown kind published")
	}
}

func TestPublishEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wg")
	out := Output{ID: "wg", Path: path, Kind: candidate.KindWireGuard}
	if _, err := Publish(out, fixtureLines(t, "wireguard", 1)); err != nil {
		t.Fatal(err)
	}
	if changed, err := PublishEmpty(out); err != nil || !changed {
		t.Fatalf("%v %v", changed, err)
	}
	if b, err := os.ReadFile(path); err != nil || len(b) != 0 {
		t.Fatalf("file %q %v", b, err)
	}
	if changed, err := PublishEmpty(out); err != nil || changed {
		t.Fatalf("an empty file rewritten: %v %v", changed, err)
	}
}
