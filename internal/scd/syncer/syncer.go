// Package syncer runs one sync of scd (03 §2): it loads the enabled sources
// of the enabled subscriptions; per subscription target it first removes
// the orphans (resources in no enabled source's namespace), then, per
// source, builds the manifest, prepares the plan of every enabled kind and,
// when a manifest hash of the source changed, applies all kinds of that
// source to Xray under the target lock. A source's hashes and topology are
// saved only when every kind succeeded (D2). A failure of a source (load,
// manifest validation, an apply) fails only that source; lines beyond the
// source's portRange are skipped (port_range_exhausted); a
// failed orphan removal fails the whole target for this sync (D23).
package syncer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/a1ekseev/scd/internal/common/inputsrc"
	"github.com/a1ekseev/scd/internal/common/vless"
	"github.com/a1ekseev/scd/internal/scd/apply"
	"github.com/a1ekseev/scd/internal/scd/config"
	"github.com/a1ekseev/scd/internal/scd/manifest"
	"github.com/a1ekseev/scd/internal/scd/normalize"
	"github.com/a1ekseev/scd/internal/scd/state"
	"github.com/a1ekseev/scd/internal/scd/topology"
	"github.com/a1ekseev/scd/internal/scd/xrayapi"
)

// Syncer runs syncs of one config against a shared Memory. LoadInput and
// Now default to inputsrc.Load and time.Now; Dial is required.
type Syncer struct {
	Config    config.Loaded
	Memory    *state.Memory
	Log       *slog.Logger
	Dial      xrayapi.Dialer
	Now       func() time.Time
	LoadInput func(ctx context.Context, input string, o inputsrc.Options) (inputsrc.Loaded, error)

	// Edge-triggered logging (sourceKey keys). Only perform touches these
	// and Run never overlaps it (Memory.TryStartSync), so no lock.
	lastFetch  map[string]failure // the current fetch failure per source
	lastOrphan map[string]failure // the current orphan removal failure per subscription id
	lastApply  map[string]string  // the last logged apply_failed text per source
	lastWarn   map[string]string  // the warning fingerprint per source and kind of warning
	// blocked holds the sources whose xray_state_restore_blocked was
	// logged at warn in the current StateLost episode of the source.
	blocked map[string]bool
	// applyLogged holds the sources whose apply_failed was logged in this
	// sync; the other kinds of the source are debug.
	applyLogged map[string]bool
	// startedAt is the start of this sync and drifted holds the sources
	// that logged xray_state_drift in it (StateLost marks this sync covers).
	startedAt time.Time
	drifted   map[string]bool
}

// failure is the current failure of an edge-triggered event: its text and
// when the series started.
type failure struct {
	msg   string
	since time.Time
	// logged reports that the series was logged above debug; its recovery
	// is info only then.
	logged bool
}

// sourceKey keys the edge-triggered state of a source.
func sourceKey(subscriptionID, sourceID string) string {
	return subscriptionID + "\x00" + sourceID
}

// edgeLevel returns level when the text of key changed in last (then
// stored), else slog.LevelDebug.
func edgeLevel(last map[string]string, key, text string, level slog.Level) slog.Level {
	if prev, ok := last[key]; ok && prev == text {
		return slog.LevelDebug
	}
	last[key] = text
	return level
}

func (s *Syncer) now() time.Time {
	if s.Now == nil {
		return time.Now()
	}
	return s.Now()
}

// Run performs one sync. An overlapping call (another Run of the same Memory
// in progress) logs sync_skipped_overlap and returns an empty report. The
// load and plan steps use ctx; the Xray apply of a target runs under
// context.WithoutCancel(ctx) with the per-call timeouts of the client, so a
// cancellation never stops an apply halfway (Review Focus 1). After a
// cancellation no further source is loaded and no further target or source
// is applied; those are left out of the report and Report.Canceled names the
// phase. The syncer does not log the cancellation: `scd sync` does (warn),
// the daemon stays quiet. Every
// subscription or target failure is part of the report, so the error is
// reserved for failures of the whole run (none exist at present).
func (s *Syncer) Run(ctx context.Context) (Report, error) {
	if !s.Memory.TryStartSync() {
		s.Log.LogAttrs(ctx, slog.LevelWarn, "Sync is already running in this process; skipping overlap run.",
			slog.String("event", "sync_skipped_overlap"))
		return emptyReport(s.now()), nil
	}
	defer s.Memory.EndSync()
	return s.perform(ctx), nil
}

