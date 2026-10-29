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
// (candidate_no_replacement). A failed replacement leaves the slot empty
// and resets the source hashes (candidate_replace_failed). The only
// returned error is a failure to create the client.
func ReplaceFailed(ctx context.Context, d Deps, sub config.SubscriptionConfig, src config.SourceConfig, tunnelID string) (done bool, err error) {
	t := sub.Target
	key := state.Key(sub.ID, t.Address)
	unlock := d.Memory.LockTarget(key)
	defer unlock()

	var (
		old    topology.Tunnel
		found  bool
		ranked []manifest.Entry
		others []string
		sel    selection.State
	)
	d.Memory.View(key, func(tg *state.Target) {
		st := tg.Sources[src.ID]
		if st == nil {
			return
		}
		ranked, sel = st.Candidates, st.Selection.Clone()
		for _, tn := range tg.Tunnels {
			switch {
			case tn.Mapping.SourceID != src.ID:
			case tn.Mapping.BaseTunnelID == tunnelID:
				old, found = tn.Mapping, true
			default:
				others = append(others, tn.Mapping.BaseTunnelID)
			}
		}
	})
	if !found {
		return false, nil
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
		started := false
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
		})
		if started {
			d.Log.LogAttrs(ctx, slog.LevelWarn, "No replacement candidate outside quarantine; the tunnel stays in repair.",
				candidateAttrs("candidate_no_replacement", sub, src.ID, tunnelID, "")...)
		}
		return false, nil
	}

	client, err := d.dial(sub)
	if err != nil {
		return true, err
	}
	defer func() { _ = client.Close() }()
	nt := topology.NewTunnel(byTag[next.Tag], src.ID, t, old.Port)
	ruleErr, err := apply.ReplaceTunnel(context.WithoutCancel(ctx), client, old, nt)
	if ruleErr != nil {
		attrs := append(candidateAttrs("candidate_replace_failed", sub, src.ID, tunnelID, nt.BaseTunnelID),
			slog.String("stage", "remove_rule"), slog.String("error", ruleErr.Error()))
		d.Log.LogAttrs(ctx, slog.LevelWarn, "Candidate replace: the old routing rule could not be removed; going on.", attrs...)
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
		attrs := append(candidateAttrs("candidate_replace_failed", sub, src.ID, tunnelID, nt.BaseTunnelID), slog.String("error", err.Error()))
		d.Log.LogAttrs(ctx, slog.LevelError, "Candidate replace failed; the slot stays empty until the next sync.", attrs...)
		return true, nil
	}
	d.Log.LogAttrs(ctx, slog.LevelInfo, "Candidate replaced.",
		candidateAttrs("candidate_replaced", sub, src.ID, tunnelID, nt.BaseTunnelID)...)
	return true, nil
}

// candidateAttrs are the attributes of a candidate_* event of the monitor
// (reasonCode failed); toTag is omitted when empty.
func candidateAttrs(event string, sub config.SubscriptionConfig, sourceID, from, to string) []slog.Attr {
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
	return append(attrs, slog.String("reasonCode", selection.ReasonFailed))
}
