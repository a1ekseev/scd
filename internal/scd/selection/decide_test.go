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

// mq is a masque candidate of key k on one endpoint with transport and
// priority.
func mq(tag string, k byte, transport string, priority int) Candidate {
	return Candidate{Tag: tag, Label: "🇳🇱 Amsterdam", Key: [32]byte{k}, Endpoint: "192.0.2.31:443", Transport: transport, Priority: priority}
}

// The candidates of one key follow each other by priority; the quarantine
// is per transport; a healthy h2 stays after the h3 quarantine expires.
func TestMasqueTransports(t *testing.T) {
	now := time.Unix(1000, 0)
	cfg := config.SelectionConfig{Strategy: config.StrategyLabel, SwitchConfirmChecks: 1, DeadPairCooldownMinutes: 4}
	aH3, aH2, bH3 := mq("a3", 1, "h3", 0), mq("a2", 1, "h2", 1), mq("b3", 2, "h3", 0)
	bH3.Label = "🇳🇱 Amsterdam B"
	// By label then tag "a2" < "a3" < "b3": the key a group sits first, h3
	// before h2.
	ranked := Rank([]Candidate{bH3, aH2, aH3}, cfg.Strategy, self)
	if got := tags(ranked); !reflect.DeepEqual(got, []string{"a3", "a2", "b3"}) {
		t.Fatalf("rank %v", got)
	}
	if got := tags(Pick(ranked, 2, self)); !reflect.DeepEqual(got, []string{"a3", "b3"}) {
		t.Fatalf("pick %v", got)
	}

	var st State
	st.QuarantinePair(aH3, now, cfg.DeadPairCooldownMinutes)
	if st.Quarantined(aH2, now) {
		t.Fatal("h2 of the key is quarantined with h3")
	}
	c, ok := Replacement(ranked, map[string]bool{"a3": true, "b3": true}, map[[32]byte]bool{bH3.Key: true}, &st, now)
	if !ok || c.Tag != "a2" {
		t.Fatalf("replacement %v %v", c.Tag, ok)
	}
	later := now.Add(5 * time.Minute)
	for _, strategy := range []string{config.StrategyLabel, config.StrategyScore} {
		cfg.Strategy = strategy
		if got := brief(Decide(healthy(aH2, bH3), ranked, &st, later, cfg)); !reflect.DeepEqual(got, []string{"keep", "keep"}) {
			t.Fatalf("%s: h2 switched back: %v", strategy, got)
		}
	}
	// Not even with a better h3 metric.
	s1, s9 := 1.0, 9.0
	h3, h2 := aH3, aH2
	h3.Score, h2.Score = &s1, &s9
	if got := brief(Decide(healthy(h2, bH3), []Candidate{h3, h2, bH3}, &st, later, cfg)); !reflect.DeepEqual(got, []string{"keep", "keep"}) {
		t.Fatalf("h2 switched back by score: %v", got)
	}
	// h2 gone: the key takes h3 again.
	if got := brief(Decide(healthy(aH2, bH3), []Candidate{aH3, bH3}, &st, later, cfg)); !reflect.DeepEqual(got, []string{"a3:gone", "keep"}) {
		t.Fatalf("h2 gone: %v", got)
	}
}

// For a better candidate only the first allowed line of a masque key
// competes: a better h2 behind a worse h3 does not switch; with the h3
// quarantined it does.
func TestMasqueBetterFirstLineOnly(t *testing.T) {
	now := time.Unix(1000, 0)
	cfg := config.SelectionConfig{Strategy: config.StrategyScore, SwitchMarginPercent: 10, SwitchConfirmChecks: 1, DeadPairCooldownMinutes: 4}
	score := func(c Candidate, s float64) Candidate { c.Score = &s; return c }
	a := score(mq("a3", 1, "h3", 0), 4)
	b3, b2 := score(mq("b3", 2, "h3", 0), 5), score(mq("b2", 2, "h2", 1), 1)
	ranked := Rank([]Candidate{b2, a, b3}, cfg.Strategy, self)
	if got := tags(ranked); !reflect.DeepEqual(got, []string{"b3", "b2", "a3"}) {
		t.Fatalf("rank %v", got)
	}
	if got := brief(Decide(healthy(a), ranked, &State{}, now, cfg)); !reflect.DeepEqual(got, []string{"keep"}) {
		t.Fatalf("h2 behind h3 switched: %v", got)
	}
	var st State
	st.QuarantinePair(b3, now, 4)
	if got := brief(Decide(healthy(a), ranked, &st, now, cfg)); !reflect.DeepEqual(got, []string{"b2:better"}) {
		t.Fatalf("with h3 quarantined: %v", got)
	}
}

// Rank keeps the wireguard order: without a transport nothing is grouped.
func TestRankWireGuardNotGrouped(t *testing.T) {
	a, b, a2 := cand("a", 1, 1), cand("b", 2, 2), cand("a2", 1, 3)
	if got := tags(Rank([]Candidate{a2, b, a}, config.StrategyScore, self)); !reflect.DeepEqual(got, []string{"a", "b", "a2"}) {
		t.Fatalf("rank %v", got)
	}
}
