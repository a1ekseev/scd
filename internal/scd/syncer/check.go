package syncer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/a1ekseev/scd/internal/scd/apply"
	"github.com/a1ekseev/scd/internal/scd/config"
	"github.com/a1ekseev/scd/internal/scd/manifest"
	"github.com/a1ekseev/scd/internal/scd/state"
	"github.com/a1ekseev/scd/internal/scd/xrayapi"
)

// Reason codes of source_check_failed.
const (
	// CodeDependencyNotApplied: a dependsOn source has never been applied
	// to the target (no state), so the source would have nothing to dial
	// through.
	CodeDependencyNotApplied = "dependency_not_applied"
	// CodeBalancerNotFound: Xray has no balancer with the source's balancer
	// tag.
	CodeBalancerNotFound = "balancer_not_found"
	// CodeRoutingServiceDisabled: the balancer cannot be checked, Xray's
	// api.services has no RoutingService.
	CodeRoutingServiceDisabled = "routing_service_disabled"
	// CodeDialerProxyMissing: the source's dialerProxy is not an outbound of
	// the target.
	CodeDialerProxyMissing = "dialer_proxy_missing"
)

// dependencyOrder returns loaded in dependsOn order: a source after every
// loaded source it depends on, ties in id order (Kahn's algorithm over the
// id-ordered list). A dependency that is not loaded in this sync puts no
// constraint on the order. The config has no cycle; were there one, its
// sources would follow the others in id order.
func dependencyOrder(loaded []manifest.LoadedSource) []manifest.LoadedSource {
	sorted := slices.SortedStableFunc(slices.Values(loaded), func(a, b manifest.LoadedSource) int {
		return strings.Compare(a.Config.ID, b.Config.ID)
	})
	pending := map[string]bool{}
	for _, l := range sorted {
		pending[l.Config.ID] = true
	}
	out := make([]manifest.LoadedSource, 0, len(sorted))
	for len(out) < len(sorted) {
		next := -1
		for i, l := range sorted {
			if pending[l.Config.ID] && !slices.ContainsFunc(l.Config.DependsOn, func(d config.DependencyConfig) bool { return pending[d.Source] }) {
				next = i
				break
			}
		}
		if next < 0 { // a cycle: the rest in id order
			for _, l := range sorted {
				if pending[l.Config.ID] {
					out = append(out, l)
				}
			}
			break
		}
		pending[sorted[next].Config.ID] = false
		out = append(out, sorted[next])
	}
	return out
}

// checkFailure is a failed check of a source before its apply.
type checkFailure struct {
	code, msg string
}

// text is the text of the failure in the report: the code first.
func (f checkFailure) text() string { return f.code + ": " + f.msg }

// checkSource checks a loaded source before anything of it is applied, on
// every sync (unchanged sources too), in this order:
//   - every dependsOn source has state on the target (applied in this or an
//     earlier sync, an empty topology included): dependency_not_applied;
//   - its balancer exists in Xray (read-only GetBalancerInfo):
//     balancer_not_found, or routing_service_disabled without
//     RoutingService; both are answers of the API, so they end an
//     xray_unavailable episode like a success;
//   - its dialerProxy is an outbound of the target (the list of this sync's
//     orphan removal): dialer_proxy_missing.
//
// A failed check is logged as source_check_failed by edge (a new code or
// text is error, a repeat debug) and source_check_recovered ends it. Any
// other error (Xray unreachable, no outbound list) fails the source like
// an apply error (apply_failed, state.NoteXray). ok is false when the
// source must not be applied in this sync; r is then its report, except
// after a cancellation of ctx: nothing is logged or reported then, like a
// cancelled orphan removal, and the caller stops.
func (s *Syncer) checkSource(ctx context.Context, l manifest.LoadedSource) (r TargetReport, ok bool) {
	c, src := l.Subscription, l.Config
	f, err := s.runChecks(ctx, l)
	if ctx.Err() != nil {
		return TargetReport{}, false
	}
	if err != nil {
		s.noteError(c, src.ID, CodeApplyFailed, err.Error())
		return newTargetReport(c.ID, c.Target.Address, src.ID, nil, s.failAll(ctx, c, src.ID, nil, err.Error(), true), nil), false
	}
	key := sourceKey(c.ID, src.ID)
	// The monitor treats a source with a failed check as waiting.
	s.Memory.Update(state.Key(c.ID, c.Target.Address), func(tg *state.Target) {
		if f.code == "" {
			delete(tg.CheckFailed, src.ID)
			return
		}
		if tg.CheckFailed == nil {
			tg.CheckFailed = map[string]string{}
		}
		tg.CheckFailed[src.ID] = f.code
	})
	if f.code == "" {
		if prev, failed := s.lastCheck[key]; failed {
			delete(s.lastCheck, key)
			s.Log.LogAttrs(ctx, slog.LevelInfo, "Source check recovered.",
				slog.String("event", "source_check_recovered"),
				slog.String("subscriptionId", c.ID),
				slog.String("sourceId", src.ID),
				slog.String("targetAddress", c.Target.Address),
				slog.Int64("failedForMs", s.now().Sub(prev.since).Milliseconds()))
		}
		return TargetReport{}, true
	}
	level := slog.LevelError
	prev, failed := s.lastCheck[key]
	switch {
	case failed && prev.msg == f.text():
		level = slog.LevelDebug
	case failed:
		prev.msg = f.text()
	default:
		prev = failure{msg: f.text(), since: s.now()}
	}
	s.lastCheck[key] = prev
	s.Log.LogAttrs(ctx, level, "Source check failed; the source is not applied.",
		slog.String("event", "source_check_failed"),
		slog.String("subscriptionId", c.ID),
		slog.String("sourceId", src.ID),
		slog.String("targetAddress", c.Target.Address),
		slog.String("reasonCode", f.code),
		slog.String("error", f.msg))
	s.noteError(c, src.ID, f.code, f.msg)
	return newTargetReport(c.ID, c.Target.Address, src.ID, nil, s.failAll(ctx, c, src.ID, nil, f.text(), false), nil), false
}

