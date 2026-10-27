package status

import (
	"bytes"
	"encoding/json"
	"flag"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/xtls/xray-core/app/observatory"

	"github.com/a1ekseev/scd/internal/scd/config"
	"github.com/a1ekseev/scd/internal/scd/state"
	"github.com/a1ekseev/scd/internal/scd/xrayapi/xrayfake"
)

var update = flag.Bool("update", false, "regenerate the Go golden dashboards of the balancer selection fixtures")

// goldenDir holds the Go golden dashboards of the selection fixtures (no TS
// reference exists, D22).
const goldenDir = "../../../testdata/golden/scd/status/"

// objectKeys returns the keys of the JSON object raw in document order.
func objectKeys(t *testing.T, raw json.RawMessage) []string {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(raw))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		t.Fatalf("not an object: %s", raw)
	}
	var keys []string
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			t.Fatal(err)
		}
		keys = append(keys, tok.(string))
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			t.Fatal(err)
		}
	}
	return keys
}

// field returns member name of the JSON object raw.
func field(t *testing.T, raw json.RawMessage, name string) json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	v, ok := m[name]
	if !ok {
		t.Fatalf("no %q in %s", name, raw)
	}
	return v
}

// index returns element i of the JSON array raw.
func index(t *testing.T, raw json.RawMessage, i int) json.RawMessage {
	t.Helper()
	var list []json.RawMessage
	if err := json.Unmarshal(raw, &list); err != nil || i >= len(list) {
		t.Fatalf("no element %d in %s (%v)", i, raw, err)
	}
	return list[i]
}

// 10 §6.6, §6.9 p.5: names and order of the new keys (additive, version 1).
func TestFeatureOnJSONKeys(t *testing.T) {
	l := loadFixture(t, "selection-single")
	h := NewHandler(l.deps())
	body := json.RawMessage(get(t, h, "/api/instance-status").Body.Bytes())
	if v := string(field(t, body, "version")); v != "1" {
		t.Errorf("version %s", v)
	}
	target := index(t, field(t, index(t, field(t, body, "subscriptions"), 0), "targets"), 0)
	if got, want := objectKeys(t, target), []string{"subscriptionId", "targetAddress", "tunnels", "balancerMonitor", "balancerSelection"}; !slices.Equal(got, want) {
		t.Errorf("target keys %v, want %v", got, want)
	}
	sel := field(t, target, "balancerSelection")
	wantSel := []string{
		"balancerTag", "state", "mode", "selectedOutboundTag", "selectedTunnelId", "selectedDisplayName", "selectedCountryIso2",
		"selectedManaged", "selectedDelayMs", "principleTargets", "selectedSince", "lastCheckedAt", "lastSuccessAt",
	}
	if got := objectKeys(t, sel); !slices.Equal(got, wantSel) {
		t.Errorf("balancerSelection keys %v, want %v", got, wantSel)
	}
	jsonEqual(t, decode(t, sel), decode(t, []byte(`{
		"balancerTag": "x-balancer", "state": "ok", "mode": "single",
		"selectedOutboundTag": "x-observe-de-1", "selectedTunnelId": "de-1", "selectedDisplayName": "🇩🇪 Germany 1",
		"selectedCountryIso2": "DE", "selectedManaged": true, "selectedDelayMs": 42,
		"principleTargets": ["x-observe-de-1"], "selectedSince": "2026-10-17T08:45:00.000Z",
		"lastCheckedAt": "2026-10-17T09:00:30.000Z", "lastSuccessAt": "2026-10-17T09:00:30.000Z"}`)))

	// Tunnels: the new keys follow balancerMonitor*, in /api/status and in
	// both lists of /api/instance-status.
	status := json.RawMessage(get(t, h, "/api/status").Body.Bytes())
	for _, tn := range []json.RawMessage{
		index(t, field(t, status, "tunnels"), 0),
		index(t, field(t, body, "tunnels"), 0),
		index(t, field(t, target, "tunnels"), 0),
	} {
		keys := objectKeys(t, tn)
		i := slices.Index(keys, "balancerMonitorRemotePingState")
		if i < 0 || !slices.Equal(keys[i+1:], []string{
			"outboundTag", "balancerSelected", "balancerCandidate", "observatoryStatus",
			"observatoryAlive", "observatoryDelayMs", "observatoryLastSeenAt", "observatoryLastTryAt",
		}) {
			t.Errorf("tunnel keys %v", keys)
		}
		if tag, _ := decode(t, field(t, tn, "outboundTag")).(string); !strings.HasPrefix(tag, "x-observe-") {
			t.Errorf("outboundTag %q", tag)
		}
	}
}

