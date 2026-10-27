package manifest

import (
	"reflect"
	"regexp"
	"testing"

	"github.com/a1ekseev/scd/internal/common/labelfilter"
	"github.com/a1ekseev/scd/internal/common/regexlit"
	"github.com/a1ekseev/scd/internal/scd/config"
)

func compile(t *testing.T, exprs ...string) ([]*regexp.Regexp, []string) {
	t.Helper()
	var res []*regexp.Regexp
	for _, e := range exprs {
		re, err := regexlit.Compile(e)
		if err != nil {
			t.Fatal(err)
		}
		res = append(res, re)
	}
	return res, exprs
}

func filters(t *testing.T, allow []string, include, exclude []string) *config.SubscriptionFiltersConfig {
	t.Helper()
	var f labelfilter.Filter
	f.Include, f.IncludeSrc = compile(t, include...)
	f.Exclude, f.ExcludeSrc = compile(t, exclude...)
	return &config.SubscriptionFiltersConfig{CountryAllowlist: allow, Label: f}
}

func labels(m Manifest) []string {
	var out []string
	for _, e := range m.Entries {
		out = append(out, e.Label)
	}
	return out
}

func assertFiltered(t *testing.T, m Manifest, wantLabels []string, byCountry, byLabel int) {
	t.Helper()
	if got := labels(m); !reflect.DeepEqual(got, wantLabels) {
		t.Fatalf("labels %q, want %q", got, wantLabels)
	}
	s := m.Summary
	if s.Parsed != len(wantLabels) || s.FilteredByCountry != byCountry || s.FilteredByLabelRegex != byLabel ||
		s.Filtered != byCountry+byLabel {
		t.Fatalf("summary %+v, want byCountry %d byLabel %d", s, byCountry, byLabel)
	}
}

func TestFilterByCountryAllowlist(t *testing.T) { // P3
	m := links(t, "🇦🇹 Вена, Австрия, Extra", "🇩🇪 Берлин, Германия, Extra")
	assertFiltered(t, ApplyFilters(m, filters(t, []string{"DE"}, nil, nil)), []string{"🇩🇪 Берлин, Германия, Extra"}, 1, 0)
}

func TestFilterByLabelRegex(t *testing.T) { // P4, regex rewritten for RE2 (D8)
	m := links(t, "🇦🇹 Вена, Австрия, Extra", "🇩🇪 Берлин, Германия, Extra Whitelist#2")
	f := ApplyFilters(m, filters(t, nil, []string{`/,\s*Extra\s*$/i`}, nil))
	assertFiltered(t, f, []string{"🇦🇹 Вена, Австрия, Extra"}, 0, 1)
}

func TestFilterCountryThenLabel(t *testing.T) { // P5
	m := links(t, "🇦🇹 Вена, Австрия, Extra", "🇩🇪 Берлин, Германия, Extra", "🇳🇱 Амстердам, Нидерланды, Extra Whitelist#2")
	f := ApplyFilters(m, filters(t, []string{"DE", "NL"}, []string{`/,\s*Extra\s*$/i`}, nil))
	assertFiltered(t, f, []string{"🇩🇪 Берлин, Германия, Extra"}, 1, 1)
	if f.Summary.Skipped != 0 || len(f.Skipped) != 0 || f.Summary.TotalLines != 3 {
		t.Fatalf("filters changed skipped: %+v", f.Summary)
	}
}

func TestFilterDropsEntriesWithoutCountry(t *testing.T) {
	m := links(t, "no flag", "🇩🇪 Berlin")
	assertFiltered(t, ApplyFilters(m, filters(t, []string{"DE"}, nil, nil)), []string{"🇩🇪 Berlin"}, 1, 0)
}

func TestEmptyAllowlistDisablesFilter(t *testing.T) {
	m := links(t, "no flag", "🇩🇪 Berlin")
	for _, f := range []*config.SubscriptionFiltersConfig{nil, {}, filters(t, []string{}, nil, nil)} {
		if got := ApplyFilters(m, f); !reflect.DeepEqual(got, m) {
			t.Fatalf("filters %+v changed the manifest: %+v", f, got.Summary)
		}
	}
	// An empty allowlist with a label filter filters by label only.
	assertFiltered(t, ApplyFilters(m, filters(t, []string{}, []string{"Berlin"}, nil)), []string{"🇩🇪 Berlin"}, 0, 1)
}

func TestExcludeCountsAsLabelFiltered(t *testing.T) { // D29
	m := links(t, "DE", "DE Extra", "NL")
	f := ApplyFilters(m, filters(t, nil, []string{"/DE/"}, []string{"/Extra/i"}))
	assertFiltered(t, f, []string{"DE"}, 0, 2)
}

func TestFilterDoesNotMutateInput(t *testing.T) {
	m := links(t, "🇦🇹 Wien", "🇩🇪 Berlin")
	before := labels(m)
	_ = ApplyFilters(m, filters(t, []string{"DE"}, nil, nil))
	if !reflect.DeepEqual(labels(m), before) || m.Summary.Parsed != 2 {
		t.Fatalf("input manifest changed: %q %+v", labels(m), m.Summary)
	}
}
