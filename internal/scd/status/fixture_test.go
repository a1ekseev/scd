package status

import (
	"cmp"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/a1ekseev/scd/internal/scd/config"
	"github.com/a1ekseev/scd/internal/scd/normalize"
	"github.com/a1ekseev/scd/internal/scd/state"
	"github.com/a1ekseev/scd/internal/scd/topology"
	"github.com/a1ekseev/scd/internal/scd/xrayapi"
)

// refStatusDir holds the task 0.4 references (fixtures and TS outputs).
const refStatusDir = "../../../testdata/reference/scd/status/"

// fixture is the task 0.4 fixture contract (testdata/reference/README.md).
type fixture struct {
	Now time.Time `json:"now"`
	// WGP extends the contract (wgp-*): status.json per wgpDir, written
	// with mode 0600; a JSON string is written verbatim (broken files).
	WGP    map[string]json.RawMessage `json:"wgp"`
	Config struct {
		InstanceID    string `json:"instanceId"`
		Subscriptions []struct {
			ID      string `json:"id"`
			Enabled bool   `json:"enabled"`
			Address string `json:"address"`
			Prefix  string `json:"observatorySubjectSelectorPrefix"`
			// BalancerSelection extends the 0.4 contract (task 6.6).
			BalancerSelection config.BalancerSelectionConfig `json:"balancerSelection"`
			// Sources extends the 0.4 contract (sources-*); empty = one
			// source "vless".
			Sources []struct {
				ID        string `json:"id"`
				Type      string `json:"type"`
				Enabled   bool   `json:"enabled"`
				MaxActive int    `json:"maxActive"`
				// WGPDir extends the contract (wgp-*): the source input is
				// <tmp>/<wgpDir>/<id>, next to the status.json of wgp[wgpDir].
				WGPDir string `json:"wgpDir"`
			} `json:"sources"`
		} `json:"subscriptions"`
		RemoteInstances []struct {
			ID       string `json:"id"`
			APIURL   string `json:"apiUrl"`
			Schedule string `json:"schedule"`
		} `json:"remoteInstances"`
		RuntimeState config.RuntimeStateConfig `json:"runtimeState"`
	} `json:"config"`
	Targets []struct {
		SubscriptionID  string          `json:"subscriptionId"`
		TargetAddress   string          `json:"targetAddress"`
		BalancerMonitor fixtureBalancer `json:"balancerMonitor"`
		// BalancerSelection extends the 0.4 contract (task 6.6).
		BalancerSelection *fixtureSelection `json:"balancerSelection"`
		// Sources extends the 0.4 contract (sources-*): the source warnings.
		Sources []struct {
			SourceID       string              `json:"sourceId"`
			Warnings       []normalize.Warning `json:"warnings"`
			CandidateCount int                 `json:"candidateCount"`
			// NoReplacement are the tunnels without a replacement candidate.
			NoReplacement []string `json:"noReplacement"`
		} `json:"sources"`
		Tunnels []struct {
			// SourceID extends the 0.4 contract (sources-*); "" = "vless".
			SourceID                 string         `json:"sourceId"`
			BaseTunnelID             string         `json:"baseTunnelId"`
			DisplayName              string         `json:"displayName"`
			CountryISO2              string         `json:"countryIso2"`
			Listen                   string         `json:"listen"`
			Port                     int            `json:"port"`
			OutboundTagInitial       string         `json:"outboundTagInitial"`
			OutboundTagCurrent       string         `json:"outboundTagCurrent"`
			OutboundWithoutPrefixTag string         `json:"outboundWithoutPrefixTag"`
			Monitor                  fixtureMonitor `json:"monitor"`
		} `json:"tunnels"`
	} `json:"targets"`
	Remotes []struct {
		ID            string          `json:"id"`
		APIURL        string          `json:"apiUrl"`
		LastAttemptAt string          `json:"lastAttemptAt"`
		LastSuccessAt string          `json:"lastSuccessAt"`
		LastError     string          `json:"lastError"`
		Snapshot      json.RawMessage `json:"snapshot"`
	} `json:"remotes"`
}

type fixtureMonitor struct {
	State                                       string
	LastCheckedAt, LastSuccessAt, LastFailureAt *time.Time
	LastStatusCode, LastLatencyMs               *int
	LastSuccessStatusCode, LastSuccessLatencyMs *int
	LastError                                   string
	ConsecutiveFailures                         int
	// FailuresInRow and the rest are not in the API; the fixtures never set
	// them.
	FailuresInRow   int             `json:"-"`
	FailingSince    *time.Time      `json:"-"`
	LastRepairError string          `json:"-"`
	LastRejoinError string          `json:"-"`
	Flap            state.FlapState `json:"-"`
}

