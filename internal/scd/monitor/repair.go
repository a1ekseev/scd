package monitor

import (
	"context"
	"log/slog"
	"time"

	"github.com/a1ekseev/scd/internal/scd/config"
	"github.com/a1ekseev/scd/internal/scd/outbound"
	"github.com/a1ekseev/scd/internal/scd/resources"
	"github.com/a1ekseev/scd/internal/scd/state"
	"github.com/a1ekseev/scd/internal/scd/topology"
	"github.com/a1ekseev/scd/internal/scd/xrayapi"
)

const (
	repairedMessage    = "Tunnel repaired; awaiting next successful monitor check."
	rejoinFailedPrefix = "Tunnel healthy but balancer rejoin failed: "
)

// viewTunnel returns a copy of tunnel id of key.
func viewTunnel(m *state.Memory, key, id string) (state.Tunnel, bool) {
	var (
		out   state.Tunnel
		found bool
	)
	m.View(key, func(tg *state.Target) {
		for _, tn := range tg.Tunnels {
			if tn.Mapping.BaseTunnelID == id {
				out, found = *tn, true
				return
			}
		}
	})
	return out, found
}

func (d Deps) dial(t config.SubscriptionTargetConfig) (xrayapi.Client, error) {
	return d.Dial(t.Address, time.Duration(t.TimeoutMs)*time.Millisecond)
}

// Repair moves a tunnel whose final probe failed to its unprefixed outbound
// (03 §4.2, 02 §2.5) under the target lock: RemoveRule(route) and
// RemoveOutbound(current), both best effort, then AddOutbound(unprefixed)
// and AddRule(route → unprefixed). Success leaves the tunnel degraded with
// "Tunnel repaired; …" until the next successful check; an RPC failure is
// recorded on the tunnel and logged (tunnel_repair_failed). A tunnel that is
// gone is ignored. The only returned error is a failure to create the
// client (TS createMutationClient outside the try); the tunnel then stays
// "repairing" until its next check, as in TS.
func Repair(ctx context.Context, d Deps, sub config.SubscriptionConfig, tunnelID string) error {
	t := sub.Target
	key := state.Key(sub.ID, t.Address)
	unlock := d.Memory.LockTarget(key)
	defer unlock()

	var (
		cur   state.Tunnel
		found bool
	)
	updateTunnel(d.Memory, key, tunnelID, func(tn *state.Tunnel) {
		tn.Monitor.State = state.TunnelRepairing
		cur, found = *tn, true
	})
	if !found {
		return nil
	}
	client, err := d.dial(t)
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()

	repaired := cur.Mapping
	repaired.OutboundTagCurrent = repaired.OutboundWithoutPrefix.Tag
	if err := repairCalls(context.WithoutCancel(ctx), client, cur.Mapping, repaired); err != nil {
		failedAt := d.now()
		updateTunnel(d.Memory, key, tunnelID, func(tn *state.Tunnel) {
			tn.Monitor.State = state.TunnelDegraded
			tn.Monitor.LastFailureAt = &failedAt
			tn.Monitor.LastError = err.Error()
		})
		d.Log.LogAttrs(ctx, slog.LevelError, "Tunnel repair failed.",
			slog.String("event", "tunnel_repair_failed"),
			slog.String("subscriptionId", sub.ID),
			slog.String("targetAddress", t.Address),
			slog.String("tunnelId", tunnelID),
			slog.String("error", err.Error()))
		return nil
	}

	updateTunnel(d.Memory, key, tunnelID, func(tn *state.Tunnel) {
		tn.Mapping = repaired
		m := &tn.Monitor
		m.State = state.TunnelDegraded
		m.LastStatusCode, m.LastLatencyMs = nil, nil
		m.ConsecutiveFailures = 0
		m.LastError = repairedMessage
	})
	d.Log.LogAttrs(ctx, slog.LevelWarn, "Tunnel repaired after failed monitor probe.",
		slog.String("event", "tunnel_repaired"),
		slog.String("subscriptionId", sub.ID),
		slog.String("targetAddress", t.Address),
		slog.String("tunnelId", tunnelID),
		slog.String("outboundTag", repaired.OutboundTagCurrent))
	return nil
}

// repairCalls is the RPC sequence of a repair; cur is the tunnel before it.
func repairCalls(ctx context.Context, c xrayapi.Client, cur, repaired topology.Tunnel) error {
	_ = c.RemoveRule(ctx, cur.RouteTag)
	_ = c.RemoveOutbound(ctx, cur.OutboundTagCurrent)
	h, err := outbound.ToProto(repaired.OutboundWithoutPrefix.JSON)
	if err != nil {
		return err
	}
	if err := c.AddOutbound(ctx, h); err != nil {
		return err
	}
	return c.AddRule(ctx, resources.Rule(repaired, repaired.OutboundTagCurrent))
}

