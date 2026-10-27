package manifest

import (
	"slices"

	"github.com/a1ekseev/scd/internal/scd/config"
)

// ApplyFilters returns a copy of m with the entries that pass f (01 §9, D29):
// first the country allowlist (an empty list disables it; an entry without
// a country is dropped), then the label filter. Entries dropped by the
// label filter (an include not matched or an exclude matched) are counted
// in FilteredByLabelRegex. Parsed becomes the number of remaining entries;
// skipped lines are not changed. Without filters m is returned as is.
func ApplyFilters(m Manifest, f *config.SubscriptionFiltersConfig) Manifest {
	if f == nil || (len(f.CountryAllowlist) == 0 && f.Label.Empty()) {
		return m
	}
	entries := make([]Entry, 0, len(m.Entries))
	var byCountry, byLabel int
	for _, e := range m.Entries {
		if len(f.CountryAllowlist) > 0 && (e.Country == nil || !slices.Contains(f.CountryAllowlist, e.Country.ISO2)) {
			byCountry++
			continue
		}
		if !f.Label.Match(e.Label) {
			byLabel++
			continue
		}
		entries = append(entries, e)
	}
	m.Entries = entries
	m.Summary.Parsed = len(entries)
	m.Summary.Filtered = byCountry + byLabel
	m.Summary.FilteredByCountry = byCountry
	m.Summary.FilteredByLabelRegex = byLabel
	return m
}
