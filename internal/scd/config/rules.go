package config

import (
	"fmt"
	"net"
	"regexp"
	"slices"
	"strings"

	"github.com/a1ekseev/scd/internal/common/cron"
	"github.com/a1ekseev/scd/internal/common/listenaddr"
	"github.com/a1ekseev/scd/internal/common/schema"
)

const noSourcesWarning = "the subscription is enabled but none of its sources is; " +
	"sync will remove every non-fixed resource of its target"

const noPrefixWarning = "the selection is enabled without observatorySubjectSelectorPrefix; " +
	"scd tunnels are unlikely to match the balancer selector"

// checkRules applies the cross-field rules (01 §2.3, 10 §6.5) to the parsed
// config. Every rule runs, whatever else failed. subNodes[i] is the node of
// cfg.Subscriptions[i]. It returns the warnings.
func checkRules(root *schema.Node, cfg *AppConfig, subNodes []*schema.Node) []string {
	checkSubscriptions(root, cfg.Subscriptions)
	checkSources(cfg.Subscriptions, subNodes, cfg.Resources)

	// A configured schedule is validated even when unused, so a typo is
	// caught before the feature is turned on.
	switch {
	case cfg.Runtime.Schedule != "":
		checkCron(root, "runtime.schedule", cfg.Runtime.Schedule)
	case cfg.Runtime.Mode == "daemon":
		root.Addf("runtime.schedule", `required when runtime.mode is "daemon"`)
	}

	res := cfg.Resources
	if res.Inbounds.Enabled && !res.Outbounds.Enabled {
		root.Addf("resources.inbounds.enabled", "requires resources.outbounds.enabled")
	}
	if res.Routing.Enabled && (!res.Outbounds.Enabled || !res.Inbounds.Enabled) {
		root.Addf("resources.routing.enabled", "requires resources.outbounds.enabled and resources.inbounds.enabled")
	}

	var warnings []string
	for i, s := range cfg.Subscriptions {
		warnings = append(warnings, checkTarget(subNodes[i], s.Target, res)...)
		warnings = append(warnings, checkDependencies(subNodes[i], s)...)
		if s.Enabled && len(s.Sources) > 0 && len(s.EnabledSources()) == 0 {
			warnings = append(warnings, subNodes[i].Path()+": "+noSourcesWarning)
		}
	}
	warnings = append(warnings, checkCrossDependencies(cfg.Subscriptions, subNodes)...)

	checkStatusServer(root, cfg.StatusServer)
	return warnings
}

// checkSubscriptions requires an enabled subscription and unique target
// addresses (disabled subscriptions included). Nothing is reported when no
// subscription could be parsed (that problem is already reported).
func checkSubscriptions(root *schema.Node, subs []SubscriptionConfig) {
	if len(subs) == 0 {
		return
	}
	anyEnabled := false
	count := map[string]int{}
	var order []string
	for _, s := range subs {
		anyEnabled = anyEnabled || s.Enabled
		if s.Target.Address == "" {
			continue
		}
		if count[s.Target.Address] == 0 {
			order = append(order, s.Target.Address)
		}
		count[s.Target.Address]++
	}
	if !anyEnabled {
		root.Addf("subscriptions", "at least one subscription must be enabled")
	}
	for _, addr := range order {
		if count[addr] > 1 {
			root.Addf("subscriptions", "duplicate target address %q", addr)
		}
	}
}