// kinds returns the enabled kinds in apply order.
func (s *Syncer) kinds() []apply.Kind {
	r := s.Config.Config.Resources
	var out []apply.Kind
	for _, k := range apply.Order {
		switch {
		case k == apply.KindOutbound && r.Outbounds.Enabled,
			k == apply.KindInbound && r.Inbounds.Enabled,
			k == apply.KindRouting && r.Routing.Enabled:
			out = append(out, k)
		}
	}
	return out
}

type fetchedLog struct {
	ID            string `json:"id"`
	SourceID      string `json:"sourceId"`
	Source        string `json:"source"`
	Encoding      string `json:"encoding"`
	TargetAddress string `json:"targetAddress"`
}

// subscriptionLoad is the load result of one enabled subscription.
type subscriptionLoad struct {
	sub    config.SubscriptionConfig
	loaded []manifest.LoadedSource
	failed []manifest.FailedSource
}

func (s *Syncer) perform(ctx context.Context) Report {
	startedAt := s.now()
	if s.lastFetch == nil {
		s.lastFetch, s.lastApply, s.lastWarn = map[string]failure{}, map[string]string{}, map[string]string{}
		s.lastOrphan, s.blocked = map[string]failure{}, map[string]bool{}
	}
	s.applyLogged, s.drifted, s.startedAt = map[string]bool{}, map[string]bool{}, startedAt
	subs := s.Config.Config.Subscriptions
	enabledSources := 0
	for _, sub := range subs {
		if sub.Enabled {
			enabledSources += len(sub.EnabledSources())
		}
	}
	s.Log.LogAttrs(ctx, slog.LevelDebug, "Sync started.",
		slog.String("event", "sync_started"), slog.Int("sourceCount", len(subs)),
		slog.Int("enabledSourceCount", enabledSources))

	load := s.LoadInput
	if load == nil {
		load = inputsrc.Load
	}
	var enabled []config.SubscriptionConfig
	for _, sub := range subs {
		if sub.Enabled {
			enabled = append(enabled, sub)
		}
	}
	slices.SortStableFunc(enabled, func(a, b config.SubscriptionConfig) int { return strings.Compare(a.ID, b.ID) })
	var loads []subscriptionLoad
	for _, sub := range enabled {
		if ctx.Err() != nil {
			break
		}
		l, f := manifest.LoadSources(ctx, sub, load)
		loads = append(loads, subscriptionLoad{sub: sub, loaded: l, failed: f})
	}

	fetched := []fetchedLog{}
	for _, sl := range loads {
		for _, l := range sl.loaded {
			fetched = append(fetched, fetchedLog{
				ID: l.Subscription.ID, SourceID: l.Config.ID, Source: inputsrc.RedactInput(l.Source),
				Encoding: l.Encoding, TargetAddress: l.Subscription.Target.Address,
			})
			s.fetchRecovered(ctx, l)
		}
	}
	s.Log.LogAttrs(ctx, slog.LevelDebug, "Subscriptions loaded.",
		slog.String("event", "subscription_fetched"), slog.Any("subscriptions", fetched))

	var (
		targets []TargetReport
		skipped = []vless.Skip{}
	)
	for _, sl := range loads {
		for _, f := range sl.failed {
			targets = append(targets, s.failedLoad(ctx, f))
		}
	}
	canceled := ""
	if ctx.Err() != nil {
		canceled = PhaseLoad
	}
	for _, sl := range loads {
		if canceled != "" {
			break
		}
		r, sk, ok := s.syncSubscription(ctx, sl)
		targets = append(targets, r...)
		skipped = append(skipped, sk...)
		if !ok {
			canceled = PhaseTargets
		}
	}
	rep := aggregate(targets, skipped, startedAt, s.now())
	rep.Canceled = canceled
	s.logFinished(ctx, rep)
	return rep
}

// logFinished logs sync_finished: the summary of every sync. A source is
// applied when its apply changed Xray (appliedSourceIds,
// "<subscriptionId>/<sourceId>" in sync order, omitted when empty).
func (s *Syncer) logFinished(ctx context.Context, rep Report) {
	var unchanged, failed int
	applied := []string{}
	for _, t := range rep.Targets {
		changes := 0
		for _, r := range t.Resources {
			changes += r.Added + r.Replaced + r.Removed
		}
		switch {
		case t.Failed > 0:
			failed++
		case changes > 0:
			applied = append(applied, t.SubscriptionID+"/"+t.SourceID)
		default:
			unchanged++
		}
	}
	attrs := []slog.Attr{
		slog.String("event", "sync_finished"),
		slog.Int64("durationMs", rep.DurationMs),
		slog.Int("sourcesApplied", len(applied)),
		slog.Int("sourcesUnchanged", unchanged),
		slog.Int("sourcesFailed", failed),
		slog.Int("resourcesAdded", rep.Added),
		slog.Int("resourcesReplaced", rep.Replaced),
		slog.Int("resourcesRemoved", rep.Removed),
		slog.Int("skippedLines", len(rep.Skipped)),
	}
	if len(applied) > 0 {
		attrs = append(attrs, slog.Any("appliedSourceIds", applied))
	}
	if rep.Canceled != "" {
		attrs = append(attrs, slog.String("canceled", rep.Canceled))
	}
	s.Log.LogAttrs(ctx, slog.LevelInfo, "Sync finished.", attrs...)
}

