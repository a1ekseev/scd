package selection

import (
	"maps"
	"time"

	"github.com/a1ekseev/scd/internal/scd/config"
)

// Replacement reasons (the reason of candidate_replaced and
// candidate_removed).
const (
	ReasonGone    = "gone"
	ReasonChanged = "changed"
	ReasonFailed  = "failed"
	ReasonBetter  = "better"
	// ReasonVacant: a candidate took an empty slot.
	ReasonVacant = "vacant"
)

// PairKey identifies a key on one server (and transport): the quarantine
// unit. Transport is empty for wireguard.
type PairKey struct {
	Key       [32]byte
	Endpoint  string
	Transport string
}

// Pair is the pair key of c.
func (c Candidate) Pair() PairKey {
	return PairKey{Key: c.Key, Endpoint: c.Endpoint, Transport: c.Transport}
}

// State is the in-memory selection state of one source: the quarantined
// pairs with their expiry and the hysteresis series ("<active
// tag>><candidate tag>" → sync ticks in a row the candidate was better).
type State struct {
	Quarantine map[PairKey]time.Time
	Better     map[string]int
}

// Clone returns a deep copy of s.
func (s State) Clone() State {
	return State{Quarantine: maps.Clone(s.Quarantine), Better: maps.Clone(s.Better)}
}

// Quarantined reports whether c's pair is in quarantine at now.
func (s *State) Quarantined(c Candidate, now time.Time) bool {
	until, ok := s.Quarantine[c.Pair()]
	return ok && now.Before(until)
}

// QuarantinePair puts c's pair in quarantine for minutes from now.
func (s *State) QuarantinePair(c Candidate, now time.Time, minutes int) {
	if s.Quarantine == nil {
		s.Quarantine = map[PairKey]time.Time{}
	}
	s.Quarantine[c.Pair()] = now.Add(time.Duration(minutes) * time.Minute)
}

// Replacement returns the first candidate of ranked that may take a slot:
// not active, its pair not in quarantine at now, its key not in keys.
func Replacement(ranked []Candidate, active map[string]bool, keys map[[32]byte]bool, st *State, now time.Time) (Candidate, bool) {
	for _, c := range ranked {
		if !active[c.Tag] && !keys[c.Key] && !st.Quarantined(c, now) {
			return c, true
		}
	}
	return Candidate{}, false
}

// ActionKind is what Decide does with one slot.
type ActionKind int

// Action kinds.
const (
	Keep ActionKind = iota
	Replace
	Remove
)

// Slot is an active tunnel: its candidate as of the sync that activated it
// and whether it is healthy (only a healthy one is switched for a better
// candidate).
type Slot struct {
	Candidate
	Healthy bool
}

// Action is the decision for slot Slot: Keep, Replace by To, or Remove
// (no replacement).
type Action struct {
	Kind   ActionKind
	Slot   int
	To     Candidate
	Reason string
}

// Decide returns one action per active slot, in slot order, for the ranked
// candidates of the current file (st is updated: expired quarantine
// dropped, hysteresis series counted):
//
//   - an active candidate gone from ranked is replaced (reason changed when
//     a candidate with its pair is left, else gone) by the first candidate
//     Replacement allows, else removed;
//   - a healthy kept candidate is replaced by the first allowed candidate
//     whose metric is lower by at least switchMarginPercent % on
//     switchConfirmChecks sync ticks in a row (reason better); the metric is
//     score (load for strategy load), else the other one when either lacks
//     it; without a metric and with strategy label nothing switches.
//     Another transport of the active key never counts as better: priority
//     only picks the candidate of a key that takes a slot.
func Decide(active []Slot, ranked []Candidate, st *State, now time.Time, cfg config.SelectionConfig) []Action {
	for k, until := range st.Quarantine {
		if !now.Before(until) {
			delete(st.Quarantine, k)
		}
	}
	present := map[string]bool{}
	pairs := map[PairKey]bool{}
	for _, c := range ranked {
		present[c.Tag] = true
		pairs[c.Pair()] = true
	}
	inUse := map[string]bool{}
	keys := map[[32]byte]bool{}
	for _, a := range active {
		inUse[a.Tag] = true
		if present[a.Tag] {
			keys[a.Key] = true
		}
	}
	actions := make([]Action, len(active))
	take := func(i int, c Candidate, reason string) {
		actions[i] = Action{Kind: Replace, Slot: i, To: c, Reason: reason}
		inUse[c.Tag], keys[c.Key] = true, true
	}
	for i, a := range active {
		actions[i] = Action{Kind: Keep, Slot: i}
		if present[a.Tag] {
			continue
		}
		reason := ReasonGone
		if pairs[a.Pair()] {
			reason = ReasonChanged
		}
		if c, ok := Replacement(ranked, inUse, keys, st, now); ok {
			take(i, c, reason)
		} else {
			actions[i] = Action{Kind: Remove, Slot: i, Reason: reason}
		}
	}

	better := map[string]int{}
	byTag := map[string]Candidate{}
	for _, c := range ranked {
		byTag[c.Tag] = c
	}
	for i, a := range active {
		if actions[i].Kind != Keep || !a.Healthy || cfg.Strategy == config.StrategyLabel {
			continue
		}
		cur := byTag[a.Tag]
		delete(keys, cur.Key)
		seen := map[[32]byte]bool{}
		for _, c := range ranked {
			if inUse[c.Tag] || keys[c.Key] || c.Key == cur.Key && c.Transport != cur.Transport || st.Quarantined(c, now) {
				continue
			}
			// Only the first (lowest priority) allowed line of a masque key
			// competes.
			if c.Transport != "" && seen[c.Key] {
				continue
			}
			seen[c.Key] = true
			if !beats(c, cur, cfg) {
				continue
			}
			k := cur.Tag + ">" + c.Tag
			better[k] = st.Better[k] + 1
			if better[k] >= cfg.SwitchConfirmChecks {
				delete(better, k)
				take(i, c, ReasonBetter)
			}
			break
		}
		if actions[i].Kind == Keep {
			keys[cur.Key] = true
		}
	}
	st.Better = better
	return actions
}

// beats reports whether c's metric is lower than a's by at least
// cfg.SwitchMarginPercent %.
func beats(c, a Candidate, cfg config.SelectionConfig) bool {
	score := func(x Candidate) *float64 { return x.Score }
	load := func(x Candidate) *float64 {
		if x.Load == nil {
			return nil
		}
		return new(float64(*x.Load))
	}
	metrics := []func(Candidate) *float64{score, load}
	if cfg.Strategy == config.StrategyLoad {
		metrics = []func(Candidate) *float64{load, score}
	}
	for _, m := range metrics {
		mc, ma := m(c), m(a)
		if mc == nil || ma == nil {
			continue
		}
		return *mc < *ma && *mc*100 <= *ma*float64(100-cfg.SwitchMarginPercent)
	}
	return false
}
