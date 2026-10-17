package status

import (
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
	"github.com/a1ekseev/scd/internal/scd/state"
	"github.com/a1ekseev/scd/internal/scd/topology"
)

// refStatusDir holds the task 0.4 references (fixtures and TS outputs).
const refStatusDir = "../../../testdata/reference/scd/status/"

// fixture is the task 0.4 fixture contract (testdata/reference/README.md).
type fixture struct {
	Now    time.Time `json:"now"`
	Config struct {
		InstanceID    string `json:"instanceId"`
		Subscriptions []struct {
			ID      string `json:"id"`
			Enabled bool   `json:"enabled"`
			Address string `json:"address"`
			Prefix  string `json:"observatorySubjectSelectorPrefix"`
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
		Tunnels         []struct {
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
}

// loaded is a fixture turned into the inputs of the status layer.
type loaded struct {
	now time.Time
	cfg config.Loaded
	mem *state.Memory
	agg *Aggregator
}

// fixtureNames lists the 0.4 fixtures.
func fixtureNames(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob(refStatusDir + "fixtures/*.json")
	if err != nil || len(files) == 0 {
		t.Fatalf("no fixtures: %v", err)
	}
	names := make([]string, 0, len(files))
	for _, f := range files {
		names = append(names, strings.TrimSuffix(filepath.Base(f), ".json"))
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

	var cfg config.AppConfig
	for _, s := range f.Config.Subscriptions {
		cfg.Subscriptions = append(cfg.Subscriptions, config.SubscriptionConfig{
			ID: s.ID, Input: "/fixture/sub.txt", Enabled: s.Enabled, Format: "auto", FetchTimeoutMs: 5000, AllowInsecure: "deny",
			Target: config.SubscriptionTargetConfig{
				Address: s.Address, TimeoutMs: 5000, ObservatorySubjectSelectorPrefix: s.Prefix,
				FixedOutbounds: []string{}, FixedInbounds: []string{}, FixedRouting: []string{},
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
		var topo topology.Topology
		for _, tn := range tg.Tunnels {
			topo.Tunnels = append(topo.Tunnels, topology.Tunnel{
				BaseTunnelID: tn.BaseTunnelID, DisplayName: tn.DisplayName, CountryISO2: tn.CountryISO2,
				BaseOutboundTag:    tn.OutboundWithoutPrefixTag,
				OutboundTagInitial: tn.OutboundTagInitial, OutboundTagCurrent: tn.OutboundTagCurrent,
				InboundTag: "in-" + tn.BaseTunnelID, RouteTag: "route-" + tn.BaseTunnelID,
				Listen: tn.Listen, Port: tn.Port,
				OutboundInitial:       topology.PreparedOutbound{Tag: tn.OutboundTagInitial},
				OutboundWithoutPrefix: topology.PreparedOutbound{Tag: tn.OutboundWithoutPrefixTag},
			})
		}
		mem.ReplaceTopology(key, topo, nil)
		mem.Update(key, func(st *state.Target) {
			st.Balancer = state.BalancerMonitor(tg.BalancerMonitor)
			for i, tn := range tg.Tunnels {
				st.Tunnels[i].Monitor = state.TunnelMonitor(tn.Monitor)
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

// deps returns handler dependencies for l. Dial fails: the four captured
// endpoints never reach Xray.
func (l loaded) deps() Deps {
	return Deps{
		Config: l.cfg, Memory: l.mem, Aggregator: l.agg,
		Dial:     nil,
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
