// Package syncer runs one sync of scd (03 §2): it loads the enabled
// subscriptions, builds each manifest once, prepares the plan of every
// enabled kind for the subscription's target and, when a manifest hash
// changed, applies all kinds to Xray under the target lock. Hashes and the
// topology are saved only when every kind succeeded (D2). Any failure of a
// subscription or target (load, manifest validation, port allocation, an
// apply) fails only that target (D23).
package syncer

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/a1ekseev/scd/internal/common/inputsrc"
	"github.com/a1ekseev/scd/internal/common/vless"
	"github.com/a1ekseev/scd/internal/scd/apply"
	"github.com/a1ekseev/scd/internal/scd/config"
	"github.com/a1ekseev/scd/internal/scd/manifest"
	"github.com/a1ekseev/scd/internal/scd/normalize"
	"github.com/a1ekseev/scd/internal/scd/state"
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
// cancellation never stops an apply halfway (Review Focus 1). Every
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
	Source        string `json:"source"`
	Encoding      string `json:"encoding"`
	TargetAddress string `json:"targetAddress"`
}

func (s *Syncer) perform(ctx context.Context) Report {
	startedAt := s.now()
	subs := s.Config.Config.Subscriptions
	s.Log.LogAttrs(ctx, slog.LevelInfo, "Sync started.",
		slog.String("event", "sync_started"), slog.Int("sourceCount", len(subs)))

	load := s.LoadInput
	if load == nil {
		load = inputsrc.Load
	}
	loaded, failed := manifest.LoadSubscriptions(ctx, subs, load)

	fetched := make([]fetchedLog, 0, len(loaded))
	for _, l := range loaded {
		fetched = append(fetched, fetchedLog{
			ID: l.Config.ID, Source: inputsrc.RedactInput(l.Source), Encoding: l.Encoding, TargetAddress: l.Config.Target.Address,
		})
	}
	s.Log.LogAttrs(ctx, slog.LevelInfo, "Subscriptions loaded.",
		slog.String("event", "subscription_fetched"), slog.Any("subscriptions", fetched))

	var (
		targets []TargetReport
		skipped = []vless.Skip{}
	)
	for _, f := range failed {
		targets = append(targets, s.failedLoad(ctx, f))
	}
	for _, l := range loaded {
		r, sk := s.syncSubscription(ctx, l)
		targets = append(targets, r)
		skipped = append(skipped, sk...)
	}
	return aggregate(targets, skipped, startedAt, s.now())
}

// failedLoad reports a subscription whose input could not be loaded: one
// failed report per enabled kind. The input URL is redacted (D33).
func (s *Syncer) failedLoad(ctx context.Context, f manifest.FailedSubscription) TargetReport {
	c := f.Config
	msg := inputsrc.RedactInputIn(f.Error, c.Input)
	s.Log.LogAttrs(ctx, slog.LevelError, "Subscription fetch failed.",
		slog.String("event", "subscription_fetch_failed"),
		slog.String("subscriptionId", c.ID),
		slog.String("source", inputsrc.RedactInput(c.Input)),
		slog.String("error", msg),
		slog.String("targetAddress", c.Target.Address))
	return newTargetReport(c.ID, c.Target.Address, c.ID, nil, s.failAll(ctx, c, c.ID, nil, msg), nil)
}

// failAll returns a FailedReport for every enabled kind and logs each.
func (s *Syncer) failAll(ctx context.Context, c config.SubscriptionConfig, sourceID string, skipped []vless.Skip, msg string) []apply.Report {
	reports := []apply.Report{}
	for _, k := range s.kinds() {
		r := apply.FailedReport(k, s.meta(c, sourceID), skipped, errors.New(msg))
		s.logApply(ctx, r)
		reports = append(reports, r)
	}
	return reports
}

func (s *Syncer) meta(c config.SubscriptionConfig, sourceID string) apply.Meta {
	return apply.Meta{SourceID: sourceID, SubscriptionID: c.ID, TargetAddress: c.Target.Address, Now: s.Now}
}

