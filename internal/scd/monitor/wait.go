package monitor

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"time"

	"github.com/a1ekseev/scd/internal/scd/config"
	"github.com/a1ekseev/scd/internal/scd/state"
	"github.com/a1ekseev/scd/internal/scd/xrayapi"
)

// dependents returns the enabled sources of sub with dependsOn.
func dependents(sub config.SubscriptionConfig) []config.SourceConfig {
	var out []config.SourceConfig
	for _, src := range sub.EnabledSources() {
		if len(src.DependsOn) > 0 {
			out = append(out, src)
		}
	}
	return out
}

// dependencyBalancer is the balancer whose selection is the health of
// dependency dep of sub ("" when the dependency has none: config rejects
// it).
func dependencyBalancer(sub config.SubscriptionConfig, dep config.DependencyConfig) string {
	if s := sourceOf(sub, dep.Source); s != nil {
		return s.Balancer
	}
	return ""
}

// balancerTags returns the balancers of the dependencies of srcs, each
// once, in order.
func balancerTags(sub config.SubscriptionConfig, srcs []config.SourceConfig) []string {
	var tags []string
	for _, src := range srcs {
		for _, dep := range src.DependsOn {
			if b := dependencyBalancer(sub, dep); b != "" && !slices.Contains(tags, b) {
				tags = append(tags, b)
			}
		}
	}
	return tags
}

// checkBalancers asks Xray for the selection of every tag (one
// GetBalancerInfo each, read-only): a balancer is healthy when it selects
// an outbound (an override or a principle target), unhealthy when it
// selects none, unknown when the call failed. noteErr is what the calls
// mean for state.NoteXray: nil when the API answered (BalancerNotFound and
// ServiceDisabled are answers), else the first other error.
func checkBalancers(ctx context.Context, c xrayapi.Client, tags []string) (health map[string]string, noteErr error) {
	health = make(map[string]string, len(tags))
	for _, tag := range tags {
		info, err := c.GetBalancerInfo(ctx, tag)
		var (
			notFound *xrayapi.BalancerNotFoundError
			disabled *xrayapi.ServiceDisabledError
		)
		switch {
		case err == nil && (info.OverrideTarget != "" || len(info.PrincipleTargets) > 0):
			health[tag] = state.DepHealthy
		case err == nil:
			health[tag] = state.DepUnhealthy
		case errors.As(err, &notFound), errors.As(err, &disabled):
			health[tag] = state.DepUnknown
		default:
			health[tag] = state.DepUnknown
			if noteErr == nil {
				noteErr = err
			}
		}
	}
	return health, noteErr
}

// dependencyHealth is the first step of TunnelTick: it checks the
// balancers of the dependencies of every enabled source with dependsOn
// (outside the target lock) and applies the result to their waits.
func (d Deps) dependencyHealth(ctx context.Context, sub config.SubscriptionConfig) {
	srcs := dependents(sub)
	if len(srcs) == 0 {
		return
	}
	t := sub.Target
	tags := balancerTags(sub, srcs)
	var (
		health  map[string]string
		noteErr error
	)
	client, err := d.Dial(t.Address, time.Duration(t.TimeoutMs)*time.Millisecond)
	if err != nil {
		noteErr = err
	} else {
		health, noteErr = checkBalancers(ctx, client, tags)
		_ = client.Close()
	}
	if ctx.Err() != nil {
		return
	}
	state.NoteXray(ctx, d.Memory, d.Log, sub.ID, t.Address, noteErr, d.now())
	d.applyWaits(ctx, sub, srcs, health, false)
}

// waitEvent is a wait transition of one source, logged after the update.
type waitEvent struct {
	src       config.SourceConfig
	start     bool
	deps      []string
	waitedFor time.Duration
	reset     int
	released  int
}

