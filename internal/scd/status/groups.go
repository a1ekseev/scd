package status

import (
	"cmp"
	"math"
	"slices"
	"strings"

	"github.com/a1ekseev/scd/internal/scd/config"
	"github.com/a1ekseev/scd/internal/scd/state"
)

// balancerConfig returns target.balancers[] entry tag of sub, or one with
// only the tag when it is not configured.
func balancerConfig(sub config.SubscriptionConfig, tag string) config.BalancerConfig {
	for _, b := range sub.Target.Balancers {
		if b.Tag == tag {
			return b
		}
	}
	return config.BalancerConfig{Tag: tag}
}

// tunnelBalancer is the balancer whose state the tunnels of source id show:
// the source's balancer, else target.balancers[0].
func tunnelBalancer(sub config.SubscriptionConfig, id string) config.BalancerConfig {
	for _, src := range sub.Sources {
		if src.ID == id && src.Balancer != "" {
			return balancerConfig(sub, src.Balancer)
		}
	}
	return firstBalancer(sub)
}

// monitorBalancer is the balancer of the target-level balancerMonitor: the
// first of target.balancers with a monitor, else balancers[0].
func monitorBalancer(sub config.SubscriptionConfig) config.BalancerConfig {
	for _, b := range sub.Target.Balancers {
		if b.Monitor != nil {
			return b
		}
	}
	return firstBalancer(sub)
}

// dependencyViews are the dependsOn entries of src with their health in t
// (nil t: before any state). A dependency without state on the target is
// not_applied (a balancer dependency has no state of its own); while the Xray API is unavailable every other one is
// unknown (the state keeps the last real health); never checked is unknown.
func dependencyViews(src config.SourceConfig, t *state.Target) []DependencyView {
	var out []DependencyView
	for _, d := range src.DependsOn {
		v := DependencyView{SourceID: d.Source, Balancer: d.Balancer, Health: d.Health, State: state.DepUnknown}
		switch {
		case d.Source != "" && (t == nil || t.Sources[d.Source] == nil):
			v.State = DepNotApplied
		case t == nil || t.XrayDown != nil:
		default:
			if w := t.Waits[src.ID]; w != nil {
				if h := w.Deps[d.Name()]; h != nil {
					if h.State != "" {
						v.State = h.State
					}
					v.Since, v.RecoveredAt, v.GraceUntil = formatOptTime(h.Since), formatOptTime(h.RecoveredAt), formatOptTime(h.GraceUntil)
				}
			}
		}
		out = append(out, v)
	}
	return out
}

// DepNotApplied is the dependsOn state of a dependency that has never been
// applied to the target.
const DepNotApplied = "not_applied"

// groupsShown reports whether target sub gets balancer groups in the API:
// it has balancers or a source names one.
func groupsShown(sub config.SubscriptionConfig) bool {
	return len(sub.Target.Balancers) > 0 || slices.ContainsFunc(sub.Sources, func(s config.SourceConfig) bool { return s.Balancer != "" })
}

// balancerGroups are the balancer groups of target sub with the state t
// (nil before any state): without any source balancer one group of
// balancers[0] with every source; else one group per balancer of the
// sources and of target.balancers, the sources without a balancer in group
// "". Monitor and selection are set when configured (enabled). Groups are
// ordered by the dependsOn depth of their deepest source (a group after the
// groups of its dependencies), then tag; groups without sources last.
func balancerGroups(sub config.SubscriptionConfig, t *state.Target) []BalancerGroupView {
	if !groupsShown(sub) {
		return nil
	}
	ids := map[string][]string{}
	var tags []string
	add := func(tag, id string) {
		if _, ok := ids[tag]; !ok {
			tags = append(tags, tag)
			ids[tag] = []string{}
		}
		if id != "" {
			ids[tag] = append(ids[tag], id)
		}
	}
	sources := slices.SortedStableFunc(slices.Values(sub.Sources), func(a, b config.SourceConfig) int { return strings.Compare(a.ID, b.ID) })
	if !slices.ContainsFunc(sources, func(s config.SourceConfig) bool { return s.Balancer != "" }) {
		tag := firstBalancer(sub).Tag
		add(tag, "")
		for _, s := range sources {
			add(tag, s.ID)
		}
	} else {
		for _, s := range sources {
			add(s.Balancer, s.ID)
		}
		for _, b := range sub.Target.Balancers {
			add(b.Tag, "")
		}
	}

	depth := dependencyDepth(sources)
	// A group comes after the groups of its dependencies: its rank is the
	// deepest of its sources; ties by tag; groups without sources last.
	groupRank := func(tag string) int {
		if len(ids[tag]) == 0 {
			return math.MaxInt
		}
		r := 0
		for _, id := range ids[tag] {
			r = max(r, depth[id])
		}
		return r
	}
	slices.SortStableFunc(tags, func(a, b string) int {
		return cmp.Or(cmp.Compare(groupRank(a), groupRank(b)), strings.Compare(a, b))
	})

	prefix := sub.Target.ObservatorySubjectSelectorPrefix
	out := make([]BalancerGroupView, 0, len(tags))
	for _, tag := range tags {
		g := BalancerGroupView{BalancerTag: tag, SourceIDs: ids[tag]}
		b := balancerConfig(sub, tag)
		bal := &state.Balancer{Monitor: state.BalancerMonitor{State: state.BalancerIdle}}
		var tunnels []*state.Tunnel
		if t != nil {
			bal, tunnels = balancerState(t, b), t.Tunnels
		}
		if m := b.Monitor; m != nil && m.Enabled {
			v := BalancerMonitorView{State: state.BalancerIdle}
			if t != nil && t.SyncSeen {
				v = balancerView(&bal.Monitor)
			}
			g.Monitor = &v
		}
		if s := enabledSelection(b); s != nil && tag != "" {
			var st *state.SelectionState
			if t != nil {
				st = &bal.Selection
			}
			g.Selection = new(selectionView(*s, tag, prefix, st, tunnels))
		}
		out = append(out, g)
	}
	return out
}

// dependencyDepth is the length of the longest dependsOn path from every
// source (0 without dependencies); a cycle (the config rejects it) counts
// once.
func dependencyDepth(sources []config.SourceConfig) map[string]int {
	byID := make(map[string]config.SourceConfig, len(sources))
	for _, s := range sources {
		byID[s.ID] = s
	}
	depth := make(map[string]int, len(sources))
	visiting := map[string]bool{}
	var walk func(id string) int
	walk = func(id string) int {
		if d, ok := depth[id]; ok {
			return d
		}
		if visiting[id] {
			return 0
		}
		visiting[id] = true
		d := 0
		for _, dep := range byID[id].DependsOn {
			if _, ok := byID[dep.Source]; ok {
				d = max(d, walk(dep.Source)+1)
			}
		}
		visiting[id] = false
		depth[id] = d
		return d
	}
	for _, s := range sources {
		walk(s.ID)
	}
	return depth
}