// fetchRecovered logs subscription_fetch_recovered for a loaded source
// whose last load failed.
func (s *Syncer) fetchRecovered(ctx context.Context, l manifest.LoadedSource) {
	key := sourceKey(l.Subscription.ID, l.Config.ID)
	f, ok := s.lastFetch[key]
	if !ok {
		return
	}
	delete(s.lastFetch, key)
	s.Log.LogAttrs(ctx, slog.LevelInfo, "Subscription fetch recovered.",
		slog.String("event", "subscription_fetch_recovered"),
		slog.String("subscriptionId", l.Subscription.ID),
		slog.String("sourceId", l.Config.ID),
		slog.String("targetAddress", l.Subscription.Target.Address),
		slog.Int64("failedForMs", s.now().Sub(f.since).Milliseconds()))
}

// failedLoad reports a source whose input could not be loaded: one failed
// report per enabled kind. The input URL is redacted (D33). The error is
// logged when its text is new for the source, else at debug.
func (s *Syncer) failedLoad(ctx context.Context, f manifest.FailedSource) TargetReport {
	c, src := f.Subscription, f.Config
	msg := inputsrc.RedactInputIn(f.Error, src.Input)
	attrs := []slog.Attr{
		slog.String("event", "subscription_fetch_failed"),
		slog.String("subscriptionId", c.ID),
		slog.String("sourceId", src.ID),
		slog.String("source", inputsrc.RedactInput(src.Input)),
		slog.String("error", msg),
		slog.String("targetAddress", c.Target.Address),
	}
	if f.Code != "" {
		attrs = append(attrs, slog.String("reasonCode", f.Code))
	}
	key := sourceKey(c.ID, src.ID)
	level := slog.LevelError
	prev, ok := s.lastFetch[key]
	switch {
	case ok && prev.msg == msg:
		level = slog.LevelDebug
	case ok:
		prev.msg = msg
	default:
		prev = failure{msg: msg, since: s.now()}
	}
	s.lastFetch[key] = prev
	s.Log.LogAttrs(ctx, level, "Subscription fetch failed.", attrs...)
	return newTargetReport(c.ID, c.Target.Address, src.ID, nil, s.failAll(ctx, c, src.ID, nil, msg, false), nil)
}

// failAll returns a FailedReport for every enabled kind. With log they go
// through logApply (one apply_failed line for the source); without, the
// caller has logged the failure under its own event.
func (s *Syncer) failAll(ctx context.Context, c config.SubscriptionConfig, sourceID string, skipped []vless.Skip, msg string, log bool) []apply.Report {
	reports := []apply.Report{}
	for _, k := range s.kinds() {
		r := apply.FailedReport(k, s.meta(c, sourceID), skipped, errors.New(msg))
		if log {
			s.logApply(ctx, r)
		}
		reports = append(reports, r)
	}
	return reports
}

// dial connects to the target of c for mutations: AddOutbound of a source
// with maxActive goes through apply.Capped.
func (s *Syncer) dial(c config.SubscriptionConfig) (xrayapi.Client, error) {
	client, err := s.Dial(c.Target.Address, time.Duration(c.Target.TimeoutMs)*time.Millisecond)
	if err != nil {
		return nil, err
	}
	return apply.Capped(client, c.Target, c.Sources), nil
}

func (s *Syncer) meta(c config.SubscriptionConfig, sourceID string) apply.Meta {
	return apply.Meta{SourceID: sourceID, SubscriptionID: c.ID, TargetAddress: c.Target.Address, Now: s.Now}
}

