package balancer

import (
	"context"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/a1ekseev/scd/internal/scd/config"
	"github.com/a1ekseev/scd/internal/scd/state"
	"github.com/a1ekseev/scd/internal/scd/xrayapi"
)

// Deps are the dependencies of Tick.
type Deps struct {
	Memory *state.Memory
	Log    *slog.Logger
	// Dial connects to Xray; nil means xrayapi.Dial.
	Dial xrayapi.Dialer
	// Now defaults to time.Now.
	Now func() time.Time
}

func (d Deps) now() time.Time {
	if d.Now == nil {
		return time.Now()
	}
	return d.Now()
}

func (d Deps) dial(address string, timeout time.Duration) (xrayapi.Client, error) {
	if d.Dial == nil {
		return xrayapi.Dial(address, timeout)
	}
	return d.Dial(address, timeout)
}

// logEvent is a log record produced under the state lock and written after
// it is released.
type logEvent struct {
	level   slog.Level
	msg     string
	event   string
	details []slog.Attr
}

// Tick polls the balancer selection of the target of sub (10 §6.3): it
// calls GetBalancerInfo and, with balancerSelection.observatory,
// GetOutboundStatus in parallel, without the target lock (read-only, like
// the balancer monitor), and records the result under the state lock. It
// runs before the first sync too: the state is created (Memory.Ensure),
// the tunnel mapping appears after the sync. Logs are written only on
// transitions (10 §6.4): balancer_selection_changed,
// balancer_selection_failed (new error text), balancer_selection_recovered
// and balancer_observatory_unavailable (new error text); while the Xray API
// is unavailable (xray_unavailable, cleared by a successful
// GetBalancerInfo: xray_recovered) the first two are debug, and the
// observatory line is debug when GetBalancerInfo failed too. It is a no-op when
// the selection is disabled; the error is reserved for a cancelled ctx, in
// which case nothing is recorded.
func Tick(ctx context.Context, d Deps, sub config.SubscriptionConfig) error {
	t := sub.Target
	bs := t.BalancerSelection
	if !bs.Enabled {
		return nil
	}

	var (
		info            xrayapi.BalancerInfo
		obs             []xrayapi.OutboundObservation
		infoErr, obsErr error
		obsChecked      = bs.Observatory
	)
	client, err := d.dial(t.Address, time.Duration(t.TimeoutMs)*time.Millisecond)
	if err != nil {
		// Nothing was asked: the observatory is not checked this time.
		infoErr, obsChecked = err, false
	} else {
		var wg sync.WaitGroup
		wg.Go(func() { info, infoErr = client.GetBalancerInfo(ctx, bs.BalancerTag) })
		if bs.Observatory {
			wg.Go(func() { obs, obsErr = client.GetOutboundStatus(ctx) })
		}
		wg.Wait()
		_ = client.Close()
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}

	now := d.now()
	key := state.Key(sub.ID, t.Address)
	d.Memory.Ensure(key)
	// One cause, one line: while the API is unavailable xray_unavailable
	// covers the selection and observatory errors of the same tick.
	down, recovered := state.NoteXray(ctx, d.Memory, d.Log, sub.ID, t.Address, infoErr, now)
	var events []logEvent
	d.Memory.Update(key, func(tg *state.Target) {
		events = record(&tg.BalancerSelection, bs, now, info, infoErr)
		if obsChecked || !bs.Observatory {
			events = append(events, recordObservatory(&tg.BalancerSelection, bs.Observatory, now, obs, obsErr)...)
		}
	})

	base := []slog.Attr{
		slog.String("subscriptionId", sub.ID),
		slog.String("targetAddress", t.Address),
		slog.String("balancerTag", bs.BalancerTag),
	}
	for _, e := range events {
		switch {
		case e.event == "balancer_selection_failed" && down,
			e.event == "balancer_selection_recovered" && recovered,
			e.event == "balancer_observatory_unavailable" && (infoErr != nil || xrayapi.IsUnavailable(obsErr)):
			e.level = slog.LevelDebug
		}
		attrs := append([]slog.Attr{slog.String("event", e.event)}, base...)
		d.Log.LogAttrs(ctx, e.level, e.msg, append(attrs, e.details...)...)
	}
	return nil
}

// record applies one GetBalancerInfo result to s and returns the log events
// of its transitions.
func record(s *state.SelectionState, bs config.BalancerSelectionConfig, now time.Time, info xrayapi.BalancerInfo, infoErr error) []logEvent {
	var events []logEvent
	s.BalancerTag = bs.BalancerTag
	s.LastCheckedAt = &now

	if infoErr != nil {
		msg := infoErr.Error()
		if s.State != state.SelectionError || s.LastError != msg {
			events = append(events, logEvent{
				slog.LevelWarn, "Balancer selection check failed.", "balancer_selection_failed",
				[]slog.Attr{slog.String("error", msg)},
			})
		}
		s.State = state.SelectionError
		s.LastError = msg
		s.LastFailureAt = &now
		s.ConsecutiveFailures++
	} else {
		mode, selected, _ := ResolveSelection(info)
		if s.State == state.SelectionError {
			events = append(events, logEvent{slog.LevelInfo, "Balancer selection check recovered.", "balancer_selection_recovered", nil})
		}
		if s.State == "" || selected != s.SelectedTag || mode != s.Mode {
			// Xray moves its pick on its own; only the first pick, a mode
			// change and a move to or from no pick are info.
			level := slog.LevelDebug
			if s.State == "" || mode != s.Mode || selected == "" || s.SelectedTag == "" {
				level = slog.LevelInfo
			}
			events = append(events, logEvent{
				level, "Balancer selection changed.", "balancer_selection_changed",
				[]slog.Attr{slog.String("from", s.SelectedTag), slog.String("to", selected), slog.String("mode", string(mode))},
			})
		}
		if s.State == "" || selected != s.SelectedTag {
			if s.State != "" {
				s.PreviousSelectedTag = s.SelectedTag
			}
			s.SelectedSince = &now
		}
		s.State = state.SelectionOK
		s.Mode = mode
		s.SelectedTag = selected
		s.OverrideTarget = info.OverrideTarget
		s.PrincipleTargets = slices.Clone(info.PrincipleTargets)
		s.LastSuccessAt = &now
		s.LastError = ""
		s.ConsecutiveFailures = 0
	}
	return events
}

// recordObservatory applies one GetOutboundStatus result to s (or clears
// the observatory data when it is disabled) and returns the log events.
func recordObservatory(s *state.SelectionState, enabled bool, now time.Time, obs []xrayapi.OutboundObservation, obsErr error) []logEvent {
	var events []logEvent
	switch {
	case !enabled:
		s.Observatory, s.ObservatoryError, s.ObservatoryCheckedAt = nil, "", nil
	case obsErr != nil:
		msg := obsErr.Error()
		if s.ObservatoryError != msg {
			events = append(events, logEvent{
				slog.LevelWarn, "Xray observatory is unavailable.", "balancer_observatory_unavailable",
				[]slog.Attr{slog.String("error", msg)},
			})
		}
		s.Observatory, s.ObservatoryError = nil, msg
	default:
		m := make(map[string]xrayapi.OutboundObservation, len(obs))
		for _, o := range obs {
			m[o.Tag] = o
		}
		s.Observatory, s.ObservatoryError, s.ObservatoryCheckedAt = m, "", &now
	}
	return events
}