// 10 §6.6: values of the tunnel fields per selection fixture.
func TestFeatureOnTunnelFields(t *testing.T) {
	type tunnelWant struct {
		name                string
		selected, candidate bool
		status              string
		delay               any // float64 or nil
	}
	cases := map[string][]tunnelWant{
		"selection-single": {
			{"🇦🇹 Austria 1", false, false, "alive", 130.0},
			{"🇩🇪 Germany 1", true, true, "alive", 42.0},
			{"🇳🇱 Netherlands 1", false, false, "alive", 95.0},
		},
		"selection-pool": {
			{"🇦🇹 Austria 1", false, true, "dead", nil},
			{"🇩🇪 Germany 1", false, true, "alive", 42.0},
			{"🇳🇱 Netherlands 1", false, true, "alive", 95.0},
		},
		"selection-repaired": {
			{"🇦🇹 Austria 1", false, false, "awaiting", nil},
			{"🇩🇪 Germany 1", true, true, "alive", 42.0},
			{"🇳🇱 Netherlands 1", false, false, "n/a", nil},
		},
		"selection-observatory-unavailable": {
			{"🇦🇹 Austria 1", false, false, "n/a", nil},
			{"🇩🇪 Germany 1", true, true, "n/a", nil},
			{"🇳🇱 Netherlands 1", false, false, "n/a", nil},
		},
	}
	for name, want := range cases {
		t.Run(name, func(t *testing.T) {
			l := loadFixture(t, name)
			got := decode(t, get(t, NewHandler(l.deps()), "/api/status").Body.Bytes()).(map[string]any)["tunnels"].([]any)
			if len(got) != len(want) {
				t.Fatalf("%d tunnels", len(got))
			}
			for i, w := range want {
				tn := got[i].(map[string]any)
				if tn["displayName"] != w.name || tn["balancerSelected"] != w.selected || tn["balancerCandidate"] != w.candidate ||
					tn["observatoryStatus"] != w.status || tn["observatoryDelayMs"] != w.delay {
					t.Errorf("tunnel %d = %v, want %+v", i, tn, w)
				}
				if _, ok := tn["observatoryAlive"]; ok != (w.status == "alive" || w.status == "dead") {
					t.Errorf("tunnel %d observatoryAlive presence: %v", i, tn)
				}
			}
		})
	}

	// Repaired: the unprefixed current tag, stale observatory entries of
	// x-observe-nl-1 and x-observe-gone are not shown.
	l := loadFixture(t, "selection-repaired")
	page := get(t, NewHandler(l.deps()), "/api/status").Body.String()
	assertContains(t, page, `"outboundTag": "nl-1"`)
	assertNotContains(t, page, "x-observe-gone", `"observatoryDelayMs": 95`, `"observatoryDelayMs": 20`)

	// Dead observation keeps its error text.
	l = loadFixture(t, "selection-pool")
	at := decode(t, get(t, NewHandler(l.deps()), "/api/status").Body.Bytes()).(map[string]any)["tunnels"].([]any)[0].(map[string]any)
	if at["observatoryAlive"] != false || !strings.Contains(at["observatoryLastError"].(string), "is dead") {
		t.Errorf("dead tunnel %v", at)
	}
}

