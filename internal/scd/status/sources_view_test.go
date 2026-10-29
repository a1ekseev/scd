package status

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/a1ekseev/scd/internal/scd/normalize"
	"github.com/a1ekseev/scd/internal/scd/state"
)

// sourcesTwoWant is targets[].sources of the sources-two fixture.
const sourcesTwoWant = `[
	{"sourceId": "main", "type": "vless", "enabled": true},
	{"sourceId": "spare", "type": "vless", "enabled": true, "warnings": [
		{"code": "port_range_exhausted", "message": "2 lines were skipped: the source portRange has no free port for them."}]}]`

// Several sources: targets[].sources and the tunnels' sourceId, appended
// after the existing keys (additive, version 1).
func TestSourcesJSON(t *testing.T) {
	l := loadFixture(t, "sources-two")
	h := NewHandler(l.deps())
	body := json.RawMessage(get(t, h, "/api/instance-status").Body.Bytes())
	target := index(t, field(t, index(t, field(t, body, "subscriptions"), 0), "targets"), 0)
	if got, want := objectKeys(t, target), []string{"subscriptionId", "targetAddress", "tunnels", "balancerMonitor", "sources"}; !slices.Equal(got, want) {
		t.Errorf("target keys %v, want %v", got, want)
	}
	jsonEqual(t, decode(t, field(t, target, "sources")), decode(t, []byte(sourcesTwoWant)))

	status := json.RawMessage(get(t, h, "/api/status").Body.Bytes())
	for _, list := range []json.RawMessage{field(t, status, "tunnels"), field(t, body, "tunnels"), field(t, target, "tunnels")} {
		var tunnels []json.RawMessage
		if err := json.Unmarshal(list, &tunnels); err != nil || len(tunnels) != 3 {
			t.Fatalf("tunnels %s", list)
		}
		for _, tn := range tunnels {
			keys := objectKeys(t, tn)
			name, src := decode(t, field(t, tn, "displayName")), decode(t, field(t, tn, "sourceId"))
			if keys[len(keys)-1] != "sourceId" || (src == "spare") != (name == "🇳🇱 Amsterdam") {
				t.Errorf("tunnel %s: sourceId %v, keys %v", name, src, keys)
			}
		}
	}
}

// One source: no sources[] and no sourceId (the TS form, D3), unless the
// source has a warning.
func TestSourcesOnlyWithSeveralOrWarnings(t *testing.T) {
	l := loadFixture(t, "healthy-single")
	h := NewHandler(l.deps())
	assertNotContains(t, get(t, h, "/api/instance-status").Body.String(), `"sources"`, `"sourceId"`)

	l.mem.Update("sub-1::127.0.0.1:8080", func(tg *state.Target) {
		tg.Sources["vless"].Warnings = []normalize.Warning{{Code: "port_range_exhausted", Message: "1 lines were skipped."}}
	})
	body := json.RawMessage(get(t, h, "/api/instance-status").Body.Bytes())
	target := index(t, field(t, index(t, field(t, body, "subscriptions"), 0), "targets"), 0)
	jsonEqual(t, decode(t, field(t, target, "sources")), decode(t, []byte(`[{"sourceId": "vless", "type": "vless", "enabled": true,
		"warnings": [{"code": "port_range_exhausted", "message": "1 lines were skipped."}]}]`)))
	if src := decode(t, field(t, index(t, field(t, body, "tunnels"), 0), "sourceId")); src != "vless" {
		t.Errorf("sourceId %v", src)
	}

	// A configured target without state still lists its sources.
	l = loadFixture(t, "sources-two")
	l.mem = state.New()
	body = json.RawMessage(get(t, NewHandler(l.deps()), "/api/instance-status").Body.Bytes())
	target = index(t, field(t, index(t, field(t, body, "subscriptions"), 0), "targets"), 0)
	jsonEqual(t, decode(t, field(t, target, "sources")), decode(t, []byte(`[
		{"sourceId": "main", "type": "vless", "enabled": true}, {"sourceId": "spare", "type": "vless", "enabled": true}]`)))
}