// syncSubscription builds the manifest and plans of a loaded subscription
// and syncs its target. It returns the target report and the manifest's
// skipped lines for the run report.
func (s *Syncer) syncSubscription(ctx context.Context, l manifest.LoadedSubscription) (TargetReport, []vless.Skip) {
	c := l.Config
	t := c.Target
	m, err := manifest.FromSubscription(l, s.now())
	if err != nil {
		// The manifest is ignored when it is invalid.
		s.Log.LogAttrs(ctx, slog.LevelError, "Manifest validation failed.",
			slog.String("event", "manifest_validation_failed"),
			slog.String("subscriptionId", c.ID),
			slog.String("sourceId", c.ID),
			slog.String("error", err.Error()),
			slog.String("targetAddress", t.Address))
		return newTargetReport(c.ID, t.Address, c.ID, nil, s.failAll(ctx, c, c.ID, nil, err.Error()), nil), nil
	}
	s.logEntryWarnings(ctx, c, m)

	kinds := s.kinds()
	plans := make([]apply.Plan, 0, len(kinds))
	for _, k := range kinds {
		p, err := apply.PlanForTarget(k, m, c.ID, t)
		if err != nil {
			// E.g. topology.ErrPortRangeExhausted: only this target fails (D23).
			return newTargetReport(c.ID, t.Address, c.ID, nil, s.failAll(ctx, c, c.ID, nil, err.Error()), m.Skipped), m.Skipped
		}
		plans = append(plans, p)
	}
	for _, p := range plans {
		s.Log.LogAttrs(ctx, slog.LevelInfo, "Manifest built.",
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
	return s.syncTarget(ctx, c, plans, m.Skipped), m.Skipped
}

// logEntryWarnings logs the normalizer warnings of every entry:
// param_ignored at debug (D28), the others at warn.
func (s *Syncer) logEntryWarnings(ctx context.Context, c config.SubscriptionConfig, m manifest.Manifest) {
	for _, e := range m.Entries {
		for _, w := range e.Warnings {
			level := slog.LevelWarn
			if w.Code == normalize.WarnParamIgnored {
				level = slog.LevelDebug
			}
			s.Log.LogAttrs(ctx, level, "Manifest entry warning.",
				slog.String("event", "manifest_entry_warning"),
				slog.String("subscriptionId", c.ID),
				slog.String("targetAddress", c.Target.Address),
				slog.String("tag", e.Tag),
				slog.String("code", w.Code),
				slog.String("message", w.Message))
		}
	}
}

// targetAttrs are the common attributes of the per-kind target events;
// the observatory prefix is omitted when empty.
func targetAttrs(event string, kind apply.Kind, c config.SubscriptionConfig) []slog.Attr {
	attrs := []slog.Attr{
		slog.String("event", event),
		slog.String("kind", string(kind)),
		slog.String("subscriptionId", c.ID),
		slog.String("targetAddress", c.Target.Address),
	}
	if p := c.Target.ObservatorySubjectSelectorPrefix; p != "" {
		attrs = append(attrs, slog.String("observatorySubjectSelectorPrefix", p))
	}
	return attrs
}

// syncTarget compares the plan hashes with the saved ones and, when any
// differs, applies every plan in order under the target lock (03 §2.2).
func (s *Syncer) syncTarget(ctx context.Context, c config.SubscriptionConfig, plans []apply.Plan, skipped []vless.Skip) TargetReport {
	t := c.Target
	key := state.Key(c.ID, t.Address)
	s.Memory.MarkSynced(key)

	changed := false
	s.Memory.View(key, func(tg *state.Target) {
		for _, p := range plans {
			if h, ok := tg.Hashes[p.Kind]; !ok || h != p.ManifestHash {
				changed = true
			}
		}
	})
	if !changed {
		unchanged := make([]apply.Kind, 0, len(plans))
		for _, p := range plans {
			unchanged = append(unchanged, p.Kind)
			s.Log.LogAttrs(ctx, slog.LevelInfo, "Manifest unchanged for resource; skipping Xray API.",
				targetAttrs("skipped_no_changes", p.Kind, c)...)
		}
		return newTargetReport(c.ID, t.Address, c.ID, unchanged, nil, skipped)
	}

	unlock := s.Memory.LockTarget(key)
	defer unlock()
	// The mutation sequence is not cut short by a cancellation; each RPC has
	// the client's own timeout (Review Focus 1, D7).
	actx := context.WithoutCancel(ctx)

	client, err := s.Dial(t.Address, time.Duration(t.TimeoutMs)*time.Millisecond)
	if err != nil {
		reports := make([]apply.Report, 0, len(plans))
		for _, p := range plans {
			r := apply.FailedReport(p.Kind, s.meta(c, p.SourceID), p.Skipped, err)
			s.logApply(ctx, r)
			reports = append(reports, r)
		}
		return newTargetReport(c.ID, t.Address, c.ID, nil, reports, skipped)
	}
	defer func() { _ = client.Close() }()

	reports := make([]apply.Report, 0, len(plans))
	hashes := map[apply.Kind]string{}
	failed := false
	for _, p := range plans {
		s.Log.LogAttrs(ctx, slog.LevelInfo, "Applying resource to target.", targetAttrs("resource_apply_started", p.Kind, c)...)
		r := apply.Apply(actx, client, p, t, s.meta(c, p.SourceID))
		s.logApply(ctx, r)
		reports = append(reports, r)
		if r.Failed > 0 {
			failed = true
			continue
		}
		hashes[p.Kind] = p.ManifestHash
	}
	if !failed {
		// Every plan carries the same target topology.
		s.Memory.ReplaceTopology(key, *plans[len(plans)-1].Topology, hashes)
	}
	return newTargetReport(c.ID, t.Address, c.ID, nil, reports, skipped)
}

// logApply logs apply_finished, or apply_failed with the failed items and
// their messages.
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
		s.Log.LogAttrs(ctx, slog.LevelInfo, "Apply finished.", attrs...)
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
	s.Log.LogAttrs(ctx, slog.LevelError, "Apply failed.", attrs...)
}
