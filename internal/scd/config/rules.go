package config

import (
	"fmt"
	"net"
	"regexp"
	"strings"

	"github.com/a1ekseev/scd/internal/common/cron"
	"github.com/a1ekseev/scd/internal/common/listenaddr"
	"github.com/a1ekseev/scd/internal/common/schema"
)

const noSourcesWarning = "the subscription is enabled but none of its sources is; " +
	"sync will remove every non-fixed resource of its target"

const noPrefixWarning = "balancerSelection is enabled without observatorySubjectSelectorPrefix; " +
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
		if s.Enabled && len(s.Sources) > 0 && len(s.EnabledSources()) == 0 {
			warnings = append(warnings, subNodes[i].Path()+": "+noSourcesWarning)
		}
	}

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

	bm := t.BalancerMonitor
	checkSchedule(n, "target.balancerMonitor", bm.Schedule, bm.Enabled)
	if bm.Enabled {
		if bm.Socks5 == nil {
			n.Addf("target.balancerMonitor.socks5", "required when balancerMonitor.enabled is true")
		}
		if bm.Request == nil {
			n.Addf("target.balancerMonitor.request", "required when balancerMonitor.enabled is true")
		}
	}
	if bm.RemotePing.Enabled && !bm.Enabled {
		n.Addf("target.balancerMonitor.remotePing.enabled", "requires balancerMonitor.enabled")
	}

	var warnings []string
	bs := t.BalancerSelection
	checkSchedule(n, "target.balancerSelection", bs.Schedule, bs.Enabled)
	if bs.Enabled {
		if bs.BalancerTag == "" {
			n.Addf("target.balancerSelection.balancerTag", "required when balancerSelection.enabled is true")
		}
		if t.ObservatorySubjectSelectorPrefix == "" {
			warnings = append(warnings, n.Path()+".target.balancerSelection: "+noPrefixWarning)
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
		n.Addf(obj+".schedule", "required when %s.enabled is true", obj[len("target."):])
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
// node n.
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
		if src.Type == SourceWireGuard && (!res.Inbounds.Enabled || !res.Routing.Enabled) {
			n.Addf(key+"type", "%q requires resources.inbounds.enabled and resources.routing.enabled", src.Type)
		}
		if !ValidSourceID(src.ID) {
			continue
		}
		p := src.ID + "-"
		if obs != "" && (strings.HasPrefix(obs, p) || strings.HasPrefix(p, obs)) {
			n.Addf(key+"id", "namespace %q overlaps target.observatorySubjectSelectorPrefix %q", p, obs)
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
