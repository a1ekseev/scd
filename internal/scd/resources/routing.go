package resources

import (
	"github.com/xtls/xray-core/app/router"

	"github.com/a1ekseev/scd/internal/scd/topology"
)

// Rule returns the routing rule that sends the tunnel's inbound to
// outboundTag under the tunnel's ruleTag (02 §1.6).
func Rule(t topology.Tunnel, outboundTag string) *router.RoutingRule {
	return &router.RoutingRule{
		TargetTag:  &router.RoutingRule_Tag{Tag: outboundTag},
		InboundTag: []string{t.InboundTag},
		RuleTag:    t.RouteTag,
	}
}
