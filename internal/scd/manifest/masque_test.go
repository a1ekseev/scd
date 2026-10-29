package manifest

import (
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/a1ekseev/scd/internal/common/candidate"
	"github.com/a1ekseev/scd/internal/common/vless"
	"github.com/a1ekseev/scd/internal/scd/config"
	"github.com/a1ekseev/scd/internal/scd/outbound"
)

const mqFixture = "../../../docker/fixtures/masque/candidates.txt"

// mqSecrets are the device keys of the valid fixture lines in every form
// scd or Xray could print: base64url and std base64 PKCS #8, std base64
// SEC 1 and the hex scalar.
func mqSecrets(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile(mqFixture)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for l := range strings.SplitSeq(strings.TrimSpace(string(b)), "\n") {
		c, err := candidate.ParseMasque(l)
		if err != nil {
			continue
		}
		p8, _ := x509.MarshalPKCS8PrivateKey(c.PrivateKey)
		sec1, _ := x509.MarshalECPrivateKey(c.PrivateKey)
		raw, _ := c.PrivateKey.Bytes()
		out = append(out, base64.RawURLEncoding.EncodeToString(p8), base64.StdEncoding.EncodeToString(p8),
			base64.StdEncoding.EncodeToString(sec1), hex.EncodeToString(raw))
	}
	if len(out) == 0 {
		t.Fatal("no keys")
	}
	return out
}

func mqManifest(t *testing.T) Manifest {
	t.Helper()
	b, err := os.ReadFile(mqFixture)
	if err != nil {
		t.Fatal(err)
	}
	return BuildCandidates(candidate.KindMasque, string(b), "mq", "mq", "", time.Unix(0, 0))
}

func TestBuildMasqueFixture(t *testing.T) {
	m := mqManifest(t)
	var tags []string
	for _, e := range m.Entries {
		tags = append(tags, e.Tag)
		if e.Protocol != ProtocolMasque || e.Candidate != nil || !strings.HasPrefix(e.Normalized.Raw, "masque://redacted@") ||
			e.Normalized.Security != "tls" || e.Normalized.Network != "masque" {
			t.Errorf("entry %s: %+v", e.Tag, e)
		}
		if _, ok := e.JSONOutbound.(outbound.Masque); !ok {
			t.Errorf("entry %s outbound %T", e.Tag, e.JSONOutbound)
		}
	}
	for i, prefix := range []string{"mq-nl-amsterdam-mq-", "mq-nl-amsterdam-mq-", "mq-de-frankfurt-mq-", "mq-de-frankfurt-mq-", "mq-us-new-york-mq-"} {
		if i >= len(tags) || !strings.HasPrefix(tags[i], prefix) || len(tags[i]) != len(prefix)+8 {
			t.Fatalf("tags %v, want %d: %s<sha8>", tags, i, prefix)
		}
	}
	if len(tags) != 5 || tags[0] == tags[1] {
		t.Fatalf("tags %v", tags)
	}
	// The h3 and h2 lines of a device share the selection key, not the pair.
	a3, a2, b3 := m.Entries[0].SelectionCandidate(), m.Entries[1].SelectionCandidate(), m.Entries[2].SelectionCandidate()
	if a3.Key != a2.Key || a3.Key == b3.Key || a3.Pair() == a2.Pair() || a3.Transport != "h3" || a2.Transport != "h2" ||
		a3.Priority != 0 || a2.Priority != 1 || a3.Endpoint != "192.0.2.31:443" || a3.Exit != "NL" {
		t.Fatalf("selection %+v %+v", a3, a2)
	}
	type skip struct {
		line int
		code string
	}
	var got []skip
	for _, s := range m.Skipped {
		got = append(got, skip{s.Line, s.Code})
		if s.Raw != "" {
			t.Errorf("skip %d has raw text", s.Line)
		}
	}
	if want := []skip{{6, CodeMasqueInvalid}, {7, vless.CodeUnsupportedScheme}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("skipped %v, want %v", got, want)
	}
	if m.Summary.MasqueInvalid != 1 || m.Summary.WireGuardInvalid != 0 || m.Summary.Parsed != 5 {
		t.Fatalf("summary %+v", m.Summary)
	}
	if d := m.Skipped[0].Details; len(d) != 1 || d[0] != "candidate field transport: must be h3 or h2" {
		t.Fatalf("details %v", d)
	}
}

// No JSON, fmt verb or slog handler prints a device key.
func TestMasqueHidesKeys(t *testing.T) {
	m := mqManifest(t)
	var out strings.Builder
	b, _ := json.Marshal(m)
	out.Write(b)
	b, _ = json.Marshal(m.ParseLog())
	out.Write(b)
	text := slog.New(slog.NewTextHandler(&out, nil))
	js := slog.New(slog.NewJSONHandler(&out, nil))
	for _, e := range m.Entries {
		for _, v := range []any{e, &e, e.JSONOutbound, e.SelectionCandidate()} {
			fmt.Fprintf(&out, "%v %+v %#v %s\n", v, v, v, v)
			text.Info("x", "v", v)
			js.Info("x", "v", v)
		}
	}
	for _, f := range mqSecrets(t) {
		if strings.Contains(out.String(), f) {
			t.Fatalf("output holds key form %q", f)
		}
	}
	if !strings.Contains(out.String(), "<redacted>") || !strings.Contains(out.String(), "masque://redacted@") {
		t.Fatal("nothing redacted was printed")
	}
}

func TestLoadSourcesMasqueRefused(t *testing.T) {
	p := filepath.Join(t.TempDir(), "mq")
	if err := os.WriteFile(p, []byte("x\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	mq := config.SourceConfig{ID: "mq", Type: config.SourceMasque, Input: p, Enabled: true, MaxInputBytes: 1 << 20}
	never := &fakeLoader{}
	loaded, failed := loadAll(never.load, mq)
	if len(loaded) != 0 || len(failed) != 1 || failed[0].Code != CodeMasqueInputRefused || len(never.calls) != 0 {
		t.Fatalf("loaded %v failed %+v calls %v", loaded, failed, never.calls)
	}
}

// FromSource builds a masque source and filters it by exit country.
func TestFromSourceMasque(t *testing.T) {
	b, err := os.ReadFile(mqFixture)
	if err != nil {
		t.Fatal(err)
	}
	src := config.SourceConfig{ID: "mq", Type: config.SourceMasque, Filters: &config.SourceFiltersConfig{CountryAllowlist: []string{"DE"}}}
	m, err := FromSource(LoadedSource{Config: src, Content: string(b)}, time.Unix(0, 0))
	if err != nil || len(m.Entries) != 2 || m.Summary.FilteredByCountry != 3 {
		t.Fatalf("manifest %+v, %v", m.Summary, err)
	}
}
