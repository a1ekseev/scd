package syncer

import (
	"context"
	"log/slog"
	"slices"
	"time"

	"github.com/a1ekseev/scd/internal/common/vless"
	"github.com/a1ekseev/scd/internal/scd/apply"
	"github.com/a1ekseev/scd/internal/scd/config"
	"github.com/a1ekseev/scd/internal/scd/manifest"
	"github.com/a1ekseev/scd/internal/scd/selection"
	"github.com/a1ekseev/scd/internal/scd/state"
	"github.com/a1ekseev/scd/internal/scd/topology"
	"github.com/a1ekseev/scd/internal/scd/xrayapi"
)

// wgSync is one sync of a wireguard or masque source under its target lock.
type wgSync struct {
	s       *Syncer
	c       config.SubscriptionConfig
	src     config.SourceConfig
	key     string
	client  xrayapi.Client
	now     time.Time
	ranked  []manifest.Entry
	sel     selection.State
	skipped []vless.Skip
}

// syncWireGuard syncs wireguard or masque source src under the target lock. It
// applies the source in full (every kind, like syncPlans) when it has no
// state, its hashes were reset, apply.ConfigHash changed or Xray lost its
// resources; the active set is then the first maxActive ranked candidates
// outside quarantine. Otherwise selection.Decide replaces or removes single
// tunnels (apply.ReplaceTunnel, apply.RemoveTunnel), then empty slots are
// filled while a candidate can take them (apply.AddTunnel, reason vacant);
// a failure leaves the slot empty and resets the hashes. ok is false when ctx was cancelled before
// any RPC.
func (s *Syncer) syncWireGuard(ctx context.Context, c config.SubscriptionConfig, src config.SourceConfig, m manifest.Manifest, skipped []vless.Skip) (r TargetReport, ok bool) {
	t := c.Target
	key := state.Key(c.ID, t.Address)
	s.Memory.MarkSynced(key)
	unlock := s.Memory.LockTarget(key)
	defer unlock()
	if ctx.Err() != nil {
		return TargetReport{}, false
	}

	var (
		cur   *state.Source
		slots []state.Tunnel
	)
	w := &wgSync{
		s: s, c: c, src: src, key: key, now: s.now(), skipped: skipped,
		ranked: selection.Rank(m.Entries, src.Selection.Strategy, manifest.Entry.SelectionCandidate),
	}
	s.Memory.View(key, func(tg *state.Target) {
		st := tg.Sources[src.ID]
		if st == nil {
			return
		}
		cp := *st
		cur = &cp
		w.sel = st.Selection.Clone()
		for _, tn := range tg.Tunnels {
			if tn.Mapping.SourceID == src.ID {
				slots = append(slots, *tn)
			}
		}
	})

	client, err := s.dial(c)
	if err != nil {
		reports := make([]apply.Report, 0, 3)
		for _, k := range s.kinds() {
			r := apply.FailedReport(k, s.meta(c, src.ID), skipped, err)
			s.logApply(ctx, r)
			reports = append(reports, r)
		}
		return newTargetReport(c.ID, t.Address, src.ID, nil, reports, skipped), true
	}
	defer func() { _ = client.Close() }()
	w.client = client

	full := cur == nil || len(cur.Hashes) == 0 || cur.ConfigHash != apply.ConfigHash(src, t)
	if !full {
		var plans []apply.Plan
		for _, k := range s.kinds() {
			p, _ := apply.TopologyPlan(k, src.ID, *cur.Topology)
			plans = append(plans, p)
		}
		if missing := missingIn(ctx, client, t, plans); len(missing) > 0 {
			s.logDrift(ctx, c, src.ID, missing)
			full = true
		}
	}
	if full {
		return w.full(ctx, m, slots), true
	}
	return w.decide(ctx, slots, w.slots(slots, cur)), true
}

// candidates are the selection view of the ranked entries.
func (w *wgSync) candidates() []selection.Candidate {
	out := make([]selection.Candidate, 0, len(w.ranked))
	for _, e := range w.ranked {
		out = append(out, e.SelectionCandidate())
	}
	return out
}

