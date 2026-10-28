package apply

import (
	"context"
	"slices"

	"github.com/xtls/xray-core/core"

	"github.com/a1ekseev/scd/internal/common/jsonout"
	"github.com/a1ekseev/scd/internal/scd/outbound"
	"github.com/a1ekseev/scd/internal/scd/xrayapi"
)

// Outbounds replaces every non-fixed outbound of the target with entries
// (02 §2.2, D2), in the order given (the applicator sorts them):
//
//  1. an entry tag listed in fixed fails the report without any RPC;
//  2. ListOutbounds; its error is returned as is (the caller turns it into a
//     FailedReport);
//  3. every listed outbound that owns reports for KindOutbound is removed,
//     its config kept as a backup; fixed ones are never removed;
//  4. every entry is added: "replaced" when its tag was removed in 3, else
//     "added".
//
// A failure in 3 or 4 rolls back best-effort — the outbounds added by this
// apply are removed in reverse order, then the backups are re-added in
// removal order, rollback errors ignored — and ends the report with one
// failed item. The report's Skipped is empty; the caller sets it.
func Outbounds(ctx context.Context, c xrayapi.OutboundAPI, fixed []string, owns Owner, entries []outbound.Outbound, m Meta) (Report, error) {
	startedAt := m.now()
	r := newReport(KindOutbound, m, startedAt, nil)
	finish := func() {
		r.DurationMs = m.now().Sub(startedAt).Milliseconds()
	}

	for _, e := range entries {
		if slices.Contains(fixed, e.Tag) {
			r.Failed = 1
			r.Items = append(r.Items, Item{
				ID:      e.Tag,
				Status:  StatusFailed,
				Message: "Managed outbound tag \"" + e.Tag + "\" conflicts with fixedOutbounds on target \"" + m.TargetAddress + "\".",
			})
			finish()
			return r, nil
		}
	}

	existing, err := c.ListOutbounds(ctx)
	if err != nil {
		return Report{}, err
	}

	var (
		backups   []*core.OutboundHandlerConfig // in removal order
		removed   = map[string]bool{}
		added     []string
		currentID string
	)
	apply := func() error {
		for _, h := range existing {
			tag := h.GetTag()
			if removed[tag] || slices.Contains(fixed, tag) || !owns(KindOutbound, tag) {
				continue
			}
			if err := c.RemoveOutbound(ctx, tag); err != nil {
				return err
			}
			backups = append(backups, h)
			removed[tag] = true
			r.Removed++
			r.DeletedIDs = append(r.DeletedIDs, tag)
			r.Items = append(r.Items, Item{ID: tag, Status: StatusRemoved})
		}
		for _, e := range entries {
			currentID = e.Tag
			h, err := outbound.ToProto(e)
			if err != nil {
				return err
			}
			if err := c.AddOutbound(ctx, h); err != nil {
				return err
			}
			added = append(added, e.Tag)
			r.AppliedIDs = append(r.AppliedIDs, e.Tag)
			status := StatusAdded
			if removed[e.Tag] {
				status = StatusReplaced
				r.Replaced++
			} else {
				r.Added++
			}
			r.Items = append(r.Items, Item{ID: e.Tag, Status: status})
		}
		return nil
	}

	if err := apply(); err != nil {
		// Best-effort rollback; it runs even if ctx is already canceled so
		// that Xray is not left half-applied (each RPC keeps its own timeout).
		rctx := context.WithoutCancel(ctx)
		for _, tag := range slices.Backward(added) {
			_ = c.RemoveOutbound(rctx, tag)
		}
		for _, h := range backups {
			_ = c.AddOutbound(rctx, h)
		}
		r.Failed = 1
		r.Items = append(r.Items, Item{ID: failedID(currentID, added, entries), Status: StatusFailed, Message: err.Error()})
	}

	finish()
	r.AppliedAt = jsonout.Timestamp(m.now())
	return r, nil
}

// failedID is the id of the failed item: the entry being added, else the
// last added one, else the first entry, else "unknown".
func failedID(currentID string, added []string, entries []outbound.Outbound) string {
	switch {
	case currentID != "":
		return currentID
	case len(added) > 0:
		return added[len(added)-1]
	case len(entries) > 0 && entries[0].Tag != "":
		return entries[0].Tag
	default:
		return "unknown"
	}
}
