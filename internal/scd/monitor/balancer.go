package monitor

import (
	"context"
	"log/slog"

	"github.com/a1ekseev/scd/internal/scd/config"
	"github.com/a1ekseev/scd/internal/scd/state"
)

// BalancerTick checks the target's Xray balancer through the external
// SOCKS5 of balancerMonitor (03 §4.5): observe-only (no repair, no target
// lock), with retries; it records the result in the balancer monitor state
// and schedules a remote push (up/OK/<latency> or down/<error>). The check
// result itself is not logged. It is a no-op without socks5 and request or
// before the first sync created the target. The error is reserved for a
// cancelled ctx, in which case nothing is recorded or pushed.
func BalancerTick(ctx context.Context, d Deps, sub config.SubscriptionConfig) error {
	t := sub.Target
	bm := t.BalancerMonitor
	if !bm.Enabled || bm.Socks5 == nil || bm.Request == nil {
		return nil
	}
	key := state.Key(sub.ID, t.Address)
	if !d.Memory.View(key, func(*state.Target) {}) {
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
		d.Memory.Update(key, func(tg *state.Target) {
			b := &tg.Balancer
			b.State = state.BalancerDegraded
			b.LastCheckedAt, b.LastFailureAt = &failedAt, &failedAt
			b.LastStatusCode, b.LastLatencyMs = nil, nil
			b.LastError = msg
			b.ConsecutiveFailures++
		})
		d.scheduleRemotePing(ctx, sub, key, pingPayload{status: "down", msg: msg})
		return nil
	}

	checkedAt := d.now()
	code, latency := resp.StatusCode, resp.LatencyMs
	d.Memory.Update(key, func(tg *state.Target) {
		b := &tg.Balancer
		b.State = state.BalancerHealthy
		b.LastCheckedAt, b.LastSuccessAt = &checkedAt, &checkedAt
		c, l := code, latency
		b.LastStatusCode, b.LastLatencyMs = &c, &l
		b.LastSuccessStatusCode, b.LastSuccessLatencyMs = &c, &l
		b.LastError = ""
		b.ConsecutiveFailures = 0
	})
	d.scheduleRemotePing(ctx, sub, key, pingPayload{status: "up", msg: "OK", pingMs: &latency})
	return nil
}