// 10 §6.6: the grouped view of each fixture.
func TestFeatureOnTargetView(t *testing.T) {
	cases := map[string]string{
		"selection-override": `{"balancerTag":"x-balancer","state":"ok","mode":"override","selectedOutboundTag":"x-observe-nl-1",
			"selectedTunnelId":"nl-1","selectedDisplayName":"🇳🇱 Netherlands 1","selectedCountryIso2":"NL","selectedManaged":true,
			"selectedDelayMs":95,"overrideTarget":"x-observe-nl-1","principleTargets":["x-observe-de-1"],
			"selectedSince":"2026-10-17T08:45:00.000Z","lastCheckedAt":"2026-10-17T09:00:30.000Z","lastSuccessAt":"2026-10-17T09:00:30.000Z"}`,
		"selection-pool": `{"balancerTag":"x-balancer","state":"ok","mode":"pool","strategy":"random",
			"principleTargets":["x-observe-at-1","x-observe-de-1","x-observe-nl-1"],"selectedSince":"2026-10-17T08:45:00.000Z",
			"lastCheckedAt":"2026-10-17T09:00:30.000Z","lastSuccessAt":"2026-10-17T09:00:30.000Z"}`,
		"selection-none": `{"balancerTag":"x-balancer","state":"ok","mode":"none","principleTargets":[],
			"selectedSince":"2026-10-17T08:59:45.000Z","lastCheckedAt":"2026-10-17T09:00:30.000Z","lastSuccessAt":"2026-10-17T09:00:30.000Z"}`,
		"selection-error": `{"balancerTag":"x-balancer","state":"error","mode":"single","selectedOutboundTag":"x-observe-de-1",
			"selectedTunnelId":"de-1","selectedDisplayName":"🇩🇪 Germany 1","selectedCountryIso2":"DE","selectedManaged":true,
			"selectedDelayMs":42,"principleTargets":["x-observe-de-1"],"selectedSince":"2026-10-17T08:45:00.000Z",
			"lastCheckedAt":"2026-10-17T09:00:30.000Z","lastSuccessAt":"2026-10-17T08:59:30.000Z","lastFailureAt":"2026-10-17T09:00:30.000Z",
			"lastError":"Request to Xray API at 127.0.0.1:8080 timed out after 5000ms."}`,
		"selection-observatory-unavailable": `{"balancerTag":"x-balancer","state":"ok","mode":"single","selectedOutboundTag":"x-observe-de-1",
			"selectedTunnelId":"de-1","selectedDisplayName":"🇩🇪 Germany 1","selectedCountryIso2":"DE","selectedManaged":true,
			"principleTargets":["x-observe-de-1"],"selectedSince":"2026-10-17T08:45:00.000Z",
			"lastCheckedAt":"2026-10-17T09:00:30.000Z","lastSuccessAt":"2026-10-17T09:00:30.000Z",
			"observatoryError":"Xray API service ObservatoryService is not enabled (add it to api.services)."}`,
	}
	for name, want := range cases {
		t.Run(name, func(t *testing.T) {
			l := loadFixture(t, name)
			body := json.RawMessage(get(t, NewHandler(l.deps()), "/api/instance-status").Body.Bytes())
			target := index(t, field(t, index(t, field(t, body, "subscriptions"), 0), "targets"), 0)
			jsonEqual(t, decode(t, field(t, target, "balancerSelection")), decode(t, []byte(want)))
		})
	}

	// Enabled but never polled: idle, nothing else.
	l := loadFixture(t, "healthy-single")
	l.cfg.Config.Subscriptions[0].Target.BalancerSelection.Enabled = true
	l.cfg.Config.Subscriptions[0].Target.BalancerSelection.BalancerTag = "x-balancer"
	body := json.RawMessage(get(t, NewHandler(l.deps()), "/api/instance-status").Body.Bytes())
	target := index(t, field(t, index(t, field(t, body, "subscriptions"), 0), "targets"), 0)
	jsonEqual(t, decode(t, field(t, target, "balancerSelection")), map[string]any{"balancerTag": "x-balancer", "state": "idle"})
	assertNotContains(t, string(body), "outboundTag", "observatory")
}