// slots are the selection view of the source's tunnels: the candidate from
// the current file, else from the last sync, else the tag only. A tunnel
// is healthy when the monitor has not failed it and it is in the balancer.
func (w *wgSync) slots(tunnels []state.Tunnel, cur *state.Source) []selection.Slot {
	known := map[string]selection.Candidate{}
	if cur != nil {
		for _, e := range cur.Candidates {
			known[e.Tag] = e.SelectionCandidate()
		}
	}
	for _, e := range w.ranked {
		known[e.Tag] = e.SelectionCandidate()
	}
	out := make([]selection.Slot, 0, len(tunnels))
	for _, tn := range tunnels {
		c, ok := known[tn.Mapping.BaseTunnelID]
		if !ok {
			c = selection.Candidate{Tag: tn.Mapping.BaseTunnelID}
		}
		healthy := tn.Monitor.State != state.TunnelDegraded && tn.Monitor.State != state.TunnelRepairing &&
			tn.Mapping.OutboundTagCurrent == tn.Mapping.OutboundTagInitial
		out = append(out, selection.Slot{Candidate: c, Healthy: healthy})
	}
	return out
}

// full applies the source in full with the candidates outside quarantine.
// A key with a transport (masque) whose active tunnel is still eligible
// keeps that transport: priority only picks a new one. A failure resets the
// hashes of an existing state.
func (w *wgSync) full(ctx context.Context, m manifest.Manifest, active []state.Tunnel) TargetReport {
	s, c, src := w.s, w.c, w.src
	eligible := m
	eligible.Entries = nil
	for _, e := range m.Entries {
		if !w.sel.Quarantined(e.SelectionCandidate(), w.now) {
			eligible.Entries = append(eligible.Entries, e)
		}
	}
	kept := map[[32]byte]string{}
	for _, e := range eligible.Entries {
		cand := e.SelectionCandidate()
		if cand.Transport != "" && slices.ContainsFunc(active, func(tn state.Tunnel) bool { return tn.Mapping.BaseTunnelID == e.Tag }) {
			kept[cand.Key] = cand.Transport
		}
	}
	if len(kept) > 0 {
		eligible.Entries = slices.DeleteFunc(eligible.Entries, func(e manifest.Entry) bool {
			cand := e.SelectionCandidate()
			t, ok := kept[cand.Key]
			return ok && cand.Transport != t
		})
	}
	plans := make([]apply.Plan, 0, 3)
	for _, k := range s.kinds() {
		p, err := apply.PlanForSource(k, eligible, src, c.Target)
		if err != nil {
			return newTargetReport(c.ID, c.Target.Address, src.ID, nil, s.failAll(ctx, c, src.ID, nil, err.Error(), true), w.skipped)
		}
		plans = append(plans, p)
	}
	reports, hashes, failed := s.applyPlans(ctx, w.client, c, src.ID, plans)
	if !failed {
		s.Memory.ReplaceSourceTopology(w.key, src.ID, *plans[len(plans)-1].Topology, hashes)
	}
	s.Memory.Update(w.key, func(tg *state.Target) {
		st := tg.Sources[src.ID]
		if st == nil {
			return
		}
		if failed {
			// Xray no longer matches the old topology: no repair, rejoin or
			// replacement may act on it until the next full apply.
			tg.DropSourceTunnels(src.ID)
		} else {
			st.ConfigHash = apply.ConfigHash(src, c.Target)
			pruneNoReplacement(st, src.MaxActive)
		}
		st.Selection, st.Candidates = w.sel, w.ranked
	})
	return newTargetReport(c.ID, c.Target.Address, src.ID, nil, reports, w.skipped)
}

