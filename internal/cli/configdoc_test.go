package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	gtlconfig "github.com/a1ekseev/scd/internal/gtl/config"
	scdconfig "github.com/a1ekseev/scd/internal/scd/config"
	sfcconfig "github.com/a1ekseev/scd/internal/sfc/config"
)

// hiddenKeys lists the config keys behind `json:"-"` fields, which print
// through their own MarshalJSON (labelfilter.Filter). The keys accept a
// string or a list, so they are leaves like the TS string keys.
var hiddenKeys = map[string][]string{
	"config.SubscriptionFiltersConfig.Label": {"labelIncludeRegex", "labelExcludeRegex"},
	"config.Output.Filter":                   {"labelIncludeRegex", "labelExcludeRegex"},
}

// TestConfigMigrationDocCoversKeys checks (D32) that every config key that
// exists only in the Go schema or only in the TS schema
// (testdata/reference/<svc>/config-keys.json) is mentioned by its full path
// in the service's section of docs/CONFIG-MIGRATION.md.
func TestConfigMigrationDocCoversKeys(t *testing.T) {
	doc, err := os.ReadFile(filepath.Join("..", "..", "docs", "CONFIG-MIGRATION.md"))
	if err != nil {
		t.Fatal(err)
	}
	services := []struct {
		name    string // reference dir
		section string // level-2 heading in the doc
		typ     reflect.Type
	}{
		{"scd", "scd", reflect.TypeFor[scdconfig.AppConfig]()},
		{"sfc", "scd sfc", reflect.TypeFor[sfcconfig.Config]()},
		{"gtl", "scd gtl", reflect.TypeFor[gtlconfig.Config]()},
	}
	for _, svc := range services {
		t.Run(svc.name, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "reference", svc.name, "config-keys.json"))
			if err != nil {
				t.Fatal(err)
			}
			var tsKeys []string
			if err := json.Unmarshal(raw, &tsKeys); err != nil {
				t.Fatal(err)
			}
			goKeys := configKeys(t, svc.typ)
			section := docSection(string(doc), svc.section)
			if section == "" {
				t.Fatalf("docs/CONFIG-MIGRATION.md has no section %q", "## "+svc.section)
			}
			for _, k := range symmetricDiff(tsKeys, goKeys) {
				if !strings.Contains(section, "`"+k+"`") {
					t.Errorf("key `%s` differs between the TS and Go schemas but is not mentioned in section %q", k, "## "+svc.section)
				}
			}
		})
	}
}

// TestConfigKeysWalk pins the reflect walk to the TS key format.
func TestConfigKeysWalk(t *testing.T) {
	keys := configKeys(t, reflect.TypeFor[scdconfig.AppConfig]())
	for _, want := range []string{
		"subscriptions[].fetchHeaders",
		"subscriptions[].filters.countryAllowlist[]",
		"subscriptions[].filters.labelExcludeRegex",
		"subscriptions[].target.balancerSelection.balancerTag",
		"statusServer.remoteInstances[].apiUrl",
		"subscriptions[].target.inboundSocks.portRange.end",
	} {
		if !slices.Contains(keys, want) {
			t.Errorf("walk does not produce %q", want)
		}
	}
}

// configKeys returns the sorted leaf key paths of a config type in the
// format of config-keys.json: `a.b`, lists of objects as `a[].b`, lists of
// scalars as `a[]`, maps as the key itself.
func configKeys(t *testing.T, typ reflect.Type) []string {
	t.Helper()
	var keys []string
	var walk func(prefix string, typ reflect.Type)
	walk = func(prefix string, typ reflect.Type) {
		for typ.Kind() == reflect.Pointer {
			typ = typ.Elem()
		}
		for f := range typ.Fields() {
			name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
			if name == "-" {
				id := typ.String() + "." + f.Name
				hidden, ok := hiddenKeys[id]
				if !ok {
					t.Fatalf("field %s is hidden from JSON; list its config keys in hiddenKeys", id)
				}
				for _, k := range hidden {
					keys = append(keys, prefix+k)
				}
				continue
			}
			if name == "" {
				t.Fatalf("field %s.%s has no json tag", typ, f.Name)
			}
			path := prefix + name
			ft := f.Type
			for ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			switch ft.Kind() {
			case reflect.Struct:
				walk(path+".", ft)
			case reflect.Slice:
				elem := ft.Elem()
				for elem.Kind() == reflect.Pointer {
					elem = elem.Elem()
				}
				if elem.Kind() == reflect.Struct {
					walk(path+"[].", elem)
				} else {
					keys = append(keys, path+"[]")
				}
			default: // scalars and maps (fetchHeaders)
				keys = append(keys, path)
			}
		}
	}
	walk("", typ)
	slices.Sort(keys)
	return keys
}

func symmetricDiff(a, b []string) []string {
	var out []string
	for _, k := range a {
		if !slices.Contains(b, k) {
			out = append(out, k)
		}
	}
	for _, k := range b {
		if !slices.Contains(a, k) {
			out = append(out, k)
		}
	}
	slices.Sort(out)
	return out
}

// docSection returns the text of the level-2 section `## <title>` up to the
// next level-2 heading, or "" if there is none.
func docSection(doc, title string) string {
	var b strings.Builder
	in := false
	for line := range strings.Lines(doc) {
		if strings.HasPrefix(line, "## ") {
			if in {
				break
			}
			in = strings.TrimSpace(line) == "## "+title
			continue
		}
		if in {
			b.WriteString(line)
		}
	}
	return b.String()
}
