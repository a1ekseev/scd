package manifest

import (
	"bytes"
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

	"github.com/a1ekseev/scd/internal/common/vless"
	"github.com/a1ekseev/scd/internal/scd/config"
	"github.com/a1ekseev/scd/internal/scd/outbound"
)

const wgFixture = "../../../docker/fixtures/wireguard/candidates.txt"

// wgSecrets are the private and preshared keys of the fixture.
func wgSecrets() []string {
	var out []string
	for _, b := range []byte{0x11, 0x12, 0x14, 0x15, 0x16, 0x17, 0x18, 0x19, 0x35} {
		out = append(out, base64.StdEncoding.EncodeToString([]byte(strings.Repeat(string(rune(b)), 32))))
	}
	return out
}

func wgManifest(t *testing.T) Manifest {
	t.Helper()
	b, err := os.ReadFile(wgFixture)
	if err != nil {
		t.Fatal(err)
	}
	return BuildWireGuard(string(b), "wg", "wg", "UseIPv4", time.Unix(0, 0))
}

func TestBuildWireGuardFixture(t *testing.T) {
	m := wgManifest(t)
	var tags []string
	for _, e := range m.Entries {
		tags = append(tags, e.Tag)
		if e.Protocol != ProtocolWireGuard || e.Candidate == nil || !strings.HasPrefix(e.Normalized.Raw, "wireguard://redacted@") {
			t.Errorf("entry %s: %+v", e.Tag, e)
		}
		if _, ok := e.JSONOutbound.(outbound.WireGuard); !ok {
			t.Errorf("entry %s outbound %T", e.Tag, e.JSONOutbound)
		}
	}
	for i, prefix := range []string{
		"wg-nl-amsterdam-wg-", "wg-nl-amsterdam-wg-", "wg-nl-rotterdam-wg-", "wg-nl-amsterdam-wg-",
		"wg-ch-zurich-wg-", "wg-xx-warp-wg-", "wg-xx-vpn-example-com-wg-",
	} {
		if i >= len(tags) || !strings.HasPrefix(tags[i], prefix) || len(tags[i]) != len(prefix)+8 {
			t.Fatalf("tags %v, want %d: %s<sha8>", tags, i, prefix)
		}
	}
	if len(tags) != 7 {
		t.Fatalf("tags %v", tags)
	}
	if e := m.Entries[6]; e.Label != "vpn.example.com:51820" || e.Country != nil {
		t.Errorf("unlabelled entry %+v", e)
	}
	if e := m.Entries[3]; e.Country == nil || e.Country.ISO2 != "NL" {
		t.Errorf("exit country %+v", e.Country)
	}
	type skip struct {
		line int
		code string
	}
	var skips []skip
	for _, s := range m.Skipped {
		skips = append(skips, skip{s.Line, s.Code})
		if s.Code != vless.CodeDuplicateTag && s.Raw != "" {
			t.Errorf("skip %d keeps raw %q", s.Line, s.Raw)
		}
	}
	want := []skip{{8, CodeWireGuardInvalid}, {9, CodeWireGuardInvalid}, {10, vless.CodeUnsupportedScheme}, {11, vless.CodeDuplicateTag}}
	if !reflect.DeepEqual(skips, want) {
		t.Fatalf("skips %v, want %v", skips, want)
	}
	// remoteDNS "not-an-ip" would panic Xray: the parser drops it first.
	if d := m.Skipped[1].Details; len(d) != 1 || d[0] != "candidate field dns: each must be an IP" {
		t.Errorf("details %v", d)
	}
	if s := m.Summary; s.Parsed != 7 || s.Skipped != 4 || s.WireGuardInvalid != 2 || s.UnsupportedScheme != 1 || s.DuplicateTag != 1 {
		t.Errorf("summary %+v", s)
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range wgSecrets() {
		if strings.Contains(string(b), k) {
			t.Fatalf("manifest JSON holds key %s", k)
		}
	}
}

func TestWireGuardFilters(t *testing.T) {
	m := wgManifest(t)
	for _, tc := range []struct {
		name             string
		f                config.SourceFiltersConfig
		keep             int
		byCountry, label int
	}{
		{"exit NL", config.SourceFiltersConfig{CountryAllowlist: []string{"NL"}}, 4, 3, 0},
		{"entry CH", config.SourceFiltersConfig{EntryCountryAllowlist: []string{"CH"}}, 2, 5, 0},
		{"entry CH exit NL", config.SourceFiltersConfig{EntryCountryAllowlist: []string{"CH"}, CountryAllowlist: []string{"NL"}}, 1, 6, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := applyCandidateFilters(m, &tc.f)
			if len(got.Entries) != tc.keep || got.Summary.Parsed != tc.keep || got.Summary.FilteredByCountry != tc.byCountry ||
				got.Summary.FilteredByLabelRegex != tc.label {
				t.Fatalf("kept %d, summary %+v", len(got.Entries), got.Summary)
			}
		})
	}
}