// decide runs selection.Decide over the slots and carries out its
// replacements and removals, stopping at the first failure.
func (w *wgSync) decide(ctx context.Context, tunnels []state.Tunnel, active []selection.Slot) TargetReport {
	s, c, src, t := w.s, w.c, w.src, w.c.Target
	actx := context.WithoutCancel(ctx)
	entries := map[string]manifest.Entry{}
	for _, e := range w.ranked {
		entries[e.Tag] = e
	}
	var (
		removed, added []topology.Tunnel
		failedID       string
		failErr        error
	)
loop:
	for _, a := range selection.Decide(active, w.candidates(), &w.sel, w.now, *src.Selection) {
		old := tunnels[a.Slot].Mapping
		switch a.Kind {
		case selection.Remove:
			ruleErr, err := apply.RemoveTunnel(actx, w.client, old)
			w.ruleFailed(ctx, old.BaseTunnelID, "", a.Reason, ruleErr)
			s.Memory.Update(w.key, func(tg *state.Target) {
				tg.ReplaceSourceTunnel(src.ID, old.BaseTunnelID, nil)
				if st := tg.Sources[src.ID]; err == nil && st != nil {
					if st.NoReplacement == nil {
						st.NoReplacement = map[string]bool{}
					}
					st.NoReplacement[old.BaseTunnelID] = true
				}
			})
			if err != nil {
				failedID, failErr = old.OutboundTagCurrent, err
				w.replaceFailed(ctx, old.BaseTunnelID, "", a.Reason, err)
				break loop
			}
			removed = append(removed, old)
			s.Log.LogAttrs(ctx, slog.LevelWarn, "Candidate removed; no replacement outside quarantine.",
				w.attrs("candidate_removed", old.BaseTunnelID, "", a.Reason)...)
		case selection.Replace:
			next := topology.NewTunnel(entries[a.To.Tag], src.ID, t, old.Port)
			ruleErr, err := apply.ReplaceTunnel(actx, w.client, old, next)
			w.ruleFailed(ctx, old.BaseTunnelID, next.BaseTunnelID, a.Reason, ruleErr)
			nt := &next
			if err != nil {
				nt = nil
			}
			s.Memory.Update(w.key, func(tg *state.Target) { tg.ReplaceSourceTunnel(src.ID, old.BaseTunnelID, nt) })
			if err != nil {
				failedID, failErr = next.OutboundTagInitial, err
				w.replaceFailed(ctx, old.BaseTunnelID, next.BaseTunnelID, a.Reason, err)
				break loop
			}
			removed, added = append(removed, old), append(added, next)
			s.Log.LogAttrs(ctx, slog.LevelInfo, "Candidate replaced.",
				w.attrs("candidate_replaced", old.BaseTunnelID, next.BaseTunnelID, a.Reason)...)
		}
	}
	if failErr == nil {
		var filled []topology.Tunnel
		filled, failedID, failErr = w.fill(ctx, entries)
		added = append(added, filled...)
	}
	s.Memory.Update(w.key, func(tg *state.Target) {
		if st := tg.Sources[src.ID]; st != nil {
			st.Selection, st.Candidates = w.sel, w.ranked
		}
	})
	if len(removed) == 0 && len(added) == 0 && failErr == nil {
		unchanged := s.kinds()
		for _, k := range unchanged {
			s.Log.LogAttrs(ctx, slog.LevelDebug, "Manifest unchanged for resource; skipping Xray API.",
				targetAttrs("skipped_no_changes", k, c, src.ID)...)
		}
		return newTargetReport(c.ID, t.Address, src.ID, unchanged, nil, w.skipped)
	}
	reports := apply.SlotReports(s.kinds(), s.meta(c, src.ID), removed, added, failedID, failErr)
	for _, r := range reports {
		s.logApply(ctx, r)
	}
	return newTargetReport(c.ID, t.Address, src.ID, nil, reports, w.skipped)
}