type fixtureBalancer struct {
	State, RemotePingState                                                string
	RemotePingLastReportedStatus, RemotePingLastReportedMsg               string
	RemotePingLastReportedPingMs                                          *int
	LastCheckedAt, LastSuccessAt, LastFailureAt                           *time.Time
	LastStatusCode, LastLatencyMs                                         *int
	LastSuccessStatusCode, LastSuccessLatencyMs                           *int
	LastError                                                             string
	RemotePingLastRunAt, RemotePingLastSuccessAt, RemotePingLastFailureAt *time.Time
	RemotePingLastStatusCode                                              *int
	RemotePingLastError                                                   string
	ConsecutiveFailures                                                   int
	FailingSince                                                          *time.Time `json:"-"`
}

// fixtureSelection is targets[].balancerSelection (task 6.6): the keys of
// the JSON view (10 §6.6) where they overlap, plus the state-only ones
// (previousSelectedTag, consecutiveFailures, observatoryCheckedAt and the
// observatory list; null observatory = unavailable).
type fixtureSelection struct {
	BalancerTag          string               `json:"balancerTag"`
	State                string               `json:"state"`
	Mode                 state.Mode           `json:"mode"`
	SelectedOutboundTag  string               `json:"selectedOutboundTag"`
	OverrideTarget       string               `json:"overrideTarget"`
	PrincipleTargets     []string             `json:"principleTargets"`
	SelectedSince        *time.Time           `json:"selectedSince"`
	PreviousSelectedTag  string               `json:"previousSelectedTag"`
	LastCheckedAt        *time.Time           `json:"lastCheckedAt"`
	LastSuccessAt        *time.Time           `json:"lastSuccessAt"`
	LastFailureAt        *time.Time           `json:"lastFailureAt"`
	LastError            string               `json:"lastError"`
	ConsecutiveFailures  int                  `json:"consecutiveFailures"`
	Observatory          []fixtureObservation `json:"observatory"`
	ObservatoryError     string               `json:"observatoryError"`
	ObservatoryCheckedAt *time.Time           `json:"observatoryCheckedAt"`
}

type fixtureObservation struct {
	Tag        string     `json:"tag"`
	Alive      bool       `json:"alive"`
	DelayMs    *int64     `json:"delayMs"`
	LastError  string     `json:"lastError"`
	LastSeenAt *time.Time `json:"lastSeenAt"`
	LastTryAt  *time.Time `json:"lastTryAt"`
	HealthPing *struct {
		All, Fail                            int64
		AverageMs, DeviationMs, MinMs, MaxMs float64
	} `json:"healthPing"`
}

// selectionState converts the fixture form into the state.
func (f *fixtureSelection) selectionState() state.SelectionState {
	s := state.SelectionState{
		State: f.State, BalancerTag: f.BalancerTag, Mode: f.Mode, SelectedTag: f.SelectedOutboundTag,
		OverrideTarget: f.OverrideTarget, PrincipleTargets: f.PrincipleTargets,
		SelectedSince: f.SelectedSince, PreviousSelectedTag: f.PreviousSelectedTag,
		LastCheckedAt: f.LastCheckedAt, LastSuccessAt: f.LastSuccessAt, LastFailureAt: f.LastFailureAt,
		LastError: f.LastError, ConsecutiveFailures: f.ConsecutiveFailures,
		ObservatoryError: f.ObservatoryError, ObservatoryCheckedAt: f.ObservatoryCheckedAt,
	}
	if f.Observatory != nil {
		ms := func(v float64) time.Duration { return time.Duration(v * float64(time.Millisecond)) }
		s.Observatory = make(map[string]xrayapi.OutboundObservation, len(f.Observatory))
		for _, o := range f.Observatory {
			obs := xrayapi.OutboundObservation{Tag: o.Tag, Alive: o.Alive, DelayMs: o.DelayMs, LastError: o.LastError, LastSeenAt: o.LastSeenAt, LastTryAt: o.LastTryAt}
			if hp := o.HealthPing; hp != nil {
				obs.HealthPing = &xrayapi.HealthPing{All: hp.All, Fail: hp.Fail, Average: ms(hp.AverageMs), Deviation: ms(hp.DeviationMs), Min: ms(hp.MinMs), Max: ms(hp.MaxMs)}
			}
			s.Observatory[o.Tag] = obs
		}
	}
	return s
}