// syncSubscription removes the orphans of the subscription's target and
// then syncs every loaded source, in id order. It returns one report per
// source and the manifests' skipped lines; ok is false when ctx was
// cancelled before an apply started, and the remaining sources are then
// left out. A failed orphan removal fails every loaded source without an
// apply.
func (s *Syncer) syncSubscription(ctx context.Context, sl subscriptionLoad) (reports []TargetReport, skipped []vless.Skip, ok bool) {
	c := sl.sub
	s.logRestoreBlocked(ctx, sl)
	ok, err := s.removeOrphans(ctx, c)
	if !ok {
		return nil, nil, false
	}
	if err != nil {
		for _, l := range sl.loaded {
			reports = append(reports, newTargetReport(c.ID, c.Target.Address, l.Config.ID, nil,
				s.failAll(ctx, c, l.Config.ID, nil, fmt.Errorf("orphan removal: %w", err).Error(), false), nil))
		}
		return reports, nil, true
	}
	var applied []string
	for _, l := range sl.loaded {
		r, sk, ok := s.syncSource(ctx, l)
		if !ok {
			return reports, skipped, false
		}
		reports = append(reports, r)
		skipped = append(skipped, sk...)
		if r.Failed == 0 {
			applied = append(applied, l.Config.ID)
		}
	}
	s.stateRestored(ctx, c, applied)
	return reports, skipped, true
}

// logRestoreBlocked logs xray_state_restore_blocked for a source that lost
// its resources in Xray (StateLost) and failed to load in this sync: only
// its load brings its tunnels back. Warn once per source and episode, then
// debug; the episode ends with the source's mark (xray_state_restored).
func (s *Syncer) logRestoreBlocked(ctx context.Context, sl subscriptionLoad) {
	c := sl.sub
	var lost map[string]bool
	s.Memory.View(state.Key(c.ID, c.Target.Address), func(tg *state.Target) {
		lost = map[string]bool{}
		for id := range tg.StateLost {
			lost[id] = true
		}
	})
	for _, src := range c.Sources {
		if !lost[src.ID] {
			delete(s.blocked, sourceKey(c.ID, src.ID))
		}
	}
	for _, f := range sl.failed {
		if !lost[f.Config.ID] {
			continue
		}
		key := sourceKey(c.ID, f.Config.ID)
		level := slog.LevelDebug
		if !s.blocked[key] {
			s.blocked[key], level = true, slog.LevelWarn
		}
		s.Log.LogAttrs(ctx, level, "Xray lost the source's resources; they return after the source loads again.",
			slog.String("event", "xray_state_restore_blocked"),
			slog.String("subscriptionId", c.ID),
			slog.String("sourceId", f.Config.ID),
			slog.String("targetAddress", c.Target.Address),
			slog.String("error", inputsrc.RedactInputIn(f.Error, f.Config.Input)))
	}
}

// stateRestored unmarks the lost Xray state of the sources this sync loaded
// and applied: a mark from before the sync (the monitor had reset the
// source hashes, so it was applied in full) or from this sync's drift
// check. xray_state_restored is logged when no source of the target is
// marked any more.
func (s *Syncer) stateRestored(ctx context.Context, c config.SubscriptionConfig, applied []string) {
	var (
		lostFor  time.Duration
		restored bool
	)
	now := s.now()
	for _, id := range applied {
		delete(s.blocked, sourceKey(c.ID, id)) // loaded: a later block is a new episode
	}
	s.Memory.Update(state.Key(c.ID, c.Target.Address), func(tg *state.Target) {
		for _, id := range applied {
			before := s.startedAt
			if s.drifted[sourceKey(c.ID, id)] {
				before = now
			}
			if d, ok := tg.ClearStateLost(id, before, now); ok {
				lostFor, restored = d, true
			}
		}
	})
	if restored {
		s.Log.LogAttrs(ctx, slog.LevelInfo, "Xray state of the target restored by sync.",
			slog.String("event", "xray_state_restored"),
			slog.String("subscriptionId", c.ID),
			slog.String("targetAddress", c.Target.Address),
			slog.Int64("lostForMs", lostFor.Milliseconds()))
	}
}

// logDrift logs xray_state_drift for source sourceID: warn for the first
// source of the target's episode (StateLost), debug for the others.
func (s *Syncer) logDrift(ctx context.Context, c config.SubscriptionConfig, sourceID string, missing map[apply.Kind]int) {
	first := false
	now := s.now()
	s.drifted[sourceKey(c.ID, sourceID)] = true
	s.Memory.Update(state.Key(c.ID, c.Target.Address), func(tg *state.Target) { first = tg.SetStateLost(sourceID, now) })
	level := slog.LevelDebug
	if first {
		level = slog.LevelWarn
	}
	s.Log.LogAttrs(ctx, level, "Xray lost managed resources; applying again.",
		slog.String("event", "xray_state_drift"),
		slog.String("subscriptionId", c.ID),
		slog.String("sourceId", sourceID),
		slog.String("targetAddress", c.Target.Address),
		slog.Any("missing", missing))
}