// 10 §6.9 p.5: a remote snapshot with the new fields (version 1) parses,
// keeps them in the typed snapshot and renders the selection card; one
// without them parses with no selection.
func TestParseRemoteAcceptsNewFields(t *testing.T) {
	l := loadFixture(t, "selection-pool")
	raw := get(t, NewHandler(l.deps()), "/api/instance-status").Body.Bytes()
	parsed, err := parseRemoteSnapshot(raw)
	if err != nil {
		t.Fatal(err)
	}
	again, err := json.Marshal(parsed)
	if err != nil {
		t.Fatal(err)
	}
	jsonEqual(t, decode(t, again), decode(t, raw))
	sel := parsed.Subscriptions[0].Targets[0].BalancerSelection
	if sel == nil || sel.Mode != "pool" || len(sel.PrincipleTargets) != 3 || parsed.Tunnels[0].ObservatoryStatus != "dead" {
		t.Fatalf("parsed %+v", parsed)
	}

	var page bytes.Buffer
	if err := RenderDashboard(&page, AggregatedView{Remotes: []RemoteView{{ID: "edge", Snapshot: &parsed}}}); err != nil {
		t.Fatal(err)
	}
	assertContains(t, bodyOf(t, page.String()), "Xray selection · x-balancer", "pool (3) · random")

	old, err := os.ReadFile(refStatusDir + "healthy-single/api-instance-status.json")
	if err != nil {
		t.Fatal(err)
	}
	parsedOld, err := parseRemoteSnapshot(old)
	if err != nil || parsedOld.Subscriptions[0].Targets[0].BalancerSelection != nil || parsedOld.Tunnels[0].OutboundTag != "" {
		t.Fatalf("old snapshot %+v, %v", parsedOld, err)
	}

	// Values of an unexpected type are dropped from the typed snapshot only.
	odd := bytes.Replace(raw, []byte(`"selectedSince": "2026-10-17T08:45:00.000Z"`), []byte(`"selectedSince": 5`), 1)
	odd = bytes.Replace(odd, []byte(`"observatoryDelayMs": 42`), []byte(`"observatoryDelayMs": "42"`), 1)
	p, err := parseRemoteSnapshot(odd)
	if err != nil || p.Subscriptions[0].Targets[0].BalancerSelection.SelectedSince != "" ||
		p.Subscriptions[0].Targets[0].Tunnels[0].ObservatoryDelayMs != nil || p.Subscriptions[0].Targets[0].Tunnels[0].ObservatoryStatus != "alive" {
		t.Fatalf("odd snapshot %+v, %v", p, err)
	}
}

// selectionCardHTML returns the HTML of the "Xray selection" card of body.
func selectionCardHTML(t *testing.T, body string) string {
	t.Helper()
	i := strings.Index(body, "Xray selection")
	if i < 0 {
		t.Fatal("no selection card")
	}
	start := strings.LastIndex(body[:i], "<article")
	end := strings.Index(body[i:], "</article>")
	return body[start : i+end]
}

// nodeCard returns the HTML of the node card titled name.
func nodeCard(t *testing.T, body, name string) string {
	t.Helper()
	i := strings.Index(body, name+"</div>")
	if i < 0 {
		t.Fatalf("no node card %q", name)
	}
	start := strings.LastIndex(body[:i], `<article class="node-card`)
	end := strings.Index(body[i:], "</article>")
	return body[start : i+end]
}