// loaded is a fixture turned into the inputs of the status layer.
type loaded struct {
	now time.Time
	cfg config.Loaded
	mem *state.Memory
	agg *Aggregator
}

// selectionPrefix names the balancer selection fixtures (task 6.6). They
// have no TS reference: the feature does not exist in TS (D22).
const selectionPrefix = "selection-"

// sourcesPrefix names the fixtures of several sources per subscription; they
// have no TS reference either.
const sourcesPrefix = "sources-"

// wgpPrefix names the fixtures with wgp status files; no TS reference.
const wgpPrefix = "wgp-"

// fixtureNames lists the 0.4 fixtures (with a TS reference).
func fixtureNames(t *testing.T) []string {
	t.Helper()
	return globFixtures(t, func(name string) bool {
		return !strings.HasPrefix(name, selectionPrefix) && !strings.HasPrefix(name, sourcesPrefix) && !strings.HasPrefix(name, wgpPrefix)
	})
}

// selectionFixtureNames lists the balancer selection fixtures.
func selectionFixtureNames(t *testing.T) []string {
	t.Helper()
	return globFixtures(t, func(name string) bool { return strings.HasPrefix(name, selectionPrefix) })
}

func globFixtures(t *testing.T, keep func(name string) bool) []string {
	t.Helper()
	files, err := filepath.Glob(refStatusDir + "fixtures/*.json")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range files {
		if name := strings.TrimSuffix(filepath.Base(f), ".json"); keep(name) {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		t.Fatal("no fixtures")
	}
	return names
}

// loadFixture builds config, memory state and aggregation state of a 0.4
// fixture. Subscriptions take the config defaults (input ./sub.txt, never
// read).
func loadFixture(t *testing.T, name string) loaded {
	t.Helper()
	data, err := os.ReadFile(refStatusDir + "fixtures/" + name + ".json")
	if err != nil {
		t.Fatal(err)
	}
	var f fixture
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatalf("%s: %v", name, err)
	}

	wgpRoot := ""
	if len(f.WGP) > 0 {
		wgpRoot = t.TempDir()
	}
	for dir, data := range f.WGP {
		var verbatim string
		if json.Unmarshal(data, &verbatim) == nil {
			data = []byte(verbatim)
		}
		if err := os.MkdirAll(filepath.Join(wgpRoot, dir), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(wgpRoot, dir, "status.json"), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	var cfg config.AppConfig
	for _, s := range f.Config.Subscriptions {
		sources := []config.SourceConfig{testSource("/fixture/sub.txt")}
		if len(s.Sources) > 0 {
			sources = nil
			for _, fs := range s.Sources {
				src := testSource("/fixture/" + fs.ID + ".txt")
				if fs.WGPDir != "" {
					src.Input = filepath.Join(wgpRoot, fs.WGPDir, fs.ID)
				}
				src.ID, src.Type, src.Enabled, src.MaxActive = fs.ID, fs.Type, fs.Enabled, fs.MaxActive
				sources = append(sources, src)
			}
		}
		cfg.Subscriptions = append(cfg.Subscriptions, config.SubscriptionConfig{
			ID: s.ID, Enabled: s.Enabled, Sources: sources,
			Target: config.SubscriptionTargetConfig{
				Address: s.Address, TimeoutMs: 5000, ObservatorySubjectSelectorPrefix: s.Prefix,
				FixedOutbounds: []string{}, FixedInbounds: []string{}, FixedRouting: []string{},
				BalancerSelection: s.BalancerSelection,
			},
		})
	}
	cfg.Resources = config.ResourceConfig{
		Outbounds: config.ResourceToggle{Enabled: true}, Inbounds: config.ResourceToggle{Enabled: true}, Routing: config.ResourceToggle{Enabled: true},
	}
	cfg.StatusServer = config.StatusServerConfig{Enabled: true, Listen: "127.0.0.1:9090", InstanceID: f.Config.InstanceID, RuntimeState: f.Config.RuntimeState}
	for _, r := range f.Config.RemoteInstances {
		cfg.StatusServer.RemoteInstances = append(cfg.StatusServer.RemoteInstances,
			config.RemoteStatusInstanceConfig{ID: r.ID, APIURL: r.APIURL, Schedule: r.Schedule, TimeoutMs: 5000})
	}

	mem := state.New()
	for _, tg := range f.Targets {
		key := state.Key(tg.SubscriptionID, tg.TargetAddress)
		topos := map[string]*topology.Topology{}
		monitors := map[string]state.TunnelMonitor{}
		for _, tn := range tg.Tunnels {
			id := cmp.Or(tn.SourceID, "vless")
			if topos[id] == nil {
				topos[id] = &topology.Topology{}
			}
			monitors[tn.OutboundTagInitial] = state.TunnelMonitor(tn.Monitor)
			topos[id].Tunnels = append(topos[id].Tunnels, topology.Tunnel{
				BaseTunnelID: tn.BaseTunnelID, DisplayName: tn.DisplayName, CountryISO2: tn.CountryISO2,
				BaseOutboundTag:    tn.OutboundWithoutPrefixTag,
				OutboundTagInitial: tn.OutboundTagInitial, OutboundTagCurrent: tn.OutboundTagCurrent,
				InboundTag: "in-" + tn.BaseTunnelID, RouteTag: "route-" + tn.BaseTunnelID,
				Listen: tn.Listen, Port: tn.Port,
				OutboundInitial:       topology.PreparedOutbound{Tag: tn.OutboundTagInitial},
				OutboundWithoutPrefix: topology.PreparedOutbound{Tag: tn.OutboundWithoutPrefixTag},
			})
		}
		if len(topos) == 0 {
			topos["vless"] = &topology.Topology{}
		}
		for id, topo := range topos {
			mem.ReplaceSourceTopology(key, id, *topo, nil)
		}
		mem.Update(key, func(st *state.Target) {
			st.Balancer = state.BalancerMonitor(tg.BalancerMonitor)
			if tg.BalancerSelection != nil {
				st.BalancerSelection = tg.BalancerSelection.selectionState()
			}
			for _, tn := range st.Tunnels {
				tn.Monitor = monitors[tn.Mapping.OutboundTagInitial]
			}
			for _, s := range tg.Sources {
				src := st.Sources[s.SourceID]
				src.Warnings, src.CandidateCount = s.Warnings, s.CandidateCount
				for _, tag := range s.NoReplacement {
					if src.NoReplacement == nil {
						src.NoReplacement = map[string]bool{}
					}
					src.NoReplacement[tag] = true
				}
			}
		})
	}

	agg := NewAggregator(cfg.StatusServer.RemoteInstances)
	for _, r := range f.Remotes {
		rs, ok := agg.remotes[r.ID]
		if !ok || rs.apiURL != r.APIURL {
			t.Fatalf("%s: remote %s not configured", name, r.ID)
		}
		rs.lastAttemptAt, rs.lastSuccessAt, rs.lastError = r.LastAttemptAt, r.LastSuccessAt, r.LastError
		if len(r.Snapshot) > 0 {
			parsed, err := parseRemoteSnapshot(r.Snapshot)
			if err != nil {
				t.Fatalf("%s: remote %s snapshot: %v", name, r.ID, err)
			}
			rs.raw, rs.parsed = slices.Clone(r.Snapshot), &parsed
		}
	}
	return loaded{now: f.Now, cfg: config.Loaded{Path: "/fixture/config.yml", Config: cfg}, mem: mem, agg: agg}
}

// errFixtureDial is returned by the fixture dialer.
var errFixtureDial = errors.New("fixture: Xray must not be dialled")

// deps returns handler dependencies for l. Dial fails: no fixture test may
// reach a real Xray address.
func (l loaded) deps() Deps {
	return Deps{
		Config: l.cfg, Memory: l.mem, Aggregator: l.agg,
		Dial: func(string, time.Duration) (xrayapi.Client, error) {
			return nil, errFixtureDial
		},
		Now:      func() time.Time { return l.now },
		Hostname: func() (string, error) { return "", errors.New("hostname must not be used") },
		Log:      discardLogger(),
	}
}

// get serves path through h and returns the recorder.
func get(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil))
	return rec
}

// decode unmarshals JSON into any.
func decode(t *testing.T, data []byte) any {
	t.Helper()
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, data)
	}
	return v
}

// readRef returns a parsed reference file.
func readRef(t *testing.T, rel string) any {
	t.Helper()
	data, err := os.ReadFile(refStatusDir + rel)
	if err != nil {
		t.Fatal(err)
	}
	return decode(t, data)
}

// jsonEqual reports semantic equality of two decoded JSON values and a
// readable form of both on mismatch.
func jsonEqual(t *testing.T, got, want any) bool {
	t.Helper()
	if reflect.DeepEqual(got, want) {
		return true
	}
	g, _ := json.MarshalIndent(got, "", "  ")
	w, _ := json.MarshalIndent(want, "", "  ")
	t.Errorf("JSON differs\n--- got\n%s\n--- want\n%s", g, w)
	return false
}
