package monitor

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/a1ekseev/scd/internal/scd/apply"
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
	stateLostMessage   = "Xray has no inbound of the tunnel; the next sync applies the source again."
)

// Flap logging: a flap is a successful repair of the first failed check of
// a series. These describe noise, not behaviour, so they are not config.
const (
	flapLimit   = 3                // flaps within flapWindow before tunnel_flapping
	flapWindow  = 60 * time.Minute // window of flapLimit
	stableAfter = 60 * time.Minute // no failed check before tunnel_stable
	downAfter   = 3                // failed checks in a row in an episode before tunnel_down
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

// dial connects to the target of sub for mutations: AddOutbound of a
// source with maxActive goes through apply.Capped.
func (d Deps) dial(sub config.SubscriptionConfig) (xrayapi.Client, error) {
	c, err := d.Dial(sub.Target.Address, time.Duration(sub.Target.TimeoutMs)*time.Millisecond)
	if err != nil {
		return nil, err
	}
	return apply.Capped(c, sub.Target, sub.Sources), nil
}

// capRefused resets the hashes of the tunnel's source when err is an
// AddOutbound refused by apply.Capped: the next sync applies it in full.
func capRefused(m *state.Memory, key string, tn topology.Tunnel, err error) {
	if errors.Is(err, apply.ErrCapReached) {
		m.Update(key, func(tg *state.Target) { tg.ResetSourceHashes(tn.SourceID) })
	}
}

// Repair moves a tunnel whose final probe failed to its unprefixed outbound
// (03 §4.2, 02 §2.5) under the target lock. It first reads ListInbounds: a
// tunnel whose inbound is gone (Xray lost its state) is not repaired, its
// source hashes are reset for a full apply by the next sync and
// xray_state_lost is logged. Otherwise: RemoveRule(route) (best effort)
// and RemoveOutbound(current), then AddOutbound(unprefixed) and
// AddRule(route → unprefixed). A failed RemoveOutbound stops it before the
// add and restores AddRule(route → current) best effort. Success leaves the tunnel degraded with
// "Tunnel repaired; …" until the next successful check; an RPC failure is
// recorded on the tunnel and logged (tunnel_repair_failed, at debug when
// the error is the same as the last repair's or the Xray API is
// unavailable). A successful repair of the first failed check of a series
// is a flap: the third within flapWindow logs tunnel_flapping, and inside
// the episode tunnel_repaired is debug. A tunnel that is
// gone is ignored. A tunnel that already runs on its unprefixed outbound
// (repaired, not yet rejoined) is left alone: no RPC, only a debug log
// (tunnel_repair_skipped); checkTunnel has already recorded the failure. The
// check is made under the target lock, so a sync that replaced the topology
// meanwhile (prefixed again) gets a real repair. The only returned error is
// a failure to create the client (TS createMutationClient outside the try);
// the tunnel then stays "repairing" until its next check, as in TS.
func Repair(ctx context.Context, d Deps, sub config.SubscriptionConfig, tunnelID string) error {
	t := sub.Target
	key := state.Key(sub.ID, t.Address)
	unlock := d.Memory.LockTarget(key)
	defer unlock()

	var (
		cur              state.Tunnel
		found, withdrawn bool
	)
	updateTunnel(d.Memory, key, tunnelID, func(tn *state.Tunnel) {
		found, withdrawn = true, shouldRejoin(tn.Mapping)
		if !withdrawn {
			tn.Monitor.State = state.TunnelRepairing
		}
		cur = *tn
	})
	if !found {
		return nil
	}
	if withdrawn {
		// Already out of the balancer: recreating the same unprefixed
		// outbound on every failed check only churns Xray.
		d.Log.LogAttrs(ctx, slog.LevelDebug, "Tunnel already out of the balancer; repair skipped.",
			slog.String("event", "tunnel_repair_skipped"),
			slog.String("subscriptionId", sub.ID),
			slog.String("sourceId", cur.Mapping.SourceID),
			slog.String("targetAddress", t.Address),
			slog.String("tunnelId", tunnelID),
			slog.String("outboundTag", cur.Mapping.OutboundTagCurrent))
		return nil
	}
	client, err := d.dial(sub)
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()

	actx := context.WithoutCancel(ctx)
	repaired := cur.Mapping
	repaired.OutboundTagCurrent = repaired.OutboundWithoutPrefix.Tag
	present, err := hasInbound(actx, client, cur.Mapping.InboundTag)
	if err == nil {
		state.NoteXray(ctx, d.Memory, d.Log, sub.ID, t.Address, nil, d.now())
		if !present {
			d.stateLost(ctx, sub, cur.Mapping)
			return nil
		}
	}
	added := false
	if err == nil {
		added, err = repairCalls(actx, client, cur.Mapping, repaired)
	}
	if err != nil {
		failedAt := d.now()
		capRefused(d.Memory, key, cur.Mapping, err)
		down, _ := state.NoteXray(ctx, d.Memory, d.Log, sub.ID, t.Address, err, failedAt)
		level := slog.LevelError
		updateTunnel(d.Memory, key, tunnelID, func(tn *state.Tunnel) {
			if added {
				// The unprefixed outbound is in Xray; only its rule is missing.
				tn.Mapping = repaired
			}
			tn.Monitor.State = state.TunnelDegraded
			tn.Monitor.LastFailureAt = &failedAt
			tn.Monitor.LastError = err.Error()
			if down || tn.Monitor.LastRepairError == err.Error() {
				level = slog.LevelDebug // xray_unavailable, or the same failure as the last repair
			}
			tn.Monitor.LastRepairError = err.Error()
		})
		d.Log.LogAttrs(ctx, level, "Tunnel repair failed.",
			slog.String("event", "tunnel_repair_failed"),
			slog.String("subscriptionId", sub.ID),
			slog.String("sourceId", cur.Mapping.SourceID),
			slog.String("targetAddress", t.Address),
			slog.String("tunnelId", tunnelID),
			slog.String("error", err.Error()))
		return nil
	}

	now := d.now()
	level := slog.LevelWarn
	d.Memory.View(key, func(tg *state.Target) {
		if st := tg.Sources[cur.Mapping.SourceID]; st != nil && st.NoLiveSince != nil {
			level = slog.LevelDebug // the source has no live candidate: candidate_no_replacement covers it
		}
	})
	var flap state.FlapState
	flapping := false
	updateTunnel(d.Memory, key, tunnelID, func(tn *state.Tunnel) {
		tn.Mapping = repaired
		m := &tn.Monitor
		if m.FailuresInRow == 1 {
			flapping = addFlap(&m.Flap, now)
		}
		// A tunnel without a prefix is repaired on every failed check; only
		// the first repair of a series (ConsecutiveFailures not yet reset
		// by one) and none inside a flap episode is warn.
		if m.ConsecutiveFailures != m.FailuresInRow || !m.Flap.Since.IsZero() {
			level = slog.LevelDebug
		}
		flap = m.Flap
		m.State = state.TunnelDegraded
		m.LastStatusCode, m.LastLatencyMs = nil, nil
		m.ConsecutiveFailures = 0
		m.LastError = repairedMessage
		m.LastRepairError = ""
	})
	d.Log.LogAttrs(ctx, level, "Tunnel repaired after failed monitor probe.",
		slog.String("event", "tunnel_repaired"),
		slog.String("subscriptionId", sub.ID),
		slog.String("sourceId", cur.Mapping.SourceID),
		slog.String("targetAddress", t.Address),
		slog.String("tunnelId", tunnelID),
		slog.String("outboundTag", repaired.OutboundTagCurrent),
		slog.String("probeError", cur.Monitor.LastError),
		slog.Int("failuresInRow", cur.Monitor.FailuresInRow))
	if flapping {
		d.Log.LogAttrs(ctx, slog.LevelWarn, "Tunnel keeps failing and recovering; its repairs are logged at debug until it is stable.",
			slog.String("event", "tunnel_flapping"),
			slog.String("subscriptionId", sub.ID),
			slog.String("sourceId", cur.Mapping.SourceID),
			slog.String("targetAddress", t.Address),
			slog.String("tunnelId", tunnelID),
			slog.String("outboundTag", repaired.OutboundTagCurrent),
			slog.Int("flaps", flap.Count),
			slog.Int("windowMin", int(flapWindow/time.Minute)),
			slog.String("probeError", cur.Monitor.LastError))
	}
	return nil
}

// addFlap records a flap of the tunnel at now and reports whether it
// starts an episode: flapLimit flaps within flapWindow. Inside an episode
// it only counts.
func addFlap(f *state.FlapState, now time.Time) (started bool) {
	if !f.Since.IsZero() {
		f.Count++
		return false
	}
	kept := make([]time.Time, 0, flapLimit)
	for _, at := range f.Times {
		if now.Sub(at) < flapWindow {
			kept = append(kept, at)
		}
	}
	kept = append(kept, now)
	f.Times = kept
	if len(f.Times) < flapLimit {
		return false
	}
	f.Since, f.Count, f.Times = f.Times[0], len(f.Times), nil
	return true
}

// hasInbound reports whether Xray has the inbound tag (one ListInbounds).
func hasInbound(ctx context.Context, c xrayapi.Client, tag string) (bool, error) {
	list, err := c.ListInbounds(ctx)
	if err != nil {
		return false, err
	}
	for _, in := range list {
		if in.GetTag() == tag {
			return true, nil
		}
	}
	return false, nil
}

// stateLost handles a repair of tunnel tn whose inbound is gone from Xray
// (Xray restarted): no RPC, the hashes of its source are reset so the next
// sync applies the source in full, and xray_state_lost is logged, at warn
// only for the first tunnel of the target's episode.
func (d Deps) stateLost(ctx context.Context, sub config.SubscriptionConfig, tn topology.Tunnel) {
	now := d.now()
	first := false
	d.Memory.Update(state.Key(sub.ID, sub.Target.Address), func(tg *state.Target) {
		tg.ResetSourceHashes(tn.SourceID)
		first = tg.SetStateLost(tn.SourceID, now)
		for _, x := range tg.Tunnels {
			if x.Mapping.BaseTunnelID == tn.BaseTunnelID {
				x.Monitor.State = state.TunnelDegraded
				x.Monitor.LastError = stateLostMessage
			}
		}
	})
	level := slog.LevelDebug
	if first {
		level = slog.LevelWarn
	}
	d.Log.LogAttrs(ctx, level, stateLostMessage,
		slog.String("event", "xray_state_lost"),
		slog.String("subscriptionId", sub.ID),
		slog.String("targetAddress", sub.Target.Address),
		slog.String("sourceId", tn.SourceID),
		slog.String("tunnelId", tn.BaseTunnelID))
}

// repairCalls is the RPC sequence of a repair; cur is the tunnel before it.
// added reports that the unprefixed outbound was added.
func repairCalls(ctx context.Context, c xrayapi.Client, cur, repaired topology.Tunnel) (added bool, err error) {
	h, err := outbound.ToProto(repaired.OutboundWithoutPrefix.JSON)
	if err != nil {
		return false, err
	}
	if err := removeTunnel(ctx, c, cur); err != nil {
		return false, err
	}
	if err := c.AddOutbound(ctx, h); err != nil {
		return false, err
	}
	return true, c.AddRule(ctx, resources.Rule(repaired, repaired.OutboundTagCurrent))
}

// removeTunnel removes the tunnel's rule (best effort) and current outbound
// before a new one is added: a provider may limit simultaneous connections
// per key, so the tunnel never runs on two outbounds at once. If the outbound
// cannot be removed, the rule is restored best effort and the error stops the
// operation before any add. An unknown tag is no error in Xray, so a repeated
// repair or rejoin goes ahead.
func removeTunnel(ctx context.Context, c xrayapi.Client, cur topology.Tunnel) error {
	_ = c.RemoveRule(ctx, cur.RouteTag)
	if err := c.RemoveOutbound(ctx, cur.OutboundTagCurrent); err != nil {
		_ = c.AddRule(ctx, resources.Rule(cur, cur.OutboundTagCurrent))
		return err
	}
	return nil
}

// shouldRejoin reports whether the tunnel has an observatory prefix and
// currently runs on its unprefixed (repaired) outbound.
func shouldRejoin(m topology.Tunnel) bool {
	return m.OutboundTagCurrent != m.OutboundTagInitial && m.OutboundTagInitial != m.OutboundWithoutPrefix.Tag
}

// Rejoin returns a repaired tunnel to its prefixed outbound, and so to the
// Xray balancer, after a successful probe (03 §4.3, 02 §2.5). The check is
// made before the lock (no client is created for a balanced tunnel) and
// again under it. Sequence, remove before add: RemoveRule(route) (best
// effort) and RemoveOutbound(unprefixed), then AddOutbound(initial) and
// AddRule(route → initial). A failed RemoveOutbound stops it before the add
// and restores AddRule(route → unprefixed) best effort; if either add fails it is rolled back best effort
// with RemoveOutbound(initial), AddOutbound(unprefixed), AddRule(route →
// unprefixed). Any failure keeps the tunnel healthy with "Tunnel healthy but balancer rejoin
// failed: <msg>" and is logged (tunnel_rejoin_failed). The only returned
// error is a failure to create the client.
func Rejoin(ctx context.Context, d Deps, sub config.SubscriptionConfig, tunnelID string) error {
	return rejoin(ctx, d, sub, tunnelID, nil)
}

// rejoin is Rejoin; a non-nil failedFor (the tunnel's failed time, after a
// failed check) goes into tunnel_rejoined_balancer as failedForMs, which
// then is the only line of the recovery.
func rejoin(ctx context.Context, d Deps, sub config.SubscriptionConfig, tunnelID string, failedFor *int64) error {
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
	client, err := d.dial(sub)
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()

	rejoined := cur.Mapping
	rejoined.OutboundTagCurrent = rejoined.OutboundTagInitial
	if err := rejoinCalls(context.WithoutCancel(ctx), client, cur.Mapping, rejoined); err != nil {
		capRefused(d.Memory, key, cur.Mapping, err)
		down, _ := state.NoteXray(ctx, d.Memory, d.Log, sub.ID, t.Address, err, d.now())
		_, stuck := errors.AsType[*stuckOnInitialError](err)
		repeat := false
		updateTunnel(d.Memory, key, tunnelID, func(tn *state.Tunnel) {
			if stuck {
				tn.Mapping = rejoined
			}
			tn.Monitor.LastError = rejoinFailedPrefix + err.Error()
			repeat = noteRejoinError(&tn.Monitor, err)
		})
		d.logRejoinFailed(ctx, sub, cur.Mapping.SourceID, tunnelID, err, down || repeat)
		return nil
	}
	state.NoteXray(ctx, d.Memory, d.Log, sub.ID, t.Address, nil, d.now())

	updateTunnel(d.Memory, key, tunnelID, func(tn *state.Tunnel) {
		tn.Mapping = rejoined
		tn.Monitor.LastError, tn.Monitor.LastRejoinError = "", ""
	})
	attrs := []slog.Attr{
		slog.String("event", "tunnel_rejoined_balancer"),
		slog.String("subscriptionId", sub.ID),
		slog.String("sourceId", cur.Mapping.SourceID),
		slog.String("targetAddress", t.Address),
		slog.String("tunnelId", tunnelID),
		slog.String("outboundTag", rejoined.OutboundTagCurrent),
	}
	if failedFor != nil {
		attrs = append(attrs, slog.Int64("failedForMs", *failedFor))
	}
	d.Log.LogAttrs(ctx, flapLevel(cur.Monitor, slog.LevelInfo), "Tunnel rejoined Xray balancer after successful monitor probe.", attrs...)
	return nil
}

// rejoinCalls is the RPC sequence of a rejoin with its rollback; cur is the
// tunnel before it.
func rejoinCalls(ctx context.Context, c xrayapi.Client, cur, rejoined topology.Tunnel) error {
	initial, err := outbound.ToProto(rejoined.OutboundInitial.JSON)
	if err != nil {
		return err
	}
	unprefixed, err := outbound.ToProto(cur.OutboundWithoutPrefix.JSON)
	if err != nil {
		return err
	}
	if err := removeTunnel(ctx, c, cur); err != nil {
		return err
	}
	err = c.AddOutbound(ctx, initial)
	if err == nil {
		err = c.AddRule(ctx, resources.Rule(rejoined, rejoined.OutboundTagCurrent))
	}
	if err != nil {
		if rerr := c.RemoveOutbound(ctx, rejoined.OutboundTagCurrent); rerr != nil {
			// The initial outbound may still exist: adding the unprefixed one
			// could run the tunnel on two outbounds.
			return &stuckOnInitialError{err}
		}
		_ = c.AddOutbound(ctx, unprefixed)
		_ = c.AddRule(ctx, resources.Rule(cur, cur.OutboundTagCurrent))
		return err
	}
	return nil
}

// stuckOnInitialError is a rejoin error after which the rollback could not
// remove the initial outbound: the tunnel is then recorded on it, so the
// next repair removes it first.
type stuckOnInitialError struct{ error }

func (e *stuckOnInitialError) Unwrap() error { return e.error }

// noteRejoinError records err as the tunnel's last rejoin error and
// reports whether it repeats the previous one.
func noteRejoinError(m *state.TunnelMonitor, err error) (repeat bool) {
	repeat = m.LastRejoinError == err.Error()
	m.LastRejoinError = err.Error()
	return repeat
}

// logRejoinFailed logs tunnel_rejoin_failed, at debug when quiet: the Xray
// API is unavailable (xray_unavailable covers it) or the error repeats the
// tunnel's last rejoin error.
func (d Deps) logRejoinFailed(ctx context.Context, sub config.SubscriptionConfig, sourceID, tunnelID string, err error, quiet bool) {
	level := slog.LevelError
	if quiet {
		level = slog.LevelDebug
	}
	d.Log.LogAttrs(ctx, level, "Tunnel balancer rejoin failed after successful monitor probe.",
		slog.String("event", "tunnel_rejoin_failed"),
		slog.String("subscriptionId", sub.ID),
		slog.String("sourceId", sourceID),
		slog.String("targetAddress", sub.Target.Address),
		slog.String("tunnelId", tunnelID),
		slog.String("error", err.Error()))
}

// flapLevel is level, or debug while the tunnel is in a flap episode.
func flapLevel(m state.TunnelMonitor, level slog.Level) slog.Level {
	if !m.Flap.Since.IsZero() {
		return slog.LevelDebug
	}
	return level
}
