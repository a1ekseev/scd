package monitor

import (
	"context"
	"log/slog"

	"github.com/a1ekseev/scd/internal/scd/apply"
	"github.com/a1ekseev/scd/internal/scd/config"
	"github.com/a1ekseev/scd/internal/scd/manifest"
	"github.com/a1ekseev/scd/internal/scd/selection"
	"github.com/a1ekseev/scd/internal/scd/state"
	"github.com/a1ekseev/scd/internal/scd/topology"
)

// ReplaceFailed replaces tunnel tunnelID of wireguard or masque source src
// after replaceAfterFailures failed checks in a row, under the target lock:
// its pair goes into quarantine for deadPairCooldownMinutes, and the first
// candidate of the last sync that selection.Replacement allows takes its
// slot and port (apply.ReplaceTunnel, candidate_replaced with reason
// failed). Without one the tunnel is left to the usual repair (done is
// false) and gets the no_replacement warning, logged once when it starts
// (candidate_no_replacement; when no other tunnel of the source is healthy
// it starts the source's episode without a live tunnel,
// state.Source.NoLiveSince, inside which it and candidate_replaced are
// debug). A failed replacement leaves the slot empty
// and resets the source hashes (candidate_replace_failed, at debug when the
// Xray API is unavailable: xray_unavailable). The only
// returned error is a failure to create the client. While the source's
// hashes are reset (it waits for a full apply) nothing is replaced, and a
// tunnel whose inbound is gone from Xray is handled as by Repair
// (xray_state_lost, done is true).
func ReplaceFailed(ctx context.Context, d Deps, sub config.SubscriptionConfig, src config.SourceConfig, tunnelID string) (done bool, err error) {
	t := sub.Target
	key := state.Key(sub.ID, t.Address)
	unlock := d.Memory.LockTarget(key)
	defer unlock()

	var (
		old     topology.Tunnel
		found   bool
		waiting bool
		ranked  []manifest.Entry
		others  []string
		sel     selection.State
		probe   state.TunnelMonitor
		noLive  bool
	)
	d.Memory.View(key, func(tg *state.Target) {
		st := tg.Sources[src.ID]
		if st == nil {
			return
		}
		waiting, noLive = len(st.Hashes) == 0, st.NoLiveSince != nil
		ranked, sel = st.Candidates, st.Selection.Clone()
		for _, tn := range tg.Tunnels {
			switch {
			case tn.Mapping.SourceID != src.ID:
			case tn.Mapping.BaseTunnelID == tunnelID:
				old, found, probe = tn.Mapping, true, tn.Monitor
			default:
				others = append(others, tn.Mapping.BaseTunnelID)
			}
		}
	})
	// A source whose hashes are reset waits for a full apply (Xray lost its
	// resources, a replacement failed): its tunnels fail because of Xray,
	// not of their candidate, so they are only repaired.
	if !found || waiting {
		return false, nil
	}
	client, err := d.dial(sub)
	if err != nil {
		return true, err
	}
	defer func() { _ = client.Close() }()
	// Repair's check: without its inbound the tunnel waits for the sync.
	present, err := hasInbound(context.WithoutCancel(ctx), client, old.InboundTag)
	if err != nil {
		return false, nil // the repair meets the same error and logs it
	}
	if !present {
		d.stateLost(ctx, sub, old)
		return true, nil
	}
	now := d.now()
	byTag := map[string]manifest.Entry{}
	cands := make([]selection.Candidate, 0, len(ranked))
	for _, e := range ranked {
		byTag[e.Tag] = e
		cands = append(cands, e.SelectionCandidate())
	}
	if e, ok := byTag[tunnelID]; ok {
		sel.QuarantinePair(e.SelectionCandidate(), now, src.Selection.DeadPairCooldownMinutes)
	}
	active, keys := map[string]bool{tunnelID: true}, map[[32]byte]bool{}
	for _, tag := range others {
		active[tag] = true
		if e, ok := byTag[tag]; ok {
			keys[e.SelectionCandidate().Key] = true
		}
	}
	next, ok := selection.Replacement(cands, active, keys, &sel, now)
	if !ok {
		started, inEpisode := false, false
		d.Memory.Update(key, func(tg *state.Target) {
			st := tg.Sources[src.ID]
			if st == nil {
				return
			}
			st.Selection = sel
			if !st.NoReplacement[tunnelID] {
				if st.NoReplacement == nil {
					st.NoReplacement = map[string]bool{}
				}
				st.NoReplacement[tunnelID], started = true, true
			}
			inEpisode = st.NoLiveSince != nil
			if !inEpisode && !otherHealthy(tg, src.ID, tunnelID) {
				st.NoLiveSince = &now // no tunnel of the source works: the episode starts
			}
		})
		if started {
			// Inside the source's episode without a live tunnel it is debug.
			level := slog.LevelWarn
			if inEpisode {
				level = slog.LevelDebug
			}
			d.Log.LogAttrs(ctx, level, "No replacement candidate outside quarantine; the tunnel stays in repair.",
				candidateAttrs("candidate_no_replacement", sub, src.ID, tunnelID, "", probe)...)
		}
		return false, nil
	}

	nt := topology.NewTunnel(byTag[next.Tag], src.ID, t, old.Port)
	ruleErr, err := apply.ReplaceTunnel(context.WithoutCancel(ctx), client, old, nt)
	down, _ := state.NoteXray(ctx, d.Memory, d.Log, sub.ID, t.Address, err, d.now())
	if ruleErr != nil {
		attrs := append(candidateAttrs("candidate_replace_failed", sub, src.ID, tunnelID, nt.BaseTunnelID, probe),
			slog.String("stage", "remove_rule"), slog.String("error", ruleErr.Error()))
		level := slog.LevelWarn
		if down {
			level = slog.LevelDebug
		}
		d.Log.LogAttrs(ctx, level, "Candidate replace: the old routing rule could not be removed; going on.", attrs...)
	}
	d.Memory.Update(key, func(tg *state.Target) {
		if st := tg.Sources[src.ID]; st != nil {
			st.Selection = sel
		}
		if err != nil {
			tg.ReplaceSourceTunnel(src.ID, tunnelID, nil)
			tg.ResetSourceHashes(src.ID)
			return
		}
		tg.ReplaceSourceTunnel(src.ID, tunnelID, &nt)
	})
	if err != nil {
		attrs := append(candidateAttrs("candidate_replace_failed", sub, src.ID, tunnelID, nt.BaseTunnelID, probe), slog.String("error", err.Error()))
		level := slog.LevelError
		if down {
			level = slog.LevelDebug // xray_unavailable covers it
		}
		d.Log.LogAttrs(ctx, level, "Candidate replace failed; the slot stays empty until the next sync.", attrs...)
		return true, nil
	}
	level := slog.LevelInfo
	if noLive {
		level = slog.LevelDebug // inside the source's episode without a live candidate
	}
	d.Log.LogAttrs(ctx, level, "Candidate replaced.",
		candidateAttrs("candidate_replaced", sub, src.ID, tunnelID, nt.BaseTunnelID, probe)...)
	return true, nil
}