// shouldRejoin reports whether the tunnel has an observatory prefix and
// currently runs on its unprefixed (repaired) outbound.
func shouldRejoin(m topology.Tunnel) bool {
	return m.OutboundTagCurrent != m.OutboundTagInitial && m.OutboundTagInitial != m.OutboundWithoutPrefix.Tag
}

// Rejoin returns a repaired tunnel to its prefixed outbound, and so to the
// Xray balancer, after a successful probe (03 §4.3, 02 §2.5). The check is
// made before the lock (no client is created for a balanced tunnel) and
// again under it. Sequence: AddOutbound(initial); then RemoveRule(route)
// (best effort), AddRule(route → initial), RemoveOutbound(unprefixed) (best
// effort); if that block fails it is rolled back best effort with
// RemoveRule(route), AddRule(route → current), RemoveOutbound(initial). Any
// failure keeps the tunnel healthy with "Tunnel healthy but balancer rejoin
// failed: <msg>" and is logged (tunnel_rejoin_failed). The only returned
// error is a failure to create the client.
func Rejoin(ctx context.Context, d Deps, sub config.SubscriptionConfig, tunnelID string) error {
	t := sub.Target
	key := state.Key(sub.ID, t.Address)
	if snap, ok := viewTunnel(d.Memory, key, tunnelID); !ok || !shouldRejoin(snap.Mapping) {
		return nil
	}
	unlock := d.Memory.LockTarget(key)
	defer unlock()

	cur, ok := viewTunnel(d.Memory, key, tunnelID)
	if !ok || !shouldRejoin(cur.Mapping) {
		return nil
	}
	client, err := d.dial(t)
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()

	rejoined := cur.Mapping
	rejoined.OutboundTagCurrent = rejoined.OutboundTagInitial
	if err := rejoinCalls(context.WithoutCancel(ctx), client, cur.Mapping, rejoined); err != nil {
		updateTunnel(d.Memory, key, tunnelID, func(tn *state.Tunnel) {
			tn.Monitor.LastError = rejoinFailedPrefix + err.Error()
		})
		d.logRejoinFailed(ctx, sub, tunnelID, err)
		return nil
	}

	updateTunnel(d.Memory, key, tunnelID, func(tn *state.Tunnel) {
		tn.Mapping = rejoined
		tn.Monitor.LastError = ""
	})
	d.Log.LogAttrs(ctx, slog.LevelInfo, "Tunnel rejoined Xray balancer after successful monitor probe.",
		slog.String("event", "tunnel_rejoined_balancer"),
		slog.String("subscriptionId", sub.ID),
		slog.String("targetAddress", t.Address),
		slog.String("tunnelId", tunnelID),
		slog.String("outboundTag", rejoined.OutboundTagCurrent))
	return nil
}

// rejoinCalls is the RPC sequence of a rejoin with its rollback; cur is the
// tunnel before it.
func rejoinCalls(ctx context.Context, c xrayapi.Client, cur, rejoined topology.Tunnel) error {
	h, err := outbound.ToProto(rejoined.OutboundInitial.JSON)
	if err != nil {
		return err
	}
	if err := c.AddOutbound(ctx, h); err != nil {
		return err
	}
	_ = c.RemoveRule(ctx, cur.RouteTag)
	if err := c.AddRule(ctx, resources.Rule(rejoined, rejoined.OutboundTagCurrent)); err != nil {
		_ = c.RemoveRule(ctx, cur.RouteTag)
		_ = c.AddRule(ctx, resources.Rule(cur, cur.OutboundTagCurrent))
		_ = c.RemoveOutbound(ctx, rejoined.OutboundTagCurrent)
		return err
	}
	_ = c.RemoveOutbound(ctx, cur.OutboundTagCurrent)
	return nil
}

func (d Deps) logRejoinFailed(ctx context.Context, sub config.SubscriptionConfig, tunnelID string, err error) {
	d.Log.LogAttrs(ctx, slog.LevelError, "Tunnel balancer rejoin failed after successful monitor probe.",
		slog.String("event", "tunnel_rejoin_failed"),
		slog.String("subscriptionId", sub.ID),
		slog.String("targetAddress", sub.Target.Address),
		slog.String("tunnelId", tunnelID),
		slog.String("error", err.Error()))
}
