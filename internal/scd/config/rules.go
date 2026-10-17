package config

import (
	"github.com/a1ekseev/scd/internal/common/cron"
	"github.com/a1ekseev/scd/internal/common/listenaddr"
	"github.com/a1ekseev/scd/internal/common/schema"
)

const noPrefixWarning = "balancerSelection is enabled without observatorySubjectSelectorPrefix; " +
	"scd tunnels are unlikely to match the balancer selector"

// checkRules applies the cross-field rules (01 §2.3, 10 §6.5) to the parsed
// config. Every rule runs, whatever else failed. subNodes[i] is the node of
// cfg.Subscriptions[i]. It returns the warnings.
func checkRules(root *schema.Node, cfg *AppConfig, subNodes []*schema.Node) []string {
	checkSubscriptions(root, cfg.Subscriptions)

	if cfg.Runtime.Mode == "daemon" {
		if cfg.Runtime.Schedule == "" {
			root.Addf("runtime.schedule", `required when runtime.mode is "daemon"`)
		} else {
			checkCron(root, "runtime.schedule", cfg.Runtime.Schedule)
		}
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
	if s := t.InboundSocks; s != nil && s.PortRange.Start > 0 && s.PortRange.End > 0 && s.PortRange.End < s.PortRange.Start {
		n.Addf("target.inboundSocks.portRange.end", "must be greater than or equal to start")
	}

	if m := t.Monitor; m.Enabled {
		requireSchedule(n, "target.monitor", m.Schedule)
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
	if bm.Enabled {
		requireSchedule(n, "target.balancerMonitor", bm.Schedule)
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
	if bs := t.BalancerSelection; bs.Enabled {
		if bs.BalancerTag == "" {
			n.Addf("target.balancerSelection.balancerTag", "required when balancerSelection.enabled is true")
		}
		requireSchedule(n, "target.balancerSelection", bs.Schedule)
		if t.ObservatorySubjectSelectorPrefix == "" {
			warnings = append(warnings, n.Path()+".target.balancerSelection: "+noPrefixWarning)
		}
	}
	return warnings
}

// requireSchedule reports a missing or invalid <obj>.schedule of an enabled
// object (obj is the key path relative to n, e.g. "target.monitor").
func requireSchedule(n *schema.Node, obj, expr string) {
	if expr == "" {
		n.Addf(obj+".schedule", "required when %s.enabled is true", obj[len("target."):])
		return
	}
	checkCron(n, obj+".schedule", expr)
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
