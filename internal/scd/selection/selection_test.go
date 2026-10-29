package selection

import (
	"reflect"
	"regexp"
	"testing"

	"github.com/a1ekseev/scd/internal/common/labelfilter"
	"github.com/a1ekseev/scd/internal/common/regexlit"
	"github.com/a1ekseev/scd/internal/scd/config"
)

func self(c Candidate) Candidate { return c }

func tags(cs []Candidate) []string {
	out := []string{}
	for _, c := range cs {
		out = append(out, c.Tag)
	}
	return out
}

func TestFilter(t *testing.T) {
	re, err := regexlit.Compile("/Backup/i")
	if err != nil {
		t.Fatal(err)
	}
	f := &config.SourceFiltersConfig{
		EntryCountryAllowlist: []string{"CH"}, CountryAllowlist: []string{"NL"},
		Label: labelfilter.Filter{Exclude: []*regexp.Regexp{re}, ExcludeSrc: []string{"/Backup/i"}},
	}
	for _, tc := range []struct {
		name string
		c    Candidate
		want Reason
	}{
		{"metadata", Candidate{Entry: "CH", Exit: "NL", Label: "🇩🇪 Berlin"}, Pass},
		{"entry from the flag", Candidate{Exit: "NL", Label: "🇨🇭 Zurich"}, Pass},
		{"exit from the flag", Candidate{Entry: "CH", Label: "🇳🇱 Amsterdam"}, Pass},
		{"entry first", Candidate{Entry: "NL", Exit: "DE"}, DroppedByEntry},
		{"exit", Candidate{Entry: "CH", Exit: "CH"}, DroppedByExit},
		{"no country", Candidate{Label: "WARP"}, DroppedByEntry},
		{"label", Candidate{Entry: "CH", Exit: "NL", Label: "Backup"}, DroppedByLabel},
	} {
		if got := Filter(tc.c, f); got != tc.want {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
	if Filter(Candidate{}, nil) != Pass || Filter(Candidate{}, &config.SourceFiltersConfig{}) != Pass {
		t.Error("empty filters drop")
	}
}

func TestRank(t *testing.T) {
	n := func(v int) *int { return &v }
	f := func(v float64) *float64 { return &v }
	cs := []Candidate{
		{Tag: "a", Label: "z", Score: f(2), Load: n(10)},
		{Tag: "b", Label: "y"},
		{Tag: "c", Label: "x", Score: f(1), Load: n(50)},
		{Tag: "d", Label: "w", Load: n(10)},
		{Tag: "e", Label: "x", Score: f(1), Load: n(50)},
		{Tag: "f", Label: "v", Score: f(2), Load: n(5)},
	}
	for _, tc := range []struct {
		strategy string
		want     []string
	}{
		// score, load, label, tag; no score sorts last.
		{config.StrategyScore, []string{"c", "e", "f", "a", "d", "b"}},
		// load, score, label, tag; no load sorts last.
		{config.StrategyLoad, []string{"f", "a", "d", "c", "e", "b"}},
		{config.StrategyLabel, []string{"f", "d", "c", "e", "b", "a"}},
	} {
		if got := tags(Rank(cs, tc.strategy, self)); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: %v, want %v", tc.strategy, got, tc.want)
		}
	}
	if cs[0].Tag != "a" {
		t.Error("Rank changed its input")
	}
	// An empty label sorts last, under label and in the tie-break.
	unlabelled := []Candidate{{Tag: "a"}, {Tag: "b", Label: "z"}, {Tag: "c", Label: "a"}}
	for _, s := range []string{config.StrategyLabel, config.StrategyScore} {
		if got := tags(Rank(unlabelled, s, self)); !reflect.DeepEqual(got, []string{"c", "b", "a"}) {
			t.Errorf("%s with an empty label: %v", s, got)
		}
	}
}

func TestPick(t *testing.T) {
	cs := []Candidate{{Tag: "a", Key: [32]byte{1}}, {Tag: "b", Key: [32]byte{1}}, {Tag: "c", Key: [32]byte{2}}, {Tag: "d", Key: [32]byte{3}}}
	for _, tc := range []struct {
		max  int
		want []string
	}{{1, []string{"a"}}, {2, []string{"a", "c"}}, {3, []string{"a", "c", "d"}}, {9, []string{"a", "c", "d"}}} {
		if got := tags(Pick(cs, tc.max, self)); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("max %d: %v, want %v", tc.max, got, tc.want)
		}
	}
}
