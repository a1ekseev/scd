package apply

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/a1ekseev/scd/internal/common/jsonout"
	"github.com/a1ekseev/scd/internal/scd/config"
	"github.com/a1ekseev/scd/internal/scd/outbound"
	"github.com/a1ekseev/scd/internal/scd/resources"
	"github.com/a1ekseev/scd/internal/scd/topology"
	"github.com/a1ekseev/scd/internal/scd/xrayapi"
)

// ErrReplaceFailed wraps every error of ReplaceTunnel and RemoveTunnel: the
// slot is then empty and the caller resets the source hashes, so the next
// sync applies the source in full.
var ErrReplaceFailed = errors.New("candidate replace failed")

// ConfigHash is the hash of what a full apply of a wireguard or masque source
// depends on besides its candidates: type, portRange, inboundSocks.listen,
// the observatory prefix, endpointDomainStrategy and maxActive. A change
// makes the next sync apply the source in full.
func ConfigHash(src config.SourceConfig, t config.SubscriptionTargetConfig) string {
	v := struct {
		Type, Listen, Prefix, DomainStrategy string
		PortRange                            *config.PortRange
		MaxActive                            int
	}{Type: src.Type, Prefix: t.ObservatorySubjectSelectorPrefix, PortRange: src.PortRange, MaxActive: src.MaxActive}
	if t.InboundSocks != nil {
		v.Listen = t.InboundSocks.Listen
	}
	if src.WireGuard != nil {
		v.DomainStrategy = src.WireGuard.EndpointDomainStrategy
	}
	return jsonHash(v)
}

// RemoveTunnel removes the tunnel old of a slot: RemoveRule(route) (best
// effort: its error is returned as ruleErr and does not stop it), then
// RemoveOutbound of both outbound tags (current first; Xray does not fail
// on an unknown tag, so an outbound that state no longer knows about is
// removed too), then RemoveInbound(in). A failed RemoveOutbound restores
// the rule best effort and stops before anything else. err wraps
// ErrReplaceFailed.
func RemoveTunnel(ctx context.Context, c xrayapi.Client, old topology.Tunnel) (ruleErr, err error) {
	ruleErr = c.RemoveRule(ctx, old.RouteTag)
	for _, tag := range outboundTags(old) {
		if err := c.RemoveOutbound(ctx, tag); err != nil {
			_ = c.AddRule(ctx, resources.Rule(old, old.OutboundTagCurrent))
			return ruleErr, fmt.Errorf("%w: %w", ErrReplaceFailed, err)
		}
	}
	if err := c.RemoveInbound(ctx, old.InboundTag); err != nil {
		return ruleErr, fmt.Errorf("%w: %w", ErrReplaceFailed, err)
	}
	return ruleErr, nil
}

// outboundTags are the outbound tags of tn, current first, without
// duplicates.
func outboundTags(tn topology.Tunnel) []string {
	tags := []string{tn.OutboundTagCurrent}
	for _, tag := range []string{tn.OutboundTagInitial, tn.OutboundWithoutPrefix.Tag} {
		if tag != "" && !slices.Contains(tags, tag) {
			tags = append(tags, tag)
		}
	}
	return tags
}

// ReplaceTunnel replaces the tunnel old of a slot by next on the same port,
// remove before add, so the source never has more outbounds than slots:
// RemoveTunnel(old), then AddOutbound(next, with the observatory prefix),
// AddInbound(in-next), AddRule(route-next → next). Nothing is added when
// the removal fails, and nothing is rolled back: any error (wrapping
// ErrReplaceFailed) leaves the slot empty. ruleErr is RemoveTunnel's.
func ReplaceTunnel(ctx context.Context, c xrayapi.Client, old, next topology.Tunnel) (ruleErr, err error) {
	h, err := outbound.ToProto(next.OutboundInitial.JSON)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrReplaceFailed, err)
	}
	in, err := resources.Inbound(next)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrReplaceFailed, err)
	}
	if ruleErr, err = RemoveTunnel(ctx, c, old); err != nil {
		return ruleErr, err
	}
	err = c.AddOutbound(ctx, h)
	if err == nil {
		err = c.AddInbound(ctx, in)
	}
	if err == nil {
		err = c.AddRule(ctx, resources.Rule(next, next.OutboundTagInitial))
	}
	if err != nil {
		return ruleErr, fmt.Errorf("%w: %w", ErrReplaceFailed, err)
	}
	return ruleErr, nil
}

// AddTunnel adds next to an empty slot: AddOutbound(next, with the
// observatory prefix), AddInbound(in-next on the slot's port),
// AddRule(route-next → next). The caller calls it only while the source
// has fewer tunnels than maxActive. A failure rolls back what was added,
// best effort and in reverse (RemoveRule, RemoveInbound, RemoveOutbound),
// and returns an error wrapping ErrReplaceFailed.
func AddTunnel(ctx context.Context, c xrayapi.Client, next topology.Tunnel) error {
	h, err := outbound.ToProto(next.OutboundInitial.JSON)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrReplaceFailed, err)
	}
	in, err := resources.Inbound(next)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrReplaceFailed, err)
	}
	if err := c.AddOutbound(ctx, h); err != nil {
		_ = c.RemoveOutbound(ctx, next.OutboundTagInitial) // the add may have happened
		return fmt.Errorf("%w: %w", ErrReplaceFailed, err)
	}
	if err := c.AddInbound(ctx, in); err != nil {
		_ = c.RemoveOutbound(ctx, next.OutboundTagInitial)
		return fmt.Errorf("%w: %w", ErrReplaceFailed, err)
	}
	if err := c.AddRule(ctx, resources.Rule(next, next.OutboundTagInitial)); err != nil {
		_ = c.RemoveRule(ctx, next.RouteTag)
		_ = c.RemoveInbound(ctx, next.InboundTag)
		_ = c.RemoveOutbound(ctx, next.OutboundTagInitial)
		return fmt.Errorf("%w: %w", ErrReplaceFailed, err)
	}
	return nil
}

// SlotReports are the per-kind reports of the slot changes of one source
// (ReplaceTunnel, RemoveTunnel) in kinds order: the resources of removed
// counted as removed, of added as added. A non-nil err fails the outbound
// report with one item for failedID.
func SlotReports(kinds []Kind, m Meta, removed, added []topology.Tunnel, failedID string, err error) []Report {
	ids := map[Kind]func(topology.Tunnel) string{
		KindOutbound: func(tn topology.Tunnel) string { return tn.OutboundTagCurrent },
		KindInbound:  func(tn topology.Tunnel) string { return tn.InboundTag },
		KindRouting:  func(tn topology.Tunnel) string { return tn.RouteTag },
	}
	out := make([]Report, 0, len(kinds))
	for _, k := range kinds {
		r := newReport(k, m, m.now(), nil)
		for _, tn := range removed {
			r.Removed++
			r.DeletedIDs = append(r.DeletedIDs, ids[k](tn))
			r.Items = append(r.Items, Item{ID: ids[k](tn), Status: StatusRemoved})
		}
		for _, tn := range added {
			r.Added++
			r.AppliedIDs = append(r.AppliedIDs, ids[k](tn))
			r.Items = append(r.Items, Item{ID: ids[k](tn), Status: StatusAdded})
		}
		if err != nil && k == KindOutbound {
			r.Failed = 1
			r.Items = append(r.Items, Item{ID: failedID, Status: StatusFailed, Message: err.Error()})
		}
		r.AppliedAt = jsonout.Timestamp(m.now())
		out = append(out, r)
	}
	return out
}
