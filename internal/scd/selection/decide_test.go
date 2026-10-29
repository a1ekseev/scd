package selection

import (
	"reflect"
	"testing"
	"time"

	"github.com/a1ekseev/scd/internal/scd/config"
)

// cand is a candidate with key k on endpoint "<tag>:51820" and score s (nil
// when s < 0).
func cand(tag string, k byte, s float64) Candidate {
	c := Candidate{Tag: tag, Key: [32]byte{k}, Endpoint: tag + ":51820"}
	if s >= 0 {
		c.Score = &s
	}
	return c
}

// brief is an action as "keep", "remove:<reason>" or "<to>:<reason>".
func brief(as []Action) []string {
	out := []string{}
	for _, a := range as {
		switch a.Kind {
		case Keep:
			out = append(out, "keep")
		case Remove:
			out = append(out, "remove:"+a.Reason)
		case Replace:
			out = append(out, a.To.Tag+":"+a.Reason)
		}
	}
	return out
}

func healthy(cs ...Candidate) []Slot {
	out := []Slot{}
	for _, c := range cs {
		out = append(out, Slot{Candidate: c, Healthy: true})
	}
	return out
}

func TestDecide(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	cfg := config.SelectionConfig{Strategy: config.StrategyScore, SwitchMarginPercent: 10, SwitchConfirmChecks: 1, DeadPairCooldownMinutes: 4}
	a, b, c, d := cand("a", 1, 1), cand("b", 2, 2), cand("c", 3, 3), cand("d", 4, 4)
	changedA := cand("a2", 1, 1)
	changedA.Endpoint = a.Endpoint
	sameKeyB := cand("b2", 2, 1.5)
	for _, tc := range []struct {
		name       string
		active     []Slot
		ranked     []Candidate
		quarantine map[PairKey]time.Time
		cfg        config.SelectionConfig
		want       []string
	}{
		{"all present", healthy(a, b), []Candidate{a, b, c}, nil, cfg, []string{"keep", "keep"}},
		{"gone, the next ranked", healthy(a, b), []Candidate{b, c, d}, nil, cfg, []string{"c:gone", "keep"}},
		{"changed: its pair is left", healthy(a, b), []Candidate{changedA, b, c}, nil, cfg, []string{"a2:changed", "keep"}},
		{"gone without replacement", healthy(a, b), []Candidate{b}, nil, cfg, []string{"remove:gone", "keep"}},
		{"no two of one key; the key holder may switch", healthy(a, b), []Candidate{sameKeyB, b, d}, nil, cfg, []string{"d:gone", "b2:better"}},
		{"quarantined pair skipped", healthy(a, b), []Candidate{b, c, d}, map[PairKey]time.Time{c.Pair(): now.Add(time.Minute)}, cfg, []string{"d:gone", "keep"}},
		{"quarantine expired", healthy(a, b), []Candidate{b, c, d}, map[PairKey]time.Time{c.Pair(): now}, cfg, []string{"c:gone", "keep"}},
		{"quarantine leaves none", healthy(a, b), []Candidate{b, c}, map[PairKey]time.Time{c.Pair(): now.Add(time.Minute)}, cfg, []string{"remove:gone", "keep"}},
		{"better by the margin", healthy(c), []Candidate{a, c}, nil, cfg, []string{"a:better"}},
		{"better below the margin", healthy(cand("c", 3, 1.05)), []Candidate{a, cand("c", 3, 1.05)}, nil, cfg, []string{"keep"}},
		{"better shares the active's key", healthy(b), []Candidate{sameKeyB, b}, nil, cfg, []string{"b2:better"}},
		{"better holds another active's key", healthy(a, c), []Candidate{a, sameKeyB, c}, nil, cfg, []string{"keep", "b2:better"}},
		{"unhealthy is not switched", []Slot{{Candidate: c}}, []Candidate{a, c}, nil, cfg, []string{"keep"}},
		{"label never switches", healthy(c), []Candidate{a, c}, nil, config.SelectionConfig{Strategy: config.StrategyLabel, SwitchConfirmChecks: 1}, []string{"keep"}},
		{"no metric, no switch", healthy(cand("c", 3, -1)), []Candidate{cand("a", 1, -1), cand("c", 3, -1)}, nil, cfg, []string{"keep"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := State{Quarantine: tc.quarantine}
			if got := brief(Decide(tc.active, tc.ranked, &st, now, tc.cfg)); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// The metric is load for strategy load, the other metric when one of the
// two lacks it.
func TestDecideMetric(t *testing.T) {
	now := time.Unix(0, 0)
	load := func(c Candidate, l int) Candidate { c.Load = &l; return c }
	for _, tc := range []struct {
		name     string
		strategy string
		active   Candidate
		other    Candidate
		want     string
	}{
		{"load by load", config.StrategyLoad, load(cand("a", 1, 1), 50), load(cand("b", 2, 9), 10), "b:better"},
		{"score by score", config.StrategyScore, load(cand("a", 1, 1), 50), load(cand("b", 2, 9), 10), "keep"},
		{"no score: load", config.StrategyScore, load(cand("a", 1, 1), 50), load(cand("b", 2, -1), 10), "b:better"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.SelectionConfig{Strategy: tc.strategy, SwitchConfirmChecks: 1}
			if got := brief(Decide(healthy(tc.active), []Candidate{tc.other, tc.active}, &State{}, now, cfg)); got[0] != tc.want {
				t.Errorf("got %v, want %s", got, tc.want)
			}
		})
	}
}

// switchConfirmChecks ticks in a row; a tick without the better candidate
// starts the series again.
func TestDecideHysteresis(t *testing.T) {
	now := time.Unix(0, 0)
	cfg := config.SelectionConfig{Strategy: config.StrategyScore, SwitchMarginPercent: 10, SwitchConfirmChecks: 3}
	a, c := cand("a", 1, 1), cand("c", 3, 3)
	st := State{}
	tick := func(ranked ...Candidate) string {
		return brief(Decide(healthy(c), ranked, &st, now, cfg))[0]
	}
	for i, tc := range []struct {
		ranked []Candidate
		want   string
	}{
		{[]Candidate{a, c}, "keep"},
		{[]Candidate{a, c}, "keep"},
		{[]Candidate{c}, "keep"}, // interrupted
		{[]Candidate{a, c}, "keep"},
		{[]Candidate{a, c}, "keep"},
		{[]Candidate{a, c}, "a:better"},
	} {
		if got := tick(tc.ranked...); got != tc.want {
			t.Fatalf("tick %d: %s, want %s (better %v)", i+1, got, tc.want, st.Better)
		}
	}
	if len(st.Better) != 0 {
		t.Errorf("series left after the switch: %v", st.Better)
	}
}

func TestQuarantine(t *testing.T) {
	now := time.Unix(1000, 0)
	a := cand("a", 1, 1)
	var st State
	st.QuarantinePair(a, now, 4)
	moved := a
	moved.Tag = "a-new-tag"
	if !st.Quarantined(moved, now.Add(3*time.Minute)) || st.Quarantined(a, now.Add(4*time.Minute)) {
		t.Fatal("quarantine is per pair for deadPairCooldownMinutes")
	}
	other := a
	other.Endpoint = "other:51820"
	if st.Quarantined(other, now) {
		t.Fatal("another endpoint of the key is not quarantined")
	}
	Decide(nil, nil, &st, now.Add(4*time.Minute), config.SelectionConfig{})
	if len(st.Quarantine) != 0 {
		t.Fatalf("expired quarantine kept: %v", st.Quarantine)
	}
}