// checkTarget applies the per-target rules to the subscription node n
// (disabled subscriptions included) and returns the warnings.
func checkTarget(n *schema.Node, t SubscriptionTargetConfig, res ResourceConfig) []string {
	if (res.Inbounds.Enabled || res.Routing.Enabled) && t.InboundSocks == nil {
		n.Addf("target.inboundSocks", "required when resources.inbounds.enabled or resources.routing.enabled is true")
	}
	if s := t.InboundSocks; s != nil && s.Listen != "" && s.Listen != "localhost" && net.ParseIP(s.Listen) == nil {
		// Xray listens on an IP or "localhost" only; any other domain fails
		// on AddInbound (02 §1.5).
		n.Addf("target.inboundSocks.listen", `must be an IP address or "localhost" (Xray cannot listen on a domain)`)
	}

	m := t.Monitor
	checkSchedule(n, "target.monitor", m.Schedule, m.Enabled)
	if m.Enabled {
		if m.Request == nil {
			n.Addf("target.monitor.request", "required when monitor.enabled is true")
		}
		if t.InboundSocks == nil {
			n.Addf("target.inboundSocks", "required when monitor.enabled is true")
		}
		if !res.Outbounds.Enabled || !res.Inbounds.Enabled || !res.Routing.Enabled {
			n.Addf("target.monitor.enabled",
				"requires resources.outbounds.enabled, resources.inbounds.enabled and resources.routing.enabled")
		}
	}

	var warnings []string
	seen := map[string]bool{}
	for i, b := range t.Balancers {
		key := fmt.Sprintf("target.balancers.%d", i)
		if b.Tag != "" {
			if seen[b.Tag] {
				n.Addf(key+".tag", "duplicate balancer tag %q", b.Tag)
			}
			seen[b.Tag] = true
		}
		if bm := b.Monitor; bm != nil {
			checkSchedule(n, key+".monitor", bm.Schedule, bm.Enabled)
			if bm.Enabled {
				if bm.Socks5 == nil {
					n.Addf(key+".monitor.socks5", "required when monitor.enabled is true")
				}
				if bm.Request == nil {
					n.Addf(key+".monitor.request", "required when monitor.enabled is true")
				}
			}
			if bm.RemotePing.Enabled && !bm.Enabled {
				n.Addf(key+".monitor.remotePing.enabled", "requires monitor.enabled")
			}
		}
		if bs := b.Selection; bs != nil {
			checkSchedule(n, key+".selection", bs.Schedule, bs.Enabled)
			if bs.Enabled && t.ObservatorySubjectSelectorPrefix == "" {
				warnings = append(warnings, n.Path()+"."+key+".selection: "+noPrefixWarning)
			}
		}
	}
	return warnings
}

// checkSchedule validates <obj>.schedule (obj is the key path relative to n,
// e.g. "target.monitor"): a configured expression must parse whether or not
// the object is enabled; an enabled object requires one.
func checkSchedule(n *schema.Node, obj, expr string, enabled bool) {
	if expr != "" {
		checkCron(n, obj+".schedule", expr)
	} else if enabled {
		n.Addf(obj+".schedule", "required when %s.enabled is true", obj[strings.LastIndex(obj, ".")+1:])
	}
}

func checkCron(n *schema.Node, key, expr string) {
	if _, err := cron.Parse(expr); err != nil {
		n.Addf(key, "%v", err)
	}
}

// checkRemoteInstance reports a duplicate id (seen collects the ids) and an
// invalid schedule of the remote instance r read from node n.
func checkRemoteInstance(n *schema.Node, r RemoteStatusInstanceConfig, seen map[string]bool) {
	if r.ID != "" {
		if seen[r.ID] {
			n.Addf("id", "duplicate remote instance id %q", r.ID)
		}
		seen[r.ID] = true
	}
	if r.Schedule != "" {
		checkCron(n, "schedule", r.Schedule)
	}
}

func checkStatusServer(root *schema.Node, s StatusServerConfig) {
	if s.Enabled {
		if s.Listen == "" {
			root.Addf("statusServer.listen", "required when statusServer.enabled is true")
		} else if _, _, err := listenaddr.Parse(s.Listen); err != nil {
			root.Addf("statusServer.listen", "%v", err)
		}
	}
}

var sourceIDRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,14}[a-z0-9])?$`)

// ValidSourceID reports whether id is a valid source id: [a-z0-9-], 1..16
// characters, without a leading or trailing "-".
func ValidSourceID(id string) bool { return sourceIDRe.MatchString(id) }

// checkSources applies the source rules to every subscription (disabled
// ones included). Target addresses are unique (checkSubscriptions), so one
// subscription is one Xray: within it ids are valid and unique, no "<id>-"
// starts another id and the port ranges do not overlap; the per-target
// rules are in checkTargetSources. The same id in different subscriptions
// is fine.
func checkSources(subs []SubscriptionConfig, subNodes []*schema.Node, res ResourceConfig) {
	for i, sub := range subs {
		n := subNodes[i]
		for j, src := range sub.Sources {
			key := fmt.Sprintf("sources.%d.", j)
			if src.ID != "" && !ValidSourceID(src.ID) {
				n.Addf(key+"id", `must be 1..16 characters [a-z0-9-] without a leading or trailing "-"`)
				continue
			}
			for _, other := range sub.Sources[:j] {
				switch {
				case src.ID != "" && src.ID == other.ID:
					n.Addf(key+"id", "duplicate source id %q", src.ID)
				case src.ID != "" && other.ID != "" &&
					(strings.HasPrefix(src.ID, other.ID+"-") || strings.HasPrefix(other.ID, src.ID+"-")):
					n.Addf(key+"id", "source ids %q and %q overlap: one starts with the other followed by \"-\"", src.ID, other.ID)
				}
				if pr, op := src.PortRange, other.PortRange; pr != nil && op != nil && pr.Start <= op.End && op.Start <= pr.End {
					n.Addf(key+"portRange", "overlaps the portRange of source %q", other.ID)
				}
			}
		}
		checkTargetSources(n, sub, res)
	}
}

// checkTargetSources applies the per-target source rules to subscription
// node n. A fixed tag may not lie in the namespace of an enabled source; a
// disabled source owns nothing in Xray (its resources are orphans), so its
// namespace is free.
func checkTargetSources(n *schema.Node, sub SubscriptionConfig, res ResourceConfig) {
	t := sub.Target
	obs := t.ObservatorySubjectSelectorPrefix
	needPorts := res.Inbounds.Enabled || res.Routing.Enabled
	for j, src := range sub.Sources {
		key := fmt.Sprintf("sources.%d.", j)
		if pr := src.PortRange; pr != nil {
			if pr.Start > 0 && pr.End > 0 && pr.End < pr.Start {
				n.Addf(key+"portRange.end", "must be greater than or equal to start")
			} else if src.MaxActive > 0 && pr.Start > 0 && pr.End-pr.Start+1 < src.MaxActive {
				n.Addf(key+"portRange", "has %d ports, fewer than maxActive %d", pr.End-pr.Start+1, src.MaxActive)
			}
		} else if needPorts {
			n.Addf(key+"portRange", "required when resources.inbounds.enabled or resources.routing.enabled is true")
		}
		if CandidateType(src.Type) && (!res.Inbounds.Enabled || !res.Routing.Enabled) {
			n.Addf(key+"type", "%q requires resources.inbounds.enabled and resources.routing.enabled", src.Type)
		}
		if !ValidSourceID(src.ID) {
			continue
		}
		p := src.ID + "-"
		if obs != "" && (strings.HasPrefix(obs, p) || strings.HasPrefix(p, obs)) {
			n.Addf(key+"id", "namespace %q overlaps target.observatorySubjectSelectorPrefix %q", p, obs)
		}
		if !src.Enabled {
			continue
		}
		for _, fx := range []struct {
			key      string
			tags     []string
			prefixes []string
		}{
			{"target.fixedOutbounds", t.FixedOutbounds, []string{obs + p, p}},
			{"target.fixedInbounds", t.FixedInbounds, []string{"in-" + p}},
			{"target.fixedRouting", t.FixedRouting, []string{"route-" + p}},
		} {
			for _, tag := range fx.tags {
				for _, pre := range fx.prefixes {
					if strings.HasPrefix(tag, pre) {
						n.Addf(fx.key, "tag %q is in the namespace of source %q", tag, src.ID)
						break
					}
				}
			}
		}
	}
}

// checkDependencies applies the cascade rules (balancer, dialerProxy,
// dependsOn) to the sources of sub, whose node is n, and returns the
// warnings. A dependsOn balancer is not a source: it adds no edge, may not
// be the balancer of any source of sub (use dependsOn source), and need not
// be in target.balancers. Disabled sources are checked too, except that only an enabled
// source may not depend on a disabled one. A dependency with subscription is
// checked here for repeats only (checkCrossDependencies).
func checkDependencies(n *schema.Node, sub SubscriptionConfig) []string {
	byID := make(map[string]SourceConfig, len(sub.Sources))
	for _, src := range sub.Sources {
		if _, dup := byID[src.ID]; !dup {
			byID[src.ID] = src
		}
	}
	byBalancer := map[string]string{}
	for _, src := range sub.Sources {
		if _, dup := byBalancer[src.Balancer]; !dup && src.Balancer != "" {
			byBalancer[src.Balancer] = src.ID
		}
	}
	strategy := map[string]string{}
	for _, b := range sub.Target.Balancers {
		if b.Selection != nil {
			strategy[b.Tag] = b.Selection.Strategy
		}
	}

	var warnings []string
	edges := map[string][]string{}
	for j, src := range sub.Sources {
		key := fmt.Sprintf("sources.%d.", j)
		if src.DialerProxy != "" {
			if !slices.Contains(sub.Target.FixedOutbounds, src.DialerProxy) {
				n.Addf(key+"dialerProxy", "must be in target.fixedOutbounds")
			}
			if src.Enabled && src.Type == SourceMasque && !sub.Target.VisionUDP443Override {
				warnings = append(warnings, n.Path()+"."+key+"dialerProxy: "+masqueVisionWarning)
			}
		}
		seen := map[string]bool{}
		for k, d := range src.DependsOn {
			dkey := fmt.Sprintf("%sdependsOn.%d.", key, k)
			if d.Subscription != "" {
				// The rest is checkCrossDependencies.
				if d.Balancer != "" {
					if seen[d.Name()] {
						n.Addf(dkey+"balancer", "duplicate dependency %q", d.Name())
					}
					seen[d.Name()] = true
				}
				continue
			}
			if d.Balancer != "" {
				switch owner := byBalancer[d.Balancer]; {
				case seen[d.Name()]:
					n.Addf(dkey+"balancer", "duplicate dependency %q", d.Name())
					continue
				case owner == src.ID:
					n.Addf(dkey+"balancer", "a source cannot depend on its own balancer")
				case owner != "":
					n.Addf(dkey+"balancer", "balancer %q is the balancer of source %q: use dependsOn source", d.Balancer, owner)
				}
				seen[d.Name()] = true
				if st := strategy[d.Balancer]; d.Health == HealthBalancer && (st == "random" || st == "roundRobin") {
					warnings = append(warnings, fmt.Sprintf("%s.%shealth: balancer %q has strategy %q, which selects every candidate; "+
						"the dependency is always healthy", n.Path(), dkey, d.Balancer, st))
				}
				continue
			}
			if d.Source == "" {
				continue
			}
			dep, ok := byID[d.Source]
			switch {
			case !ok:
				n.Addf(dkey+"source", "unknown source %q", d.Source)
				continue
			case d.Source == src.ID:
				n.Addf(dkey+"source", "a source cannot depend on itself")
				continue
			case seen[d.Source]:
				n.Addf(dkey+"source", "duplicate dependency %q", d.Source)
				continue
			}
			seen[d.Source] = true
			edges[src.ID] = append(edges[src.ID], d.Source)
			if src.Enabled && !dep.Enabled {
				n.Addf(dkey+"source", "source %q is disabled", d.Source)
			}
			if d.Health == HealthBalancer {
				if dep.Balancer == "" {
					n.Addf(dkey+"health", "source %q has no balancer", d.Source)
				} else if st := strategy[dep.Balancer]; st == "random" || st == "roundRobin" {
					warnings = append(warnings, fmt.Sprintf("%s.%shealth: balancer %q has strategy %q, which selects every candidate; "+
						"the dependency is always healthy", n.Path(), dkey, dep.Balancer, st))
				}
			}
		}
	}

	cyclic := checkCycles(n, sub.Sources, edges)
	for j, src := range sub.Sources {
		if src.Balancer == "" || cyclic[src.ID] {
			continue
		}
		for _, id := range dependencies(src.ID, edges) {
			if byID[id].Balancer == src.Balancer {
				n.Addf(fmt.Sprintf("sources.%d.balancer", j), "balancer %q is also the balancer of dependency %q", src.Balancer, id)
			}
		}
	}
	return warnings
}

const masqueVisionWarning = "a masque source behind dialerProxy with target.visionUdp443Override false: " +
	"an XTLS vision outbound on the way rejects UDP/443, so h3 lines do not connect"

// checkCycles reports every dependsOn cycle of edges once, at the source of
// the cycle that comes first in config order, and returns the ids on a
// cycle.
func checkCycles(n *schema.Node, sources []SourceConfig, edges map[string][]string) map[string]bool {
	order := make([]string, len(sources))
	index := map[string]int{}
	for j, src := range sources {
		order[j] = src.ID
		if _, ok := index[src.ID]; !ok {
			index[src.ID] = j
		}
	}
	cyclic := map[string]bool{}
	for _, cycle := range findCycles(order, edges) {
		for _, c := range cycle {
			cyclic[c] = true
		}
		n.Addf(fmt.Sprintf("sources.%d.dependsOn", index[cycle[0]]), "dependsOn cycle: %s",
			strings.Join(append(cycle, cycle[0]), " → "))
	}
	return cyclic
}

// findCycles returns the cycles of edges found by a depth-first search over
// the nodes in order, each once, rotated to the node first in order.
func findCycles(order []string, edges map[string][]string) [][]string {
	const (
		unseen = iota
		active
		done
	)
	index := map[string]int{}
	for j, id := range order {
		if _, ok := index[id]; !ok {
			index[id] = j
		}
	}
	mark := map[string]int{}
	reported := map[string]bool{}
	var cycles [][]string
	var path []string
	var visit func(id string)
	visit = func(id string) {
		mark[id] = active
		path = append(path, id)
		for _, next := range edges[id] {
			switch mark[next] {
			case unseen:
				visit(next)
			case active:
				cycle := slices.Clone(path[slices.Index(path, next):])
				// Rotate to the node first in order: one report per cycle.
				first := 0
				for i, c := range cycle {
					if index[c] < index[cycle[first]] {
						first = i
					}
				}
				cycle = append(cycle[first:], cycle[:first]...)
				if k := strings.Join(cycle, " "); !reported[k] {
					reported[k] = true
					cycles = append(cycles, cycle)
				}
			}
		}
		path = path[:len(path)-1]
		mark[id] = done
	}
	for _, id := range order {
		if mark[id] == unseen {
			visit(id)
		}
	}
	return cycles
}

// checkCrossDependencies checks the dependsOn entries with subscription
// (disabled sources included), reporting at subNodes[i], and returns the
// warnings. The subscription exists, is not the source's own, has a unique
// id, is enabled when the source and its subscription are, and has the
// balancer in target.balancers with an enabled selection. It also reports
// the dependsOn cycles that span two or more subscriptions; a cycle within
// one subscription is reported by checkCycles.
func checkCrossDependencies(subs []SubscriptionConfig, subNodes []*schema.Node) []string {
	count := map[string]int{}
	used := false
	for _, s := range subs {
		count[s.ID]++
		for _, src := range s.Sources {
			for _, d := range src.DependsOn {
				used = used || d.Subscription != ""
			}
		}
	}
	if !used {
		return nil
	}

	// Nodes are "i/j" (subscription and source index): ids are not unique.
	pos := map[string][2]int{}
	node := func(i, j int) string {
		id := fmt.Sprintf("%d/%d", i, j)
		pos[id] = [2]int{i, j}
		return id
	}
	var order []string
	edges := map[string][]string{}
	var warnings []string
	for i, sub := range subs {
		n := subNodes[i]
		first := map[string]int{}
		for j, src := range sub.Sources {
			if _, ok := first[src.ID]; !ok {
				first[src.ID] = j
			}
		}
		for j, src := range sub.Sources {
			from := node(i, j)
			order = append(order, from)
			for k, d := range src.DependsOn {
				if d.Subscription == "" {
					if t, ok := first[d.Source]; ok && d.Source != "" && d.Source != src.ID {
						edges[from] = append(edges[from], node(i, t))
					}
					continue
				}
				if d.Balancer == "" {
					continue // reported by parseDependency
				}
				dkey := fmt.Sprintf("sources.%d.dependsOn.%d.", j, k)
				switch {
				case d.Subscription == sub.ID:
					n.Addf(dkey+"subscription", "the source's own subscription: omit subscription")
					continue
				case count[d.Subscription] == 0:
					n.Addf(dkey+"subscription", "unknown subscription %q", d.Subscription)
					continue
				case count[d.Subscription] > 1:
					n.Addf(dkey+"subscription", "subscription id %q is not unique", d.Subscription)
					continue
				}
				oi := slices.IndexFunc(subs, func(s SubscriptionConfig) bool { return s.ID == d.Subscription })
				other := subs[oi]
				if src.Enabled && sub.Enabled && !other.Enabled {
					n.Addf(dkey+"subscription", "subscription %q is disabled", d.Subscription)
				}
				for t, o := range other.Sources {
					if o.Balancer == d.Balancer {
						edges[from] = append(edges[from], node(oi, t))
					}
				}
				_, b, ok := FindBalancer(subs, d.Subscription, d.Balancer)
				switch {
				case !ok:
					n.Addf(dkey+"balancer", "balancer %q is not in target.balancers of subscription %q", d.Balancer, d.Subscription)
				case b.Selection == nil || !b.Selection.Enabled:
					n.Addf(dkey+"balancer", "balancer %q of subscription %q has no enabled selection; nothing tracks its health",
						d.Balancer, d.Subscription)
				default:
					if st := b.Selection.Strategy; d.Health == HealthBalancer && (st == "random" || st == "roundRobin") {
						warnings = append(warnings, fmt.Sprintf("%s.%shealth: balancer %q of subscription %q has strategy %q, "+
							"which selects every candidate; the dependency is always healthy", n.Path(), dkey, d.Balancer, d.Subscription, st))
					}
				}
			}
		}
	}

	for _, cycle := range findCycles(order, edges) {
		i0, j0 := pos[cycle[0]][0], pos[cycle[0]][1]
		names := make([]string, 0, len(cycle)+1)
		cross := false
		for _, id := range cycle {
			i, j := pos[id][0], pos[id][1]
			cross = cross || i != i0
			names = append(names, subs[i].ID+"/"+subs[i].Sources[j].ID)
		}
		if cross {
			subNodes[i0].Addf(fmt.Sprintf("sources.%d.dependsOn", j0), "dependsOn cycle across subscriptions: %s",
				strings.Join(append(names, names[0]), " → "))
		}
	}
	return warnings
}

// dependencies returns the transitive dependencies of id in edges, each
// once, in depth-first order; edges must have no cycle through id.
func dependencies(id string, edges map[string][]string) []string {
	var out []string
	seen := map[string]bool{id: true}
	var walk func(string)
	walk = func(id string) {
		for _, d := range edges[id] {
			if !seen[d] {
				seen[d] = true
				out = append(out, d)
				walk(d)
			}
		}
	}
	walk(id)
	return out
}
