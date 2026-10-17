// Package topology derives the per-target tunnel topology from a manifest
// (01 §12.2): a stable SOCKS port, inbound/route tags and the outbound pair
// (with and without the observatory prefix) for every entry.
package topology

import (
	"cmp"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/a1ekseev/scd/internal/scd/config"
	"github.com/a1ekseev/scd/internal/scd/manifest"
	"github.com/a1ekseev/scd/internal/scd/normalize"
	"github.com/a1ekseev/scd/internal/scd/outbound"
)

// ErrPortRangeExhausted is returned (wrapped) when the target's
// inboundSocks.portRange has fewer ports than there are tunnels (D21, D23).
var ErrPortRangeExhausted = errors.New("inboundSocks.portRange is too small")

// PreparedOutbound is an outbound ready for one target under one tag.
type PreparedOutbound struct {
	Tag        string
	Normalized normalize.Normalized
	JSON       outbound.Outbound
}

// Tunnel is one SOCKS inbound routed to one outbound of a target.
type Tunnel struct {
	// BaseTunnelID and BaseOutboundTag are the entry tag.
	BaseTunnelID    string
	DisplayName     string
	CountryISO2     string
	BaseOutboundTag string
	// OutboundTagInitial is prefix+tag; OutboundTagCurrent starts equal
	// and is changed by the runtime on repair.
	OutboundTagInitial string
	OutboundTagCurrent string
	InboundTag         string
	RouteTag           string
	Listen             string
	Port               int
	// OutboundInitial carries OutboundTagInitial, OutboundWithoutPrefix the
	// bare entry tag.
	OutboundInitial       PreparedOutbound
	OutboundWithoutPrefix PreparedOutbound
}

// Topology is the tunnel list of one target, ordered by tag.
type Topology struct {
	Tunnels []Tunnel
}

// Build allocates tunnels for every manifest entry. Entries are ordered by
// tag (then line), so ports follow tag order and not line order; port =
// portRange.start + index. Only the outbound tag gets the observatory
// prefix. A target without inboundSocks has an empty topology.
func Build(m manifest.Manifest, t config.SubscriptionTargetConfig) (Topology, error) {
	if t.InboundSocks == nil {
		return Topology{}, nil
	}
	in := t.InboundSocks
	entries := slices.Clone(m.Entries)
	slices.SortStableFunc(entries, func(a, b manifest.Entry) int {
		return cmp.Or(strings.Compare(a.Tag, b.Tag), cmp.Compare(a.Line, b.Line), strings.Compare(a.Normalized.Raw, b.Normalized.Raw))
	})
	capacity := max(in.PortRange.End-in.PortRange.Start+1, 0)
	if len(entries) > capacity {
		return Topology{}, fmt.Errorf("target %q inboundSocks.portRange can allocate only %d ports, but %d tunnels were generated: %w",
			t.Address, capacity, len(entries), ErrPortRangeExhausted)
	}
	tunnels := make([]Tunnel, len(entries))
	for i, e := range entries {
		initial := t.ObservatorySubjectSelectorPrefix + e.Tag
		tn := Tunnel{
			BaseTunnelID:          e.Tag,
			DisplayName:           e.Label,
			BaseOutboundTag:       e.Tag,
			OutboundTagInitial:    initial,
			OutboundTagCurrent:    initial,
			InboundTag:            "in-" + e.Tag,
			RouteTag:              "route-" + e.Tag,
			Listen:                in.Listen,
			Port:                  in.PortRange.Start + i,
			OutboundInitial:       prepare(e, t, initial),
			OutboundWithoutPrefix: prepare(e, t, e.Tag),
		}
		if e.Country != nil {
			tn.CountryISO2 = e.Country.ISO2
		}
		tunnels[i] = tn
	}
	return Topology{Tunnels: tunnels}, nil
}

func prepare(e manifest.Entry, t config.SubscriptionTargetConfig, tag string) PreparedOutbound {
	n := outbound.ForTarget(e.Normalized, tag, t.VisionUDP443Override)
	return PreparedOutbound{Tag: tag, Normalized: n, JSON: outbound.Build(n)}
}