func TestLoadSourcesWireGuardPrivate(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "wg")
	if err := os.WriteFile(p, []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	wg := config.SourceConfig{ID: "wg", Type: config.SourceWireGuard, Input: p, Enabled: true, MaxInputBytes: 1 << 20}
	never := func() *fakeLoader { return &fakeLoader{} }()
	loaded, failed := loadAll(never.load, wg)
	if len(loaded) != 0 || len(failed) != 1 || failed[0].Code != CodeWireGuardInputRefused || len(never.calls) != 0 {
		t.Fatalf("loaded %v failed %+v calls %v", loaded, failed, never.calls)
	}
	if err := os.Chmod(p, 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, failed = loadAll(never.load, wg)
	if len(loaded) != 1 || len(failed) != 0 || loaded[0].Content != "x\n" || loaded[0].Source != p || len(never.calls) != 0 {
		t.Fatalf("loaded %+v failed %+v", loaded, failed)
	}
	if _, err := LoadPrivate("wg", 1<<10); err == nil {
		t.Fatal("relative path accepted")
	}
}

// No fmt verb and no slog handler prints a key of an entry, its outbound or
// its candidate.
func TestWireGuardFormattingHidesKeys(t *testing.T) {
	m := wgManifest(t)
	var forms []string
	for _, b := range []byte{0x11, 0x12, 0x14, 0x15, 0x16, 0x17, 0x18, 0x19, 0x35} {
		k := bytes.Repeat([]byte{b}, 32)
		forms = append(forms, base64.StdEncoding.EncodeToString(k), base64.RawURLEncoding.EncodeToString(k),
			hex.EncodeToString(k), fmt.Sprint([32]byte(k)), fmt.Sprintf("%#v", [32]byte(k)))
	}
	var out strings.Builder
	text := slog.New(slog.NewTextHandler(&out, nil))
	js := slog.New(slog.NewJSONHandler(&out, nil))
	for _, e := range m.Entries {
		for _, v := range []any{e, &e, e.JSONOutbound, *e.Candidate, e.Candidate, e.SelectionCandidate()} {
			fmt.Fprintf(&out, "%v %+v %#v %s\n", v, v, v, v)
			text.Info("x", "v", v)
			js.Info("x", "v", v)
		}
	}
	for _, f := range forms {
		if strings.Contains(out.String(), f) {
			t.Fatalf("formatted output holds key form %q", f)
		}
	}
	if !strings.Contains(out.String(), "<redacted>") || !strings.Contains(out.String(), "wireguard://redacted@") {
		t.Fatal("nothing redacted was printed")
	}
}

// A label without a flag ("☁️ WARP", as wgp WARP lines have) is accepted
// like "WARP": no country without exit metadata, the label as city and the
// same tag.
func TestWireGuardLabelWithoutFlag(t *testing.T) {
	b, err := os.ReadFile(wgFixture)
	if err != nil {
		t.Fatal(err)
	}
	line, _, _ := strings.Cut(strings.SplitN(string(b), "\n", 2)[0], "#")
	line = strings.Replace(line, "&entry=NL&exit=NL", "", 1)
	var tags []string
	for _, label := range []string{"WARP", "%E2%98%81%EF%B8%8F%20WARP"} {
		m := BuildWireGuard(line+"#"+label, "wg", "wg", "UseIPv4", time.Unix(0, 0))
		if len(m.Entries) != 1 || len(m.Skipped) != 0 {
			t.Fatalf("%s: entries %d, skipped %+v", label, len(m.Entries), m.Skipped)
		}
		e := m.Entries[0]
		if e.Country != nil || !strings.HasSuffix(e.City, "WARP") || !strings.HasSuffix(e.Label, "WARP") {
			t.Fatalf("entry %q country %v city %q", e.Label, e.Country, e.City)
		}
		tags = append(tags, e.Tag)
	}
	if tags[0] != tags[1] {
		t.Fatalf("tags %q", tags)
	}
}