// removeOrphans removes, under the target lock, the resources of the
// enabled kinds that lie in no enabled source's namespace and are not fixed
// (orphans_removed), and forgets the state of sources that are no longer
// enabled. A dial, list or remove error is logged (orphan_remove_failed,
// at debug when the subscription failed so on the last sync too or the
// Xray API is unavailable: xray_unavailable) and returned; the first
// success after it logs orphan_remove_recovered (debug next to
// xray_recovered or after a failure logged only at debug). ok is false when ctx was cancelled before the removal
// started.
func (s *Syncer) removeOrphans(ctx context.Context, c config.SubscriptionConfig) (ok bool, err error) {
	kinds := s.kinds()
	if len(kinds) == 0 {
		return true, nil
	}
	t := c.Target
	key := state.Key(c.ID, t.Address)
	unlock := s.Memory.LockTarget(key)
	defer unlock()
	if ctx.Err() != nil {
		return false, nil
	}
	actx := context.WithoutCancel(ctx)

	enabled := map[string]bool{}
	var namespaces []apply.Namespace
	for _, src := range c.EnabledSources() {
		enabled[src.ID] = true
		namespaces = append(namespaces, apply.Namespace{ObsPrefix: t.ObservatorySubjectSelectorPrefix, SourceID: src.ID})
	}
	var removed []apply.Orphan
	client, err := s.Dial(t.Address, time.Duration(t.TimeoutMs)*time.Millisecond)
	if err == nil {
		removed, err = apply.RemoveOrphans(actx, client, t, kinds, namespaces)
		_ = client.Close()
	}
	if len(removed) > 0 {
		s.Log.LogAttrs(ctx, slog.LevelInfo, "Orphaned resources removed.",
			slog.String("event", "orphans_removed"),
			slog.String("subscriptionId", c.ID),
			slog.String("targetAddress", t.Address),
			slog.Any("orphans", removed))
	}
	prev, failed := s.lastOrphan[c.ID]
	down, recovered := state.NoteXray(ctx, s.Memory, s.Log, c.ID, t.Address, err, s.now())
	if err != nil {
		level := slog.LevelError
		if down {
			level = slog.LevelDebug // xray_unavailable covers it
		}
		switch {
		case failed && prev.msg == err.Error():
			level = slog.LevelDebug // the same failure as the last sync
		case failed:
			prev.msg = err.Error()
		default:
			prev = failure{msg: err.Error(), since: s.now()}
		}
		prev.logged = prev.logged || level != slog.LevelDebug
		s.lastOrphan[c.ID] = prev
		s.Log.LogAttrs(ctx, level, "Orphan removal failed; the target is not applied on this sync.",
			slog.String("event", "orphan_remove_failed"),
			slog.String("subscriptionId", c.ID),
			slog.String("targetAddress", t.Address),
			slog.String("error", err.Error()))
		return true, err
	}
	if failed {
		delete(s.lastOrphan, c.ID)
		level := slog.LevelInfo
		if recovered || !prev.logged {
			level = slog.LevelDebug // xray_recovered covers it, or the failure was debug
		}
		s.Log.LogAttrs(ctx, level, "Orphan removal recovered.",
			slog.String("event", "orphan_remove_recovered"),
			slog.String("subscriptionId", c.ID),
			slog.String("targetAddress", t.Address),
			slog.Int64("failedForMs", s.now().Sub(prev.since).Milliseconds()))
	}
	var stale []string
	s.Memory.View(key, func(tg *state.Target) {
		for _, id := range tg.SourceIDs() {
			if !enabled[id] {
				stale = append(stale, id)
			}
		}
	})
	for _, id := range stale {
		s.Memory.DropSource(key, id)
	}
	return true, nil
}

