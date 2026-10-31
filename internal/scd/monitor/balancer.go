package monitor

import (
	"context"
	"log/slog"
	"time"

	"github.com/a1ekseev/scd/internal/scd/config"
	"github.com/a1ekseev/scd/internal/scd/state"
)

// BalancerTick checks the Xray balancer b of the target through the
// external SOCKS5 of its monitor (03 §4.5): observe-only (no repair, no target
// lock), with retries; it records the result in the balancer monitor state
// of b and schedules a remote push (up/OK/<latency> or down/<error>). Only a
// change of the check result is logged: balancer_degraded when it starts
// failing, balancer_recovered when it passes again. It is a no-op without socks5 and request or
// before a sync reached the target (SyncSeen; the balancer selection tick
// may have created it earlier). The error is reserved for a
// cancelled ctx, in which case nothing is recorded or pushed.
func BalancerTick(ctx context.Context, d Deps, sub config.SubscriptionConfig, b config.BalancerConfig) error {
	t := sub.Target
	if b.Monitor == nil {
		return nil
	}
	bm := *b.Monitor
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
			slog.String("balancerTag", b.Tag),
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
			m := &tg.EnsureBalancer(b.Tag).Monitor
			if m.State != state.BalancerDegraded {
				degraded, m.FailingSince = true, new(failedAt)
			}
			m.State = state.BalancerDegraded
			m.LastCheckedAt, m.LastFailureAt = new(failedAt), new(failedAt)
			m.LastStatusCode, m.LastLatencyMs = nil, nil
			m.LastError = msg
			m.ConsecutiveFailures++
		})
		if degraded {
			d.Log.LogAttrs(ctx, slog.LevelWarn, "Balancer monitor check failed; the balancer is degraded.",
				slog.String("event", "balancer_degraded"),
				slog.String("subscriptionId", sub.ID),
				slog.String("targetAddress", t.Address),
				slog.String("balancerTag", b.Tag),
				slog.String("error", msg))
		}
		d.scheduleRemotePing(ctx, sub, b, key, pingPayload{status: "down", msg: msg})
		return nil
	}

	checkedAt := d.now()
	code, latency := resp.StatusCode, resp.LatencyMs
	var since *time.Time
	recovered := false
	d.Memory.Update(key, func(tg *state.Target) {
		m := &tg.EnsureBalancer(b.Tag).Monitor
		recovered, since, m.FailingSince = m.State == state.BalancerDegraded, m.FailingSince, nil
		m.State = state.BalancerHealthy
		m.LastCheckedAt, m.LastSuccessAt = new(checkedAt), new(checkedAt)
		m.LastStatusCode, m.LastLatencyMs = new(code), new(latency)
		m.LastSuccessStatusCode, m.LastSuccessLatencyMs = new(code), new(latency)
		m.LastError = ""
		m.ConsecutiveFailures = 0
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
			slog.String("balancerTag", b.Tag),
			slog.Int64("failedForMs", failedFor))
	}
	d.scheduleRemotePing(ctx, sub, b, key, pingPayload{status: "up", msg: "OK", pingMs: &latency})
	return nil
}