// applyWaits applies the dependency health (by balancer tag) to the waits
// of srcs. While the Xray API of the target is unavailable nothing changes
// (the sources count as waiting, state.SourceWaiting) and nothing is
// logged. A source with an unhealthy or unknown dependency starts waiting:
// source_dependency_unhealthy (warn), and the quarantines its replacements
// started within the longest recoverGraceSeconds of its dependencies are
// released. A dependency that becomes healthy during a wait starts its
// pause (recoverGraceSeconds); when every dependency is healthy and every
// pause is over the wait ends: the FailuresInRow of the source's tunnels are
// reset (ResetAt) and source_dependency_recovered is logged. locked tells
// that the caller holds the target lock (ReplaceFailed); otherwise it is
// taken for the release only, since sync writes the selection state back
// under it.
func (d Deps) applyWaits(ctx context.Context, sub config.SubscriptionConfig, srcs []config.SourceConfig, health map[string]string, locked bool) {
	key := state.Key(sub.ID, sub.Target.Address)
	now := d.now()
	var events []waitEvent
	d.Memory.Update(key, func(tg *state.Target) {
		for _, src := range srcs {
			tg.EnsureWait(src.ID) // SourceWaiting: a wait entry waits while Xray is down
		}
		if tg.XrayDown != nil {
			return
		}
		for _, src := range srcs {
			w := tg.EnsureWait(src.ID)
			var bad []string
			inGrace := false
			for _, dep := range src.DependsOn {
				dh := w.Deps[dep.Source]
				if dh == nil {
					dh = &state.DepHealth{}
					w.Deps[dep.Source] = dh
				}
				st := health[dependencyBalancer(sub, dep)]
				if st == "" {
					st = state.DepUnknown
				}
				switch {
				case st != state.DepHealthy:
					dh.RecoveredAt, dh.GraceUntil = nil, nil
					bad = append(bad, dep.Source)
				case dh.State != state.DepHealthy && w.Since != nil:
					// Healthy again during a wait: the pause starts.
					dh.RecoveredAt = new(now)
					dh.GraceUntil = new(now.Add(time.Duration(dep.RecoverGraceSeconds) * time.Second))
				}
				if dh.State != st {
					dh.State, dh.Since = st, new(now)
				}
				if dh.GraceUntil != nil && now.Before(*dh.GraceUntil) {
					inGrace = true
				}
			}
			switch {
			case len(bad) > 0 && w.Since == nil:
				w.Since = new(now)
				events = append(events, waitEvent{src: src, start: true, deps: bad})
			case len(bad) == 0 && w.Since != nil && !inGrace:
				reset := 0
				for _, tn := range tg.Tunnels {
					if tn.Mapping.SourceID == src.ID && tn.Monitor.FailuresInRow > 0 {
						tn.Monitor.FailuresInRow = 0
						reset++
					}
				}
				events = append(events, waitEvent{src: src, waitedFor: now.Sub(*w.Since), reset: reset})
				w.Since, w.ResetAt = nil, new(now)
				for _, dh := range w.Deps {
					dh.RecoveredAt, dh.GraceUntil = nil, nil
				}
			}
		}
	})
	for i, e := range events {
		if !e.start {
			continue
		}
		events[i].released = d.releaseQuarantine(key, e.src, now, locked)
	}
	for _, e := range events {
		attrs := []slog.Attr{
			slog.String("subscriptionId", sub.ID),
			slog.String("sourceId", e.src.ID),
			slog.String("targetAddress", sub.Target.Address),
		}
		if e.start {
			d.Log.LogAttrs(ctx, slog.LevelWarn, "A dependency of the source is unhealthy; its tunnels are only repaired until it recovers.",
				append([]slog.Attr{slog.String("event", "source_dependency_unhealthy")}, append(attrs,
					slog.Any("dependsOn", e.deps), slog.String("health", config.HealthBalancer),
					slog.Int("quarantineReleased", e.released))...)...)
			continue
		}
		d.Log.LogAttrs(ctx, slog.LevelInfo, "Every dependency of the source is healthy again; failed checks of its tunnels are reset.",
			append([]slog.Attr{slog.String("event", "source_dependency_recovered")}, append(attrs,
				slog.String("health", config.HealthBalancer),
				slog.Int64("waitedForMs", e.waitedFor.Milliseconds()),
				slog.Int("failuresReset", e.reset))...)...)
	}
}

// releaseQuarantine removes the quarantines source src started within the
// longest recoverGraceSeconds of its dependencies before now and returns
// how many. Without locked it takes the target lock around the update.
func (d Deps) releaseQuarantine(key string, src config.SourceConfig, now time.Time, locked bool) int {
	if src.Selection == nil {
		return 0
	}
	grace := 0
	for _, dep := range src.DependsOn {
		grace = max(grace, dep.RecoverGraceSeconds)
	}
	if !locked {
		unlock := d.Memory.LockTarget(key)
		defer unlock()
	}
	n := 0
	d.Memory.Update(key, func(tg *state.Target) {
		if st := tg.Sources[src.ID]; st != nil {
			n = st.Selection.ReleaseSince(now.Add(-time.Duration(grace)*time.Second), src.Selection.DeadPairCooldownMinutes)
		}
	})
	return n
}

// freshWait is the check of ReplaceFailed for a source with dependsOn,
// under the target lock with its client: the dependencies are asked again
// (the tick may be older than the failure), and when one is not healthy the
// source starts waiting and nothing is replaced (waiting is true).
func (d Deps) freshWait(ctx context.Context, c xrayapi.Client, sub config.SubscriptionConfig, src config.SourceConfig) (waiting bool) {
	if len(src.DependsOn) == 0 {
		return false
	}
	health, noteErr := checkBalancers(ctx, c, balancerTags(sub, []config.SourceConfig{src}))
	state.NoteXray(ctx, d.Memory, d.Log, sub.ID, sub.Target.Address, noteErr, d.now())
	for _, dep := range src.DependsOn {
		if health[dependencyBalancer(sub, dep)] != state.DepHealthy {
			d.applyWaits(ctx, sub, []config.SourceConfig{src}, health, true)
			return true
		}
	}
	return false
}
