package monitor

import (
	"context"
	"log/slog"
	"time"

	"github.com/a1ekseev/scd/internal/scd/config"
	"github.com/a1ekseev/scd/internal/scd/state"
)

// BalancerTick checks the target's Xray balancer through the external
// SOCKS5 of balancerMonitor (03 §4.5): observe-only (no repair, no target
// lock), with retries; it records the result in the balancer monitor state
// and schedules a remote push (up/OK/<latency> or down/<error>). Only a
// change of the check result is logged: balancer_degraded when it starts
// failing, balancer_recovered when it passes again. It is a no-op without socks5 and request or
// before a sync reached the target (SyncSeen; the balancer selection tick
// may have created it earlier). The error is reserved for a
// cancelled ctx, in which case nothing is recorded or pushed.
func BalancerTick(ctx context.Context, d Deps, sub config.SubscriptionConfig) error {
	t := sub.Target
	bm := t.BalancerMonitor
	if !bm.Enabled || bm.Socks5 == nil || bm.Request == nil {
		return nil
	}
	key := state.Key(sub.ID, t.Address)
	var synced bool
	d.Memory.View(key, func(tg *state.Target) { synced = tg.SyncSeen })
	if !synced {
		return nil
	}

	resp, err := d.probeWithRetry(ctx, probeSpec{
		host:    bm.Socks5.Host,
		port:    bm.Socks5.Port,
		request: bm.Request,
		retry:   bm.Retry,
		event:   "balancer_monitor_check_retry",
		message: "Balancer monitor check attempt failed; retrying.",
		attrs: []slog.Attr{
			slog.String("subscriptionId", sub.ID),
			slog.String("targetAddress", t.Address),
		},
	})
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		failedAt := d.now()
		msg := err.Error()
		degraded := false
		d.Memory.Update(key, func(tg *state.Target) {
			b := &tg.Balancer
			if b.State != state.BalancerDegraded {
				degraded, b.FailingSince = true, new(failedAt)
			}
			b.State = state.BalancerDegraded
			b.LastCheckedAt, b.LastFailureAt = new(failedAt), new(failedAt)
			b.LastStatusCode, b.LastLatencyMs = nil, nil
			b.LastError = msg
			b.ConsecutiveFailures++
		})
		if degraded {
			d.Log.LogAttrs(ctx, slog.LevelWarn, "Balancer monitor check failed; the balancer is degraded.",
				slog.String("event", "balancer_degraded"),
				slog.String("subscriptionId", sub.ID),
				slog.String("targetAddress", t.Address),
				slog.String("error", msg))
		}
		d.scheduleRemotePing(ctx, sub, key, pingPayload{status: "down", msg: msg})
		return nil
	}

	checkedAt := d.now()
	code, latency := resp.StatusCode, resp.LatencyMs
	var since *time.Time
	recovered := false
	d.Memory.Update(key, func(tg *state.Target) {
		b := &tg.Balancer
		recovered, since, b.FailingSince = b.State == state.BalancerDegraded, b.FailingSince, nil
		b.State = state.BalancerHealthy
		b.LastCheckedAt, b.LastSuccessAt = new(checkedAt), new(checkedAt)
		b.LastStatusCode, b.LastLatencyMs = new(code), new(latency)
		b.LastSuccessStatusCode, b.LastSuccessLatencyMs = new(code), new(latency)
		b.LastError = ""
		b.ConsecutiveFailures = 0
	})
	if recovered {
		var failedFor int64
		if since != nil {
			failedFor = checkedAt.Sub(*since).Milliseconds()
		}
		d.Log.LogAttrs(ctx, slog.LevelInfo, "Balancer monitor check recovered.",
			slog.String("event", "balancer_recovered"),
			slog.String("subscriptionId", sub.ID),
			slog.String("targetAddress", t.Address),
			slog.Int64("failedForMs", failedFor))
	}
	d.scheduleRemotePing(ctx, sub, key, pingPayload{status: "up", msg: "OK", pingMs: &latency})
	return nil
}