// 10 §6.7, §6.9 p.6.
func TestDashboardSelectionCard(t *testing.T) {
	cases := []struct {
		fixture string
		card    []string
		nodes   map[string][]string
	}{
		{"selection-single", []string{
			`class="balancer-card state-healthy"`, ">Xray selection · x-balancer<",
			">Selected<", ">DE · 🇩🇪 Germany 1<", ">Latency<", ">42 ms<", ">Since<", ">2026-10-17T08:45:00.000Z<",
			">Observatory<", ">3/3 alive<",
		}, map[string][]string{
			"🇩🇪 Germany 1":     {`class="node-card state-healthy is-selected"`, `<span class="node-badge">Selected</span>`, ">alive · 42 ms<", "metric-observatory-alive"},
			"🇳🇱 Netherlands 1": {`class="node-card state-healthy"`, ">alive · 95 ms<"},
		}},
		{
			"selection-override",
			[]string{`class="balancer-card state-healthy"`, ">override → x-observe-nl-1<", ">95 ms<"},
			map[string][]string{"🇳🇱 Netherlands 1": {"is-selected", ">Selected<"}, "🇩🇪 Germany 1": {`class="node-card state-healthy"`}},
		},
		{
			"selection-pool",
			[]string{`class="balancer-card state-healthy"`, ">pool (3) · random<", ">Latency<", ">-<", ">2/3 alive<"},
			map[string][]string{
				"🇦🇹 Austria 1": {`<span class="node-badge">Pool</span>`, ">dead<", "metric-observatory-dead", `title="the outbound x-observe-at-1 is dead: GET request failed: context deadline exceeded"`},
				"🇩🇪 Germany 1": {`<span class="node-badge">Pool</span>`, ">alive · 42 ms<"},
			},
		},
		{"selection-none", []string{`class="balancer-card state-degraded"`, ">none (fallback/default outbound)<", ">0/3 alive<"}, nil},
		{"selection-error", []string{
			`class="balancer-card state-degraded"`, ">DE · 🇩🇪 Germany 1 (stale)<", ">Error<",
			">Request to Xray API at 127.0.0.1:8080 timed out after 5000ms.<", `class="metric metric-state-degraded"`,
		}, nil},
		{"selection-observatory-unavailable", []string{
			`class="balancer-card state-healthy"`, ">DE · 🇩🇪 Germany 1<", ">unavailable<",
			`title="Xray API service ObservatoryService is not enabled (add it to api.services)."`, "metric-observatory-na",
		}, map[string][]string{"🇩🇪 Germany 1": {"is-selected", ">n/a<", "metric-observatory-na"}}},
		{"selection-repaired", []string{`class="balancer-card state-healthy"`, ">1/2 alive<"}, map[string][]string{
			"🇳🇱 Netherlands 1": {`class="node-card state-degraded"`, ">n/a<", ">removed<"},
			"🇦🇹 Austria 1":     {">awaiting<", "metric-observatory-awaiting"},
		}},
	}
	for _, c := range cases {
		t.Run(c.fixture, func(t *testing.T) {
			body := bodyOf(t, dashboard(t, c.fixture, "/"))
			card := selectionCardHTML(t, body)
			assertContains(t, card, c.card...)
			for name, want := range c.nodes {
				assertContains(t, nodeCard(t, body, name), want...)
			}
			// The selection card follows the external SOCKS check and
			// precedes the tunnel grid (10 §6.7).
			ext, sel, grid := strings.Index(body, "External SOCKS check"), strings.Index(body, "Xray selection"), strings.Index(body, `class="nodes-grid"`)
			if ext >= sel || sel >= grid {
				t.Errorf("card order: external %d, selection %d, grid %d", ext, sel, grid)
			}
		})
	}

	// Override to a tag outside the topology and a dead selected tunnel are
	// degraded; never polled is idle.
	for _, c := range []struct {
		fixture, want string
		mutate        func(l loaded)
	}{
		{"selection-override", "state-degraded", func(l loaded) {
			l.mem.Update("sub-1::127.0.0.1:8080", func(tg *state.Target) {
				tg.BalancerSelection.SelectedTag, tg.BalancerSelection.OverrideTarget = "x-observe-static", "x-observe-static"
			})
		}},
		{"selection-pool", "state-degraded", func(l loaded) {
			l.mem.Update("sub-1::127.0.0.1:8080", func(tg *state.Target) {
				tg.BalancerSelection.Mode, tg.BalancerSelection.SelectedTag = "single", "x-observe-at-1"
			})
		}},
		{"selection-single", "state-idle", func(l loaded) {
			l.mem.Update("sub-1::127.0.0.1:8080", func(tg *state.Target) { tg.BalancerSelection = state.SelectionState{} })
		}},
	} {
		l := loadFixture(t, c.fixture)
		c.mutate(l)
		rec := get(t, NewHandler(l.deps()), "/")
		card := selectionCardHTML(t, bodyOf(t, rec.Body.String()))
		assertContains(t, card, `class="balancer-card `+c.want+`"`)
	}
}

// D3: without balancerSelection (feature off or an old remote) no markup
// and no CSS of the selection view is rendered.
func TestDashboardFeatureOffNoMarkup(t *testing.T) {
	for _, name := range fixtureNames(t) {
		t.Run(name, func(t *testing.T) {
			page := dashboard(t, name, "/")
			assertNotContains(t, page, "Xray selection", "is-selected", "node-badge", "Observatory", "observatory")
		})
	}
	// The selection CSS is only inlined when a target has a selection.
	assertContains(t, dashboard(t, "selection-single", "/"), ".is-selected", ".node-badge", ".metric-observatory-alive")
}