// Codes of a source error in state (status API) besides the check codes.
const (
	CodeFetchFailed        = "fetch_failed"
	CodeManifestInvalid    = "manifest_invalid"
	CodeApplyFailed        = "apply_failed"
	CodeOrphanRemoveFailed = "orphan_remove_failed"
)

// noteError records the failure of source id of c in state for the status
// API (state.Target.SetSourceError), the first one of this sync only.
func (s *Syncer) noteError(c config.SubscriptionConfig, id, code, msg string) {
	key := sourceKey(c.ID, id)
	if s.errNoted[key] {
		return
	}
	s.errNoted[key] = true
	k := state.Key(c.ID, c.Target.Address)
	s.Memory.Ensure(k)
	now := s.now()
	s.Memory.Update(k, func(tg *state.Target) { tg.SetSourceError(id, code, msg, now) })
}

// clearError forgets the failure of source id of c after a successful sync
// of it.
func (s *Syncer) clearError(c config.SubscriptionConfig, id string) {
	s.Memory.Update(state.Key(c.ID, c.Target.Address), func(tg *state.Target) { delete(tg.SourceErrors, id) })
}

// firstFailure is the message of the first failed item of r.
func firstFailure(r TargetReport) string {
	for _, k := range r.Resources {
		for _, it := range k.Items {
			if it.Status == apply.StatusFailed && it.Message != "" {
				return it.Message
			}
		}
	}
	return "apply failed"
}

// runChecks runs the checks of checkSource: a failed check is f, an error
// that is not a check verdict is err.
func (s *Syncer) runChecks(ctx context.Context, l manifest.LoadedSource) (f checkFailure, err error) {
	c, src := l.Subscription, l.Config
	t := c.Target
	if len(src.DependsOn) > 0 {
		missing := src.DependsOn[0].Source // without target state no dependency is applied
		s.Memory.View(state.Key(c.ID, t.Address), func(tg *state.Target) {
			missing = ""
			for _, d := range src.DependsOn {
				if tg.Sources[d.Source] == nil {
					missing = d.Source
					break
				}
			}
		})
		if missing != "" {
			return checkFailure{CodeDependencyNotApplied, fmt.Sprintf("dependency %q is not applied to the target", missing)}, nil
		}
	}
	if src.Balancer != "" {
		client, err := s.Dial(t.Address, time.Duration(t.TimeoutMs)*time.Millisecond)
		if err == nil {
			_, err = client.GetBalancerInfo(ctx, src.Balancer)
			_ = client.Close()
		}
		var (
			notFound *xrayapi.BalancerNotFoundError
			disabled *xrayapi.ServiceDisabledError
		)
		switch {
		case errors.As(err, &notFound):
			f = checkFailure{CodeBalancerNotFound, err.Error()}
		case errors.As(err, &disabled):
			f = checkFailure{CodeRoutingServiceDisabled, err.Error()}
		case ctx.Err() != nil:
			return checkFailure{}, ctx.Err()
		case err != nil:
			state.NoteXray(ctx, s.Memory, s.Log, c.ID, t.Address, err, s.now())
			return checkFailure{}, fmt.Errorf("balancer check: %w", err)
		}
		// The API answered: a verdict ends an unavailability like a success.
		state.NoteXray(ctx, s.Memory, s.Log, c.ID, t.Address, nil, s.now())
		if f.code != "" {
			return f, nil
		}
	}
	if src.DialerProxy != "" {
		list := s.outbounds[c.ID]
		if list == nil {
			return checkFailure{}, errors.New("dialerProxy check: the outbound list of the target is not available")
		}
		if !slices.Contains(list, src.DialerProxy) {
			return checkFailure{CodeDialerProxyMissing, fmt.Sprintf("dialerProxy %q is not an outbound of the target", src.DialerProxy)}, nil
		}
	}
	return checkFailure{}, nil
}