// syncSource builds the manifest and plans of a loaded source and syncs
// them to its target. It returns the source report and the manifest's
// skipped lines for the run report; ok is false when ctx was cancelled
// before the apply started, and the source is then left out.
func (s *Syncer) syncSource(ctx context.Context, l manifest.LoadedSource) (r TargetReport, skipped []vless.Skip, ok bool) {
	if ctx.Err() != nil {
		return TargetReport{}, nil, false
	}
	c, src := l.Subscription, l.Config
	t := c.Target
	m, err := manifest.FromSource(l, s.now())
	if err != nil {
		// The manifest is ignored when it is invalid.
		s.Log.LogAttrs(ctx, slog.LevelError, "Manifest validation failed.",
			slog.String("event", "manifest_validation_failed"),
			slog.String("subscriptionId", c.ID),
			slog.String("sourceId", src.ID),
			slog.String("error", err.Error()),
			slog.String("targetAddress", t.Address))
		return newTargetReport(c.ID, t.Address, src.ID, nil, s.failAll(ctx, c, src.ID, nil, err.Error(), false), nil), nil, true
	}
	s.logEntryWarnings(ctx, c, src.ID, m)

	kinds := s.kinds()
	plans := make([]apply.Plan, 0, len(kinds))
	for _, k := range kinds {
		p, err := apply.PlanForSource(k, m, src, t)
		if err != nil {
			return newTargetReport(c.ID, t.Address, src.ID, nil, s.failAll(ctx, c, src.ID, nil, err.Error(), true), m.Skipped), m.Skipped, true
		}
		plans = append(plans, p)
	}
	skipped = m.Skipped
	var warnings []normalize.Warning
	if len(plans) > 0 {
		// Every plan carries the same skipped lines, port_range_exhausted included.
		skipped = plans[0].Skipped
		warnings = s.logPortRangeSkips(ctx, c, src.ID, skipped)
	}
	for _, p := range plans {
		s.Log.LogAttrs(ctx, slog.LevelDebug, "Manifest built.",
			slog.String("event", "manifest_built"),
			slog.String("kind", string(p.Kind)),
			slog.String("subscriptionId", c.ID),
			slog.String("sourceId", p.SourceID),
			slog.Int("managedCount", len(p.ManagedIDs)),
			slog.Int("skipped", len(p.Skipped)),
			slog.Int("filtered", m.Summary.Filtered),
			slog.Int("filteredByCountry", m.Summary.FilteredByCountry),
			slog.Int("filteredByLabelRegex", m.Summary.FilteredByLabelRegex),
			slog.String("targetAddress", t.Address))
	}
	if config.CandidateType(src.Type) {
		r, ok = s.syncWireGuard(ctx, c, src, m, skipped)
	} else {
		r, ok = s.syncPlans(ctx, c, src.ID, plans, skipped)
	}
	if ok && r.Failed == 0 {
		delete(s.lastApply, sourceKey(c.ID, src.ID)) // the next failure is logged again
		// Set also when the hashes are unchanged: the lines beyond the range
		// may change without changing the applied ones.
		s.Memory.Update(state.Key(c.ID, t.Address), func(tg *state.Target) {
			if st := tg.Sources[src.ID]; st != nil {
				st.Warnings = warnings
				if config.CandidateType(src.Type) {
					st.CandidateCount = len(m.Entries)
				}
			}
		})
	}
	return r, skipped, ok
}

// logPortRangeSkips logs one manifest_entry_warning for the lines left
// without a port by the source's portRange (line and label of the first,
// count) and returns one summary warning for the source state, or nil. The
// line is warn when the set of skipped lines changed since the last sync,
// else debug.
func (s *Syncer) logPortRangeSkips(ctx context.Context, c config.SubscriptionConfig, sourceID string, skipped []vless.Skip) []normalize.Warning {
	var lines []string
	var first vless.Skip
	for _, sk := range skipped {
		if sk.Code != topology.CodePortRangeExhausted {
			continue
		}
		if lines == nil {
			first = sk
		}
		lines = append(lines, strconv.Itoa(sk.Line))
	}
	level := edgeLevel(s.lastWarn, sourceKey(c.ID, sourceID)+"\x00port", strings.Join(lines, ","), slog.LevelWarn)
	n := len(lines)
	if n == 0 {
		return nil
	}
	label := ""
	if first.Label != nil {
		label = *first.Label
	}
	s.Log.LogAttrs(ctx, level, "Manifest entry warning.",
		slog.String("event", "manifest_entry_warning"),
		slog.String("subscriptionId", c.ID),
		slog.String("sourceId", sourceID),
		slog.String("targetAddress", c.Target.Address),
		slog.Int("line", first.Line),
		slog.String("label", label),
		slog.String("code", first.Code),
		slog.String("message", first.Reason),
		slog.Int("count", n))
	return []normalize.Warning{{
		Code:    topology.CodePortRangeExhausted,
		Message: fmt.Sprintf("%d lines were skipped: the source portRange has no free port for them.", n),
	}}
}

