package status

import (
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/a1ekseev/scd/internal/scd/config"
	"github.com/a1ekseev/scd/internal/scd/state"
)

// sourceJSON returns sources[] entry id of target i of subscription s in
// /api/instance-status of l.
func sourceJSON(t *testing.T, l loaded, s, i int, id string) map[string]any {
	t.Helper()
	body := json.RawMessage(get(t, NewHandler(l.deps()), "/api/instance-status").Body.Bytes())
	target := index(t, field(t, index(t, field(t, body, "subscriptions"), s), "targets"), i)
	for _, v := range decode(t, field(t, target, "sources")).([]any) {
		if m := v.(map[string]any); m["sourceId"] == id {
			return m
		}
	}
	t.Fatalf("no source %s", id)
	return nil
}

func depState(t *testing.T, src map[string]any) string {
	t.Helper()
	return src["dependsOn"].([]any)[0].(map[string]any)["state"].(string)
}

// dependsOn[].state: healthy and unhealthy from the wait, not_applied
// without the dependency's state, unknown while the Xray API is
// unavailable (the state keeps the last real health) and before a check.
func TestDependencyStates(t *testing.T) {
	l := loadFixture(t, "chain-upstream-down")
	if got := depState(t, sourceJSON(t, l, 0, 0, "warp")); got != state.DepUnhealthy {
		t.Errorf("down %s", got)
	}
	l.mem.Update("main::127.0.0.1:10085", func(tg *state.Target) { tg.XrayDown = &state.XrayIssue{} })
	if got := depState(t, sourceJSON(t, l, 0, 0, "warp")); got != state.DepUnknown {
		t.Errorf("xray down %s", got)
	}
	l.mem.Update("main::127.0.0.1:10085", func(tg *state.Target) { tg.XrayDown, tg.Waits = nil, nil })
	if got := depState(t, sourceJSON(t, l, 0, 0, "warp")); got != state.DepUnknown {
		t.Errorf("never checked %s", got)
	}
	l = loadFixture(t, "chain-config-error")
	edge := sourceJSON(t, l, 0, 0, "edge") // lab sorts first
	if got := depState(t, edge); got != DepNotApplied {
		t.Errorf("not applied %s", got)
	}
	if e := edge["error"].(map[string]any); e["code"] != "dependency_not_applied" || e["at"] != "2026-10-31T08:05:00.000Z" {
		t.Errorf("error %v", e)
	}
	l = loadFixture(t, "chain-recovering")
	d := sourceJSON(t, l, 0, 0, "warp")["dependsOn"].([]any)[0].(map[string]any)
	if d["state"] != state.DepHealthy || d["recoveredAt"] != "2026-10-31T08:29:00.000Z" || d["graceUntil"] != "2026-10-31T08:32:00.000Z" {
		t.Errorf("recovering %v", d)
	}
	if r := sourceJSON(t, loadFixture(t, "chain-recovered"), 0, 0, "warp")["resetAt"]; r != "2026-10-31T08:27:00.000Z" {
		t.Errorf("resetAt %v", r)
	}
}

// Groups: one per balancer of the sources, the sources without one in
// group "", a balancer without sources alone; without any source balancer
// one group of balancers[0]; order by dependsOn, then tag.
func TestBalancerGroups(t *testing.T) {
	sub := config.SubscriptionConfig{Sources: []config.SourceConfig{
		{ID: "z", Balancer: "x-up"},
		{ID: "a", Balancer: "x-down", DependsOn: []config.DependencyConfig{{Source: "z"}}},
		{ID: "b"},
	}, Target: config.SubscriptionTargetConfig{Balancers: []config.BalancerConfig{{Tag: "x-alone"}, {Tag: "x-up"}}}}
	var got []string
	for _, g := range balancerGroups(sub, nil) {
		got = append(got, g.BalancerTag+":"+strings.Join(g.SourceIDs, ","))
	}
	if want := []string{":b", "x-up:z", "x-down:a", "x-alone:"}; !slices.Equal(got, want) {
		t.Errorf("groups %v, want %v", got, want)
	}

	// Equal depth: by tag, not by source id.
	tie := config.SubscriptionConfig{Sources: []config.SourceConfig{{ID: "aaa", Balancer: "z-bal"}, {ID: "bbb", Balancer: "a-bal"}}}
	got = nil
	for _, g := range balancerGroups(tie, nil) {
		got = append(got, g.BalancerTag)
	}
	if want := []string{"a-bal", "z-bal"}; !slices.Equal(got, want) {
		t.Errorf("tie %v, want %v", got, want)
	}

	sub.Sources = []config.SourceConfig{{ID: "b"}, {ID: "a"}}
	g := balancerGroups(sub, nil)
	if len(g) != 1 || g[0].BalancerTag != "x-alone" || !slices.Equal(g[0].SourceIDs, []string{"a", "b"}) || g[0].Monitor != nil || g[0].Selection != nil {
		t.Errorf("one group %+v", g)
	}
	sub.Target.Balancers = nil
	if g := balancerGroups(sub, nil); g != nil {
		t.Errorf("groups without balancers %+v", g)
	}
}

// A tunnel shows the selection of its own source's balancer; transport and
// failuresInRow come from the tunnel.
func TestTunnelGroupFields(t *testing.T) {
	l := loadFixture(t, "chain-recovering")
	body := json.RawMessage(get(t, NewHandler(l.deps()), "/api/status").Body.Bytes())
	selected := map[string]bool{}
	for _, v := range decode(t, field(t, body, "tunnels")).([]any) {
		m := v.(map[string]any)
		if m["balancerSelected"] == true {
			selected[m["displayName"].(string)] = true
		}
		if m["displayName"] == "☁️ WARP #2" && m["transport"] != "h2" {
			t.Errorf("transport %v", m["transport"])
		}
		if m["displayName"] == "🇳🇱 Amsterdam · NL-FREE#214" && m["failuresInRow"] != 5.0 {
			t.Errorf("failuresInRow %v", m["failuresInRow"])
		}
	}
	if want := map[string]bool{"🇳🇱 Amsterdam": true, "☁️ WARP #1": true, "🇩🇪 Frankfurt · DE-FREE#33": true}; !maps.Equal(selected, want) {
		t.Errorf("selected %v, want %v", selected, want)
	}
}