// 10 §6.7: Go golden dashboards of the selection fixtures (-update
// regenerates them).
func TestDashboardSelectionGolden(t *testing.T) {
	for _, name := range selectionFixtureNames(t) {
		t.Run(name, func(t *testing.T) {
			got := []byte(dashboard(t, name, "/"))
			path := filepath.Join(goldenDir, name+".html")
			if *update {
				if err := os.MkdirAll(goldenDir, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, got, 0o600); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("%v (run with -update)", err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("%s differs from the golden file (run with -update to regenerate)", path)
			}
		})
	}
}

// 10 §6.6: runtime-state carries the complete selection state.
func TestRuntimeStateBalancerSelection(t *testing.T) {
	l := loadFixture(t, "selection-error")
	fake, _ := xrayfake.Start(t)
	fake.SetObservation(&observatory.OutboundStatus{OutboundTag: "x", Alive: true, Delay: 1})
	d := l.deps()
	d.Dial = fake.Dialer()
	body, code, err := BuildRuntimeState(t.Context(), d, "sub-1", "127.0.0.1:8080")
	if err != nil || code != http.StatusOK {
		t.Fatalf("%d %v", code, err)
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	ss := field(t, json.RawMessage(raw), "serviceState")
	if got := objectKeys(t, ss); !slices.Equal(got, []string{"balancerMonitor", "balancerSelection"}) {
		t.Errorf("serviceState keys %v", got)
	}
	sel := decode(t, field(t, ss, "balancerSelection")).(map[string]any)
	if sel["consecutiveFailures"] != 3.0 || sel["state"] != "error" || sel["previousSelectedTag"] != nil ||
		sel["observatoryCheckedAt"] != "2026-10-17T09:00:30.000Z" || sel["selectedTunnelId"] != "de-1" {
		t.Errorf("balancerSelection %v", sel)
	}
	obs, _ := sel["observatory"].([]any)
	if len(obs) != 4 || obs[0].(map[string]any)["tag"] != "x-observe-at-1" || obs[1].(map[string]any)["delayMs"] != 42.0 {
		t.Errorf("observatory %v", sel["observatory"])
	}
	cfgSel := decode(t, field(t, field(t, field(t, json.RawMessage(raw), "config"), "target"), "balancerSelection"))
	jsonEqual(t, cfgSel, map[string]any{"enabled": true, "balancerTag": "x-balancer", "schedule": "*/15 * * * * *", "observatory": true})

	// Feature off: no key.
	l = loadFixture(t, "healthy-single")
	d = l.deps()
	d.Dial = fake.Dialer()
	body, _, _ = BuildRuntimeState(t.Context(), d, "sub-1", "127.0.0.1:8080")
	raw, _ = json.Marshal(body)
	if strings.Contains(string(field(t, json.RawMessage(raw), "serviceState")), "balancerSelection") {
		t.Errorf("feature off runtime-state has a selection: %s", raw)
	}
}

// Review 6.6: a target created only by the selection tick (no sync yet)
// has no balancerMonitor in runtime-state and the TS idle balancer monitor
// in /api/instance-status.
func TestSelectionBeforeSyncKeepsMonitorAbsent(t *testing.T) {
	l := loadFixture(t, "empty")
	l.cfg.Config.Subscriptions = []config.SubscriptionConfig{{
		ID: "sub-1", Input: "/fixture/sub.txt", Enabled: true, Format: "auto", FetchTimeoutMs: 5000, AllowInsecure: "deny",
		Target: config.SubscriptionTargetConfig{
			Address: "127.0.0.1:8080", TimeoutMs: 5000, ObservatorySubjectSelectorPrefix: "x-observe-",
			FixedOutbounds: []string{}, FixedInbounds: []string{}, FixedRouting: []string{},
			BalancerSelection: config.BalancerSelectionConfig{Enabled: true, BalancerTag: "x-balancer", Observatory: true},
		},
	}}
	key := state.Key("sub-1", "127.0.0.1:8080")
	l.mem.Ensure(key)
	l.mem.Update(key, func(tg *state.Target) {
		tg.BalancerSelection = state.SelectionState{State: state.SelectionOK, BalancerTag: "x-balancer", Mode: state.ModeNone, PrincipleTargets: []string{}}
	})

	fake, _ := xrayfake.Start(t)
	d := l.deps()
	d.Dial = fake.Dialer()
	body, code, err := BuildRuntimeState(t.Context(), d, "sub-1", "127.0.0.1:8080")
	if err != nil || code != http.StatusOK {
		t.Fatalf("%d %v", code, err)
	}
	raw, _ := json.Marshal(body)
	if got := objectKeys(t, field(t, json.RawMessage(raw), "serviceState")); !slices.Equal(got, []string{"balancerSelection"}) {
		t.Errorf("serviceState keys %v, want only balancerSelection", got)
	}

	inst := json.RawMessage(get(t, NewHandler(l.deps()), "/api/instance-status").Body.Bytes())
	target := index(t, field(t, index(t, field(t, inst, "subscriptions"), 0), "targets"), 0)
	jsonEqual(t, decode(t, field(t, target, "balancerMonitor")), map[string]any{"state": "idle"})
}
