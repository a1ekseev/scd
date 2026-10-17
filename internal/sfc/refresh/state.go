package refresh

import (
	"sync"
	"time"

	"github.com/a1ekseev/scd/internal/sfc/cache"
	"github.com/a1ekseev/scd/internal/sfc/config"
)

// OutputState is the served state of one output. A stored value is never
// modified: every refresh stores a new one.
type OutputState struct {
	ID, SubscriptionID, PathRoute, Name, ProfileTitle string
	// UserAgent lists the User-Agent prefixes that get profile headers.
	UserAgent             []string
	ProfileUpdateInterval int

	// LastGoodBase64 is the served payload; "" until the first success.
	LastGoodBase64, LastGoodPlain string
	LastGoodLineCount             int
	LastRefreshAt, LastSuccessAt  time.Time
	LastFailureAt                 time.Time
	// LastError is the error of the last refresh; "" after a success.
	LastError string
}

// State holds the state of every configured output, keyed by id and by its
// URL path. It is safe for concurrent use.
type State struct {
	mu      sync.RWMutex
	outputs map[string]*OutputState
	byPath  map[string]string // URL path -> output id
}

// NewState registers all outputs of cfg, including disabled outputs and
// outputs of disabled subscriptions (they are served as "not available"
// instead of "not found", 04 §4.7).
func NewState(cfg config.Config) *State {
	s := &State{outputs: map[string]*OutputState{}, byPath: map[string]string{}}
	for _, sub := range cfg.Subscriptions {
		for _, out := range sub.Outputs {
			s.outputs[out.ID] = configured(nil, sub, out)
			s.byPath[outputPath(sub.PathRoute, out.ID)] = out.ID
		}
	}
	return s
}

// outputPath is the URL path of an output: <pathRoute>/<escaped id>.
func outputPath(pathRoute, id string) string {
	return pathRoute + "/" + cache.EscapeID(id)
}

// Lookup returns a copy of the state of the output served at path.
func (s *State) Lookup(path string) (*OutputState, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	id, ok := s.byPath[path]
	if !ok {
		return nil, false
	}
	st := *s.outputs[id]
	return &st, true
}

// get returns a copy of the current state of the output id, or a zero state.
func (s *State) get(id string) OutputState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if st := s.outputs[id]; st != nil {
		return *st
	}
	return OutputState{}
}

// put replaces the state of st.ID.
func (s *State) put(st *OutputState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.outputs[st.ID] = st
}

// configured returns a copy of prev (nil: a zero state) with the config
// fields of out taken from the current config.
func configured(prev *OutputState, sub config.Subscription, out config.Output) *OutputState {
	var st OutputState
	if prev != nil {
		st = *prev
	}
	st.ID, st.SubscriptionID, st.PathRoute = out.ID, sub.ID, sub.PathRoute
	st.Name, st.UserAgent = out.Name, out.UserAgent
	st.ProfileTitle, st.ProfileUpdateInterval = out.ProfileTitle, out.ProfileUpdateInterval
	return &st
}
