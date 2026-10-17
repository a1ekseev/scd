package normalize

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/a1ekseev/scd/internal/common/subscription"
	"github.com/a1ekseev/scd/internal/common/vless"
)

const rawFixed = "vless://x@example.com:443?type=tcp#raw"

func TestSha8Vena(t *testing.T) {
	if got := Sha8("Вена"); got != "3ed0e2a1" {
		t.Fatalf("got %s", got)
	}
}

func TestBuildTagTable(t *testing.T) {
	long := "abcdefghijklmnopqrstuvwxyz0123-56789"
	tests := []struct{ name, label, address, city string }{
		{"vena", "🇦🇹 Вена, Австрия", "example.com", "vless-at-3ed0e2a1"},
		{"extra part", "🇩🇪 Германия, Extra", "", "vless-de-c2574453"},
		{"flag only", "🇩🇪", "example.com", "vless-de-example-com"},
		{"extra whole", "🇩🇪 Extra Whitelist", "example.com", "vless-de-example-com"},
		{"cafe", "Café Zürich", "", "vless-xx-cafe-zurich"},
		{"space before flag", "  🇩🇪Berlin", "", "vless-xx-berlin"},
		{"two flags", "🇩🇪🇦🇹 Wien", "", "vless-de-wien"},
		{"flag outside map", "🇽🇰 Pristina", "", "vless-xx-pristina"},
		{"slug cut", long, "", "vless-xx-abcdefghijklmnopqrstuvwxyz0123-5"},
		{"ipv6", "", "2001:db8::1", "vless-xx-2001-db8-1"},
		{"endpoint", "", "", "vless-xx-endpoint"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			want := tc.city + "-tcp-tls-" + Sha8(rawFixed)
			if got := BuildTag(tc.label, tc.address, "tcp-tls", rawFixed); got != want {
				t.Fatalf("got %s, want %s", got, want)
			}
		})
	}
}

func TestSlugifyCutKeepsTrailingDash(t *testing.T) {
	if got := Slugify("abcdefghijklmnopqrstuvwxyz01234-56"); got != "abcdefghijklmnopqrstuvwxyz01234-" {
		t.Fatalf("got %q", got)
	}
}

func TestSameCountryDifferentCityDifferentTags(t *testing.T) {
	a := BuildTag("🇦🇹 Вена", "h", "tcp-tls", rawFixed)
	b := BuildTag("🇦🇹 Зальцбург", "h", "tcp-tls", rawFixed)
	if a == b {
		t.Fatalf("tags equal: %s", a)
	}
}

func TestTagDependsOnRawWhitespace(t *testing.T) {
	a := BuildTag("🇩🇪 Berlin", "h", "tcp-tls", rawFixed)
	b := BuildTag("🇩🇪 Berlin", "h", "tcp-tls", " "+rawFixed)
	if a == b {
		t.Fatalf("tags equal: %s", a)
	}
}

func TestInferCity(t *testing.T) {
	tests := []struct {
		label, city string
		ok          bool
	}{
		{"🇦🇹 Вена, Австрия", "Вена", true},
		{"🇩🇪 Extraordinary", "", false},
		{"🇩🇪 EXTRA", "", false},
		{"🇩🇪, Berlin", "", false},
		{"", "", false},
		{"  🇩🇪 Berlin", "Berlin", true},
	}
	for _, tc := range tests {
		city, ok := InferCity(tc.label)
		if city != tc.city || ok != tc.ok {
			t.Errorf("%q: got %q %v", tc.label, city, ok)
		}
	}
}

func TestBuildTagRealShapes(t *testing.T) {
	tests := []struct{ label, profile, want string }{
		{"🇩🇪DE-1-X", "xhttp-tls", "vless-de-de-1-x-xhttp-tls-"},
		{"🇺🇸 United States, Miami", "tcp-tls", "vless-us-united-states-tcp-tls-"},
		{"🇳🇬 Nigeria, Lagos", "tcp-tls", "vless-xx-nigeria-tcp-tls-"},
		{"🇨🇴 Colombia, Bogotá", "tcp-tls", "vless-co-colombia-tcp-tls-"},
		{"🇩🇪 Germany, Berlin ^~2~^", "tcp-tls", "vless-de-germany-tcp-tls-"},
		{"🇩🇪 Berlin ^~2~^", "tcp-tls", "vless-de-berlin-2-tcp-tls-"},
	}
	for _, tc := range tests {
		want := tc.want + Sha8(rawFixed)
		if got := BuildTag(tc.label, "h.example.com", tc.profile, rawFixed); got != want {
			t.Errorf("%q: got %s, want %s", tc.label, got, want)
		}
	}
}

func parseLine(t *testing.T, raw string, n int) vless.Link {
	t.Helper()
	link, skip := vless.Parse(subscription.Line{Number: n, Raw: raw, Trimmed: strings.TrimSpace(raw)})
	if skip != nil {
		t.Fatalf("line %d skipped: %+v", n, skip)
	}
	return link
}

func TestBuildTagMatchesTSCorpus(t *testing.T) {
	data, err := os.ReadFile("../../../testdata/reference/scd/corpus/ts-parse.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Line int    `json:"line"`
		Raw  string `json:"raw"`
		TS   struct {
			OK      bool   `json:"ok"`
			Profile string `json:"profile"`
			Tag     string `json:"tag"`
		} `json:"ts"`
	}
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, c := range cases {
		// Line 55 holds a tab inside the label: TS stripped it (WHATWG URL),
		// Go keeps it by D9 (see vless parse_test), so the tags differ by design.
		if !c.TS.OK || c.Line == 55 {
			continue
		}
		link, skip := vless.Parse(subscription.Line{Number: c.Line, Raw: c.Raw, Trimmed: strings.TrimSpace(c.Raw)})
		if skip != nil {
			continue
		}
		if got := BuildTag(link.Label, link.Address, c.TS.Profile, link.Raw); got != c.TS.Tag {
			t.Errorf("line %d: got %s, want %s", c.Line, got, c.TS.Tag)
		}
		checked++
	}
	if checked < 40 {
		t.Fatalf("only %d corpus lines compared", checked)
	}
}

func TestBuildTagMatchesTSProvider2(t *testing.T) {
	data, err := os.ReadFile("../../../testdata/reference/scd/generate/real-shapes-provider2.manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Entries []struct {
			Tag     string `json:"tag"`
			Profile string `json:"profile"`
			Line    int    `json:"line"`
			Norm    struct {
				Raw string `json:"raw"`
			} `json:"normalized"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	if len(m.Entries) != 50 {
		t.Fatalf("entries: %d", len(m.Entries))
	}
	for _, e := range m.Entries {
		link := parseLine(t, e.Norm.Raw, e.Line)
		if got := BuildTag(link.Label, link.Address, e.Profile, link.Raw); got != e.Tag {
			t.Errorf("line %d: got %s, want %s", e.Line, got, e.Tag)
		}
	}
}