// candidateAttrs are the attributes of a candidate_* event of the monitor
// (reasonCode failed, with the probe error and failuresInRow of the failed
// tunnel); toTag is omitted when empty.
func candidateAttrs(event string, sub config.SubscriptionConfig, sourceID, from, to string, probe state.TunnelMonitor) []slog.Attr {
	attrs := []slog.Attr{
		slog.String("event", event),
		slog.String("subscriptionId", sub.ID),
		slog.String("targetAddress", sub.Target.Address),
		slog.String("sourceId", sourceID),
		slog.String("fromTag", from),
	}
	if to != "" {
		attrs = append(attrs, slog.String("toTag", to))
	}
	return append(attrs, slog.String("reasonCode", selection.ReasonFailed),
		slog.String("probeError", probe.LastError), slog.Int("failuresInRow", probe.FailuresInRow))
}

// otherHealthy reports whether source sourceID has a healthy tunnel other
// than tunnelID. It must run inside View or Update.
func otherHealthy(tg *state.Target, sourceID, tunnelID string) bool {
	for _, tn := range tg.Tunnels {
		if tn.Mapping.SourceID == sourceID && tn.Mapping.BaseTunnelID != tunnelID && tn.Monitor.State == state.TunnelHealthy {
			return true
		}
	}
	return false
}
