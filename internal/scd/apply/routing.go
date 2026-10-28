package apply

import (
	"context"
	"slices"

	"github.com/a1ekseev/scd/internal/common/jsonout"
	"github.com/a1ekseev/scd/internal/scd/resources"
	"github.com/a1ekseev/scd/internal/scd/topology"
	"github.com/a1ekseev/scd/internal/scd/xrayapi"
)

// Routing replaces every non-fixed routing rule of the target with the
// rules of tunnels (02 §2.4, D2):
//
//  1. a tunnel route tag listed in fixed fails the report without any RPC;
//  2. ListRule; its error is returned as is (the caller turns it into a
//     FailedReport);
//  3. RemoveRule(ruleTag) for every listed rule whose ruleTag is not fixed
//     and that owns reports for KindRouting —
//     including a static rule without ruleTag, which sends RemoveRule("")
//     and fails with Xray's "app/router: empty tag name!" exactly as TS
//     does (O5: kept on purpose, list such rules in fixedRouting);
//  4. AddRule for every tunnel, one rule per call, routed to the tunnel's
//     current outbound tag: "replaced" when its ruleTag was listed in 2,
//     else "added".
//
// A failure in 3 or 4 ends the report with one failed item whose id is the
// first tunnel's route tag (or "routing"); there is no rollback. The
// report's Skipped is empty, as in TS.
func Routing(ctx context.Context, c xrayapi.RoutingAPI, fixed []string, owns Owner, tunnels []topology.Tunnel, m Meta) (Report, error) {
	startedAt := m.now()
	r := newReport(KindRouting, m, startedAt, nil)

	for _, tn := range tunnels {
		if slices.Contains(fixed, tn.RouteTag) {
			r.Failed = 1
			r.Items = append(r.Items, Item{
				ID:      tn.RouteTag,
				Status:  StatusFailed,
				Message: "Managed routing rule \"" + tn.RouteTag + "\" conflicts with fixedRouting on target \"" + m.TargetAddress + "\".",
			})
			r.DurationMs = m.now().Sub(startedAt).Milliseconds()
			return r, nil
		}
	}

	existing, err := c.ListRules(ctx)
	if err != nil {
		return Report{}, err
	}
	existingTags := map[string]bool{}
	for _, rl := range existing {
		existingTags[rl.RuleTag] = true
	}

	apply := func() error {
		for _, rl := range existing {
			if slices.Contains(fixed, rl.RuleTag) || !owns(KindRouting, rl.RuleTag) {
				continue
			}
			if err := c.RemoveRule(ctx, rl.RuleTag); err != nil {
				return err
			}
			r.Removed++
			r.DeletedIDs = append(r.DeletedIDs, rl.RuleTag)
			r.Items = append(r.Items, Item{ID: rl.RuleTag, Status: StatusRemoved})
		}
		for _, tn := range tunnels {
			if err := c.AddRule(ctx, resources.Rule(tn, tn.OutboundTagCurrent)); err != nil {
				return err
			}
			r.AppliedIDs = append(r.AppliedIDs, tn.RouteTag)
			r.Items = append(r.Items, Item{ID: tn.RouteTag, Status: r.countAddition(existingTags[tn.RouteTag])})
		}
		return nil
	}

	if err := apply(); err != nil {
		id := "routing"
		if len(tunnels) > 0 {
			id = tunnels[0].RouteTag
		}
		r.Failed = 1
		r.Items = append(r.Items, Item{ID: id, Status: StatusFailed, Message: err.Error()})
	}

	r.DurationMs = m.now().Sub(startedAt).Milliseconds()
	r.AppliedAt = jsonout.Timestamp(m.now())
	return r, nil
}