// fill adds candidates to the empty slots of the source while it has
// fewer tunnels than maxActive and selection.Replacement allows one: the
// lowest free port of the first maxActive ports, apply.AddTunnel,
// candidate_replaced with reason vacant and no fromTag. It stops at the
// first failure (the slot stays empty, the hashes are reset) and returns
// the added tunnels and the failure.
func (w *wgSync) fill(ctx context.Context, entries map[string]manifest.Entry) (added []topology.Tunnel, failedID string, err error) {
	s, src := w.s, w.src
	known := map[string]selection.Candidate{}
	for _, e := range w.ranked {
		known[e.Tag] = e.SelectionCandidate()
	}
	actx := context.WithoutCancel(ctx)
	for {
		inUse, keys, ports := map[string]bool{}, map[[32]byte]bool{}, map[int]bool{}
		s.Memory.View(w.key, func(tg *state.Target) {
			for _, tn := range tg.Tunnels {
				if tn.Mapping.SourceID != src.ID {
					continue
				}
				inUse[tn.Mapping.BaseTunnelID], ports[tn.Mapping.Port] = true, true
				if c, ok := known[tn.Mapping.BaseTunnelID]; ok {
					keys[c.Key] = true
				}
			}
		})
		if len(inUse) >= src.MaxActive {
			return added, "", nil
		}
		c, ok := selection.Replacement(w.candidates(), inUse, keys, &w.sel, w.now)
		if !ok {
			return added, "", nil
		}
		port := src.PortRange.Start
		for ports[port] {
			port++
		}
		next := topology.NewTunnel(entries[c.Tag], src.ID, w.c.Target, port)
		if err := apply.AddTunnel(actx, w.client, next); err != nil {
			w.replaceFailed(ctx, "", next.BaseTunnelID, selection.ReasonVacant, err)
			return added, next.OutboundTagInitial, err
		}
		s.Memory.Update(w.key, func(tg *state.Target) {
			tg.ReplaceSourceTunnel(src.ID, "", &next)
			// The slot of one removed tunnel is no longer empty: drop the
			// tag of next itself if it was one, else any other removed tag.
			nr := tg.Sources[src.ID].NoReplacement
			if nr[next.BaseTunnelID] {
				delete(nr, next.BaseTunnelID)
			} else {
				for tag := range nr {
					if !inUse[tag] {
						delete(nr, tag)
						break
					}
				}
			}
		})
		added = append(added, next)
		s.Log.LogAttrs(ctx, slog.LevelInfo, "Candidate replaced.",
			w.attrs("candidate_replaced", "", next.BaseTunnelID, selection.ReasonVacant)...)
	}
}

// ruleFailed logs a failed RemoveRule of a replacement or removal, which
// does not stop it: candidate_replace_failed at warn with stage
// remove_rule.
func (w *wgSync) ruleFailed(ctx context.Context, from, to, reason string, err error) {
	if err == nil {
		return
	}
	attrs := append(w.attrs("candidate_replace_failed", from, to, reason), slog.String("stage", "remove_rule"), slog.String("error", err.Error()))
	w.s.Log.LogAttrs(ctx, slog.LevelWarn, "Candidate replace: the old routing rule could not be removed; going on.", attrs...)
}

// pruneNoReplacement keeps, after a full apply, the no_replacement tags of
// the tunnels still active and at most as many others as there are empty
// slots.
func pruneNoReplacement(st *state.Source, maxActive int) {
	active := map[string]bool{}
	for _, tn := range st.Topology.Tunnels {
		active[tn.BaseTunnelID] = true
	}
	empty := maxActive - len(st.Topology.Tunnels)
	for tag := range st.NoReplacement {
		switch {
		case active[tag]:
		case empty > 0:
			empty--
		default:
			delete(st.NoReplacement, tag)
		}
	}
}

// replaceFailed resets the source hashes after a failed replacement or
// removal and logs candidate_replace_failed.
func (w *wgSync) replaceFailed(ctx context.Context, from, to, reason string, err error) {
	w.s.Memory.Update(w.key, func(tg *state.Target) { tg.ResetSourceHashes(w.src.ID) })
	attrs := append(w.attrs("candidate_replace_failed", from, to, reason), slog.String("error", err.Error()))
	w.s.Log.LogAttrs(ctx, slog.LevelError, "Candidate replace failed; the slot stays empty until the next sync.", attrs...)
}

// attrs are the attributes of the candidate_* events: subscriptionId,
// targetAddress, sourceId, fromTag and toTag (each when not empty) and
// reasonCode.
func (w *wgSync) attrs(event, from, to, reason string) []slog.Attr {
	attrs := []slog.Attr{
		slog.String("event", event),
		slog.String("subscriptionId", w.c.ID),
		slog.String("targetAddress", w.c.Target.Address),
		slog.String("sourceId", w.src.ID),
	}
	if from != "" {
		attrs = append(attrs, slog.String("fromTag", from))
	}
	if to != "" {
		attrs = append(attrs, slog.String("toTag", to))
	}
	return append(attrs, slog.String("reasonCode", reason))
}
