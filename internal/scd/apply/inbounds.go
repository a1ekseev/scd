package apply

import (
	"context"
	"slices"

	"github.com/a1ekseev/scd/internal/scd/resources"
	"github.com/a1ekseev/scd/internal/scd/topology"
	"github.com/a1ekseev/scd/internal/scd/xrayapi"
)

// Inbounds replaces every non-fixed inbound of the target with the SOCKS
// inbounds of tunnels (02 §2.3, D2):
//
//  1. a tunnel inbound tag listed in fixed fails the report without any RPC;
//  2. ListInbounds; its error is returned as is (the caller turns it into a
//     FailedReport);
//  3. every listed inbound that is not fixed is removed;
//  4. every tunnel inbound is added: "replaced" when its tag was listed in 2,
//     else "added".
//
// A failure in 3 or 4 ends the report with one failed item whose id is the
// first tunnel's inbound tag (or "inbound"); there is no rollback. The
// report's Skipped is empty, as in TS.
func Inbounds(ctx context.Context, c xrayapi.InboundAPI, fixed []string, tunnels []topology.Tunnel, m Meta) (Report, error) {
	startedAt := m.now()
	r := newReport(KindInbound, m, startedAt, nil)

	for _, tn := range tunnels {
		if slices.Contains(fixed, tn.InboundTag) {
			r.Failed = 1
			r.Items = append(r.Items, Item{
				ID:      tn.InboundTag,
				Status:  StatusFailed,
				Message: "Managed inbound tag \"" + tn.InboundTag + "\" conflicts with fixedInbounds on target \"" + m.TargetAddress + "\".",
			})
			r.DurationMs = m.now().Sub(startedAt).Milliseconds()
			return r, nil
		}
	}

	existing, err := c.ListInbounds(ctx)
	if err != nil {
		return Report{}, err
	}
	existingTags := map[string]bool{}
	for _, h := range existing {
		existingTags[h.GetTag()] = true
	}

	apply := func() error {
		for _, h := range existing {
			tag := h.GetTag()
			if slices.Contains(fixed, tag) {
				continue
			}
			if err := c.RemoveInbound(ctx, tag); err != nil {
				return err
			}
			r.Removed++
			r.DeletedIDs = append(r.DeletedIDs, tag)
			r.Items = append(r.Items, Item{ID: tag, Status: StatusRemoved})
		}
		for _, tn := range tunnels {
			h, err := resources.Inbound(tn)
			if err != nil {
				return err
			}
			if err := c.AddInbound(ctx, h); err != nil {
				return err
			}
			r.AppliedIDs = append(r.AppliedIDs, tn.InboundTag)
			r.Items = append(r.Items, Item{ID: tn.InboundTag, Status: r.countAddition(existingTags[tn.InboundTag])})
		}
		return nil
	}

	if err := apply(); err != nil {
		id := "inbound"
		if len(tunnels) > 0 {
			id = tunnels[0].InboundTag
		}
		r.Failed = 1
		r.Items = append(r.Items, Item{ID: id, Status: StatusFailed, Message: err.Error()})
	}

	r.DurationMs = m.now().Sub(startedAt).Milliseconds()
	r.AppliedAt = timestamp(m.now())
	return r, nil
}

// countAddition counts an added resource as replaced when it existed
// before the apply, else as added, and returns its item status.
func (r *Report) countAddition(existed bool) string {
	if existed {
		r.Replaced++
		return StatusReplaced
	}
	r.Added++
	return StatusAdded
}