// logEntryWarnings logs the normalizer warnings of every entry:
// param_ignored at debug (D28), the others at warn while the set of (tag,
// code) of the source differs from the last sync, else at debug.
func (s *Syncer) logEntryWarnings(ctx context.Context, c config.SubscriptionConfig, sourceID string, m manifest.Manifest) {
	var fp []string
	for _, e := range m.Entries {
		for _, w := range e.Warnings {
			fp = append(fp, e.Tag+"|"+w.Code)
		}
	}
	slices.Sort(fp)
	warn := edgeLevel(s.lastWarn, sourceKey(c.ID, sourceID)+"\x00entry", strings.Join(fp, "\n"), slog.LevelWarn)
	for _, e := range m.Entries {
		for _, w := range e.Warnings {
			level := warn
			if w.Code == normalize.WarnParamIgnored {
				level = slog.LevelDebug
			}
			s.Log.LogAttrs(ctx, level, "Manifest entry warning.",
				slog.String("event", "manifest_entry_warning"),
				slog.String("subscriptionId", c.ID),
				slog.String("sourceId", sourceID),
				slog.String("targetAddress", c.Target.Address),
				slog.String("tag", e.Tag),
				slog.String("code", w.Code),
				slog.String("message", w.Message))
		}
	}
}

// targetAttrs are the common attributes of the per-kind target events;
// the observatory prefix is omitted when empty.
func targetAttrs(event string, kind apply.Kind, c config.SubscriptionConfig, sourceID string) []slog.Attr {
	attrs := []slog.Attr{
		slog.String("event", event),
		slog.String("kind", string(kind)),
		slog.String("subscriptionId", c.ID),
		slog.String("sourceId", sourceID),
		slog.String("targetAddress", c.Target.Address),
	}
	if p := c.Target.ObservatorySubjectSelectorPrefix; p != "" {
		attrs = append(attrs, slog.String("observatorySubjectSelectorPrefix", p))
	}
	return attrs
}

// syncPlans compares the plan hashes of source sourceID with the saved ones
// and, when any differs, applies every plan in order under the target lock
// (03 §2.2). ok is false when ctx was cancelled before the apply started
// (possibly while waiting for the lock): no apply is started then (final
// review I1).
func (s *Syncer) syncPlans(ctx context.Context, c config.SubscriptionConfig, sourceID string, plans []apply.Plan, skipped []vless.Skip) (r TargetReport, ok bool) {
	t := c.Target
	key := state.Key(c.ID, t.Address)
	s.Memory.MarkSynced(key)

	changed := false
	s.Memory.View(key, func(tg *state.Target) {
		src := tg.Sources[sourceID]
		for _, p := range plans {
			if src == nil {
				changed = true
				break
			}
			if h, ok := src.Hashes[p.Kind]; !ok || h != p.ManifestHash {
				changed = true
			}
		}
	})
	if !changed {
		// The hashes only say what scd applied last time; an Xray restart
		// drops every API-added resource, so check that they still exist.
		if missing := s.missingInXray(ctx, c, plans); len(missing) > 0 {
			s.logDrift(ctx, c, sourceID, missing)
			changed = true
		}
	}
	if !changed {
		unchanged := make([]apply.Kind, 0, len(plans))
		for _, p := range plans {
			unchanged = append(unchanged, p.Kind)
			s.Log.LogAttrs(ctx, slog.LevelDebug, "Manifest unchanged for resource; skipping Xray API.",
				targetAttrs("skipped_no_changes", p.Kind, c, sourceID)...)
		}
		return newTargetReport(c.ID, t.Address, sourceID, unchanged, nil, skipped), true
	}

	unlock := s.Memory.LockTarget(key)
	defer unlock()
	if ctx.Err() != nil {
		return TargetReport{}, false
	}
	client, err := s.dial(c)
	if err != nil {
		reports := make([]apply.Report, 0, len(plans))
		for _, p := range plans {
			r := apply.FailedReport(p.Kind, s.meta(c, p.SourceID), p.Skipped, err)
			s.logApply(ctx, r)
			reports = append(reports, r)
		}
		return newTargetReport(c.ID, t.Address, sourceID, nil, reports, skipped), true
	}
	defer func() { _ = client.Close() }()

	reports, hashes, failed := s.applyPlans(ctx, client, c, sourceID, plans)
	if !failed {
		// Every plan carries the same source topology.
		s.Memory.ReplaceSourceTopology(key, sourceID, *plans[len(plans)-1].Topology, hashes)
	}
	return newTargetReport(c.ID, t.Address, sourceID, nil, reports, skipped), true
}

