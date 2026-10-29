// Package selection chooses the active candidates of a wireguard source:
// it filters them (entry country, then exit country, then label), ranks
// them by the source's selection.strategy and picks at most maxActive, never
// two with the same key. It does no I/O.
package selection

import (
	"cmp"
	"slices"
	"strings"

	"github.com/a1ekseev/scd/internal/scd/config"
	"github.com/a1ekseev/scd/internal/scd/country"
)

// Candidate is what selection knows about one candidate.
type Candidate struct {
	Tag, Label string
	// Entry and Exit are the ISO2 metadata ("" without).
	Entry, Exit string
	Load        *int
	Score       *float64
	// Key is the candidate's interface public key (never the private key):
	// two active candidates never share it.
	Key [32]byte
	// Endpoint is the peer "host:port".
	Endpoint string
}

// Reason is why Filter dropped a candidate.
type Reason int

// Filter results.
const (
	Pass Reason = iota
	DroppedByEntry
	DroppedByExit
	DroppedByLabel
)

// countryOf is the ISO2 of meta, else of the label flag, else "".
func countryOf(meta, label string) string {
	if meta != "" {
		return meta
	}
	if info, ok := country.FromLabel(label); ok {
		return info.ISO2
	}
	return ""
}

// Filter applies f to c: entryCountryAllowlist (the entry metadata, else
// the label flag), then countryAllowlist (the exit metadata, else the label
// flag), then the label filter. A candidate without a country fails a
// non-empty country filter. Nil f passes everything.
func Filter(c Candidate, f *config.SourceFiltersConfig) Reason {
	switch {
	case f == nil:
		return Pass
	case len(f.EntryCountryAllowlist) > 0 && !slices.Contains(f.EntryCountryAllowlist, countryOf(c.Entry, c.Label)):
		return DroppedByEntry
	case len(f.CountryAllowlist) > 0 && !slices.Contains(f.CountryAllowlist, countryOf(c.Exit, c.Label)):
		return DroppedByExit
	case !f.Label.Match(c.Label):
		return DroppedByLabel
	}
	return Pass
}

// orNil compares two optional keys: a present key sorts before a missing one.
func orNil[T cmp.Ordered](a, b *T) int {
	switch {
	case a == nil && b == nil:
		return 0
	case a == nil:
		return 1
	case b == nil:
		return -1
	}
	return cmp.Compare(*a, *b)
}

// emptyLast compares labels with strings.Compare, an empty label after the
// others.
func emptyLast(a, b string) int {
	if (a == "") != (b == "") {
		if a == "" {
			return 1
		}
		return -1
	}
	return strings.Compare(a, b)
}

// Rank returns items ordered by strategy (stable):
//
//   - score: score, load, label, tag;
//   - load: load, score, label, tag;
//   - label: label, tag.
//
// A candidate without a sort key (an empty label included) comes after the
// candidates with it; labels and tags compare with strings.Compare.
func Rank[T any](items []T, strategy string, of func(T) Candidate) []T {
	out := slices.Clone(items)
	slices.SortStableFunc(out, func(x, y T) int {
		a, b := of(x), of(y)
		names := cmp.Or(emptyLast(a.Label, b.Label), strings.Compare(a.Tag, b.Tag))
		switch strategy {
		case config.StrategyScore:
			return cmp.Or(orNil(a.Score, b.Score), orNil(a.Load, b.Load), names)
		case config.StrategyLoad:
			return cmp.Or(orNil(a.Load, b.Load), orNil(a.Score, b.Score), names)
		}
		return names
	})
	return out
}

// Pick returns the first maxActive items of ranked, skipping an item whose
// key an earlier picked item has.
func Pick[T any](ranked []T, maxActive int, of func(T) Candidate) []T {
	var out []T
	keys := map[[32]byte]bool{}
	for _, it := range ranked {
		if len(out) == maxActive {
			break
		}
		k := of(it).Key
		if keys[k] {
			continue
		}
		keys[k] = true
		out = append(out, it)
	}
	return out
}