// An old TS snapshot (no sources, no sourceId) goes through Collect and
// /api/instances without losses; a new one keeps its sources when parsed.
func TestRemoteSnapshotsWithAndWithoutSources(t *testing.T) {
	old, err := os.ReadFile(refStatusDir + "healthy-single/api-instance-status.json")
	if err != nil {
		t.Fatal(err)
	}
	r, srv := remoteServer(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(old) })
	a := NewAggregator(nil)
	a.Collect(context.Background(), r, srv.Client(), discardLogger(), clock(t0))
	v := a.View()
	if len(v.Remotes) != 1 || v.Remotes[0].LastError != "" || v.Remotes[0].Snapshot == nil {
		t.Fatalf("remote %+v", v.Remotes)
	}
	typed, err := json.Marshal(v.Remotes[0].Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	jsonEqual(t, decode(t, typed), decode(t, old))
	inst := instancesJSON(t, a, t0)["instances"].([]any)[0].(map[string]any)
	jsonEqual(t, inst["snapshot"], decode(t, old))

	l := loadFixture(t, "sources-two")
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
}

// The sources card follows the balancer cards; tunnels show their source;
// the old remote tab has neither.
func TestDashboardSources(t *testing.T) {
	body := bodyOf(t, dashboard(t, "sources-two", "/"))
	local, remote, ok := strings.Cut(body, `id="instance-panel-1"`)
	if !ok {
		t.Fatal("no remote panel")
	}
	i := strings.Index(local, ">Sources<")
	if i < 0 {
		t.Fatal("no sources card")
	}
	card, _, _ := strings.Cut(local[strings.LastIndex(local[:i], "<article"):], "</article>")
	assertContains(t, card, `class="balancer-card state-repairing"`, ">2 source(s)<",
		`<span class="metric"><span class="metric-label">main</span><span class="metric-value">vless</span></span>`,
		`<span class="metric metric-state-repairing" title="spare: port_range_exhausted"><span class="metric-label">Warning</span>`+
			`<span class="metric-value">2 lines were skipped: the source portRange has no free port for them.</span></span>`)
	ext, src, grid := strings.Index(local, "External SOCKS check"), i, strings.Index(local, `class="nodes-grid"`)
	if ext >= src || src >= grid {
		t.Errorf("card order: external %d, sources %d, grid %d", ext, src, grid)
	}
	assertContains(t, nodeCard(t, local, "🇳🇱 Amsterdam"), `<div class="node-meta">NL · 127.0.0.1:21000 · spare</div>`)
	assertContains(t, nodeCard(t, local, "🇦🇹 Vienna"), `<div class="node-meta">AT · 127.0.0.1:20000 · main</div>`)
	assertNotContains(t, remote, ">Sources<", "Warning")

	// Disabled source.
	l := loadFixture(t, "sources-two")
	l.cfg.Config.Subscriptions[0].Sources[1].Enabled = false
	var page bytes.Buffer
	if err := RenderDashboard(&page, AggregatedView{Local: BuildInstanceSnapshot(l.mem, l.cfg.Config, "x", l.now)}); err != nil {
		t.Fatal(err)
	}
	assertContains(t, page.String(), `<span class="metric-label">spare</span><span class="metric-value">vless · disabled</span>`)
}

// A wireguard source: its counts and the no_replacement warning, in the
// JSON and in the sources card.
func TestSourcesWireGuard(t *testing.T) {
	l := loadFixture(t, "sources-wireguard")
	body := json.RawMessage(get(t, NewHandler(l.deps()), "/api/instance-status").Body.Bytes())
	target := index(t, field(t, index(t, field(t, body, "subscriptions"), 0), "targets"), 0)
	jsonEqual(t, decode(t, field(t, target, "sources")), decode(t, []byte(`[
	{"sourceId": "main", "type": "vless", "enabled": true},
	{"sourceId": "wg", "type": "wireguard", "enabled": true, "maxActive": 2, "activeCount": 2, "candidateCount": 3, "warnings": [
		{"code": "no_replacement", "message": "1 tunnels have no replacement candidate outside quarantine."}]}]`)))

	html := bodyOf(t, dashboard(t, "sources-wireguard", "/"))
	assertContains(t, html,
		`<span class="metric-label">wg</span><span class="metric-value">wireguard · 2/2 active of 3</span>`,
		`<span class="metric metric-state-repairing" title="wg: no_replacement"><span class="metric-label">Warning</span>`+
			`<span class="metric-value">1 tunnels have no replacement candidate outside quarantine.</span></span>`)
}