// applyPlans applies every plan of source sourceID in order through client
// (under the target lock, which the caller holds) and logs each report. It
// returns the reports, the hashes of the plans that succeeded and whether
// any failed.
func (s *Syncer) applyPlans(ctx context.Context, client xrayapi.Client, c config.SubscriptionConfig, sourceID string, plans []apply.Plan) (reports []apply.Report, hashes map[apply.Kind]string, failed bool) {
	// The mutation sequence is not cut short by a cancellation; each RPC has
	// the client's own timeout (Review Focus 1, D7).
	actx := context.WithoutCancel(ctx)
	reports = make([]apply.Report, 0, len(plans))
	hashes = map[apply.Kind]string{}
	for _, p := range plans {
		s.Log.LogAttrs(ctx, slog.LevelDebug, "Applying resource to target.", targetAttrs("resource_apply_started", p.Kind, c, sourceID)...)
		r := apply.Apply(actx, client, p, c.Target, s.meta(c, p.SourceID))
		s.logApply(ctx, r)
		reports = append(reports, r)
		if r.Failed > 0 {
			failed = true
			continue
		}
		hashes[p.Kind] = p.ManifestHash
	}
	return reports, hashes, failed
}

// missingInXray counts, per kind, the managed resources of plans that the
// target no longer has. It reads under the target lock so that a tunnel
// repair (remove + add outbound) is never seen half done. A repaired
// outbound has no observatory prefix and still counts as present. Dial or
// list errors return nil: the sync stays unchanged, as before the check.
func (s *Syncer) missingInXray(ctx context.Context, c config.SubscriptionConfig, plans []apply.Plan) map[apply.Kind]int {
	t := c.Target
	unlock := s.Memory.LockTarget(state.Key(c.ID, t.Address))
	defer unlock()
	if ctx.Err() != nil {
		return nil
	}
	client, err := s.Dial(t.Address, time.Duration(t.TimeoutMs)*time.Millisecond)
	if err != nil {
		return nil
	}
	defer func() { _ = client.Close() }()
	return missingIn(ctx, client, t, plans)
}

// missingIn is missingInXray through client, without the lock.
func missingIn(ctx context.Context, client xrayapi.Client, t config.SubscriptionTargetConfig, plans []apply.Plan) map[apply.Kind]int {
	missing := map[apply.Kind]int{}
	for _, p := range plans {
		have := map[string]bool{}
		switch p.Kind {
		case apply.KindOutbound:
			list, err := client.ListOutbounds(ctx)
			if err != nil {
				return nil
			}
			for _, o := range list {
				have[o.GetTag()] = true
			}
		case apply.KindInbound:
			list, err := client.ListInbounds(ctx)
			if err != nil {
				return nil
			}
			for _, in := range list {
				have[in.GetTag()] = true
			}
		case apply.KindRouting:
			list, err := client.ListRules(ctx)
			if err != nil {
				return nil
			}
			for _, r := range list {
				have[r.RuleTag] = true
			}
		}
		for _, id := range p.ManagedIDs {
			base := strings.TrimPrefix(id, t.ObservatorySubjectSelectorPrefix)
			if !have[id] && !have[base] {
				missing[p.Kind]++
			}
		}
	}
	if len(missing) == 0 {
		return nil
	}
	return missing
}

// logApply logs apply_finished (debug), or
// apply_failed with the failed items and their messages: one error line per
// source and sync, and only when its text differs from the last one logged
// for the source; the other kinds and repeats are debug.
func (s *Syncer) logApply(ctx context.Context, r apply.Report) {
	attrs := []slog.Attr{
		slog.String("event", "apply_finished"),
		slog.String("kind", string(r.Kind)),
		slog.String("subscriptionId", r.SubscriptionID),
		slog.String("targetAddress", r.TargetAddress),
		slog.String("sourceId", r.SourceID),
		slog.Int("added", r.Added),
		slog.Int("replaced", r.Replaced),
		slog.Int("removed", r.Removed),
		slog.Int("failed", r.Failed),
		slog.Any("deletedIds", r.DeletedIDs),
		slog.Any("appliedIds", r.AppliedIDs),
		slog.Int64("durationMs", r.DurationMs),
	}
	if r.Failed == 0 {
		// sync_finished names the applied sources.
		s.Log.LogAttrs(ctx, slog.LevelDebug, "Apply finished.", attrs...)
		return
	}
	attrs[0] = slog.String("event", "apply_failed")
	failedItems := []apply.Item{}
	messages := []string{}
	for _, it := range r.Items {
		if it.Status != apply.StatusFailed {
			continue
		}
		failedItems = append(failedItems, it)
		if it.Message != "" {
			messages = append(messages, it.Message)
		}
	}
	attrs = append(attrs, slog.Any("errorMessages", messages), slog.Any("failedItems", failedItems))
	key := sourceKey(r.SubscriptionID, r.SourceID)
	level := slog.LevelDebug
	if !s.applyLogged[key] {
		s.applyLogged[key] = true
		level = edgeLevel(s.lastApply, key, strings.Join(messages, "\n"), slog.LevelError)
	}
	s.Log.LogAttrs(ctx, level, "Apply failed.", attrs...)
}
