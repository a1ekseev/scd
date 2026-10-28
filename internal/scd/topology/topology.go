// Package topology derives the per-target tunnel topology from a manifest
// (01 §12.2): a stable SOCKS port, inbound/route tags and the outbound pair
// (with and without the observatory prefix) for every entry.
package topology

import (
	"cmp"
	"fmt"
	"slices"
	"strings"

	"github.com/a1ekseev/scd/internal/common/vless"
	"github.com/a1ekseev/scd/internal/scd/config"
	"github.com/a1ekseev/scd/internal/scd/manifest"
	"github.com/a1ekseev/scd/internal/scd/normalize"
	"github.com/a1ekseev/scd/internal/scd/outbound"
)

// CodePortRangeExhausted is the skip reason code of a line that gets no port
// because the source's portRange is full.
const CodePortRangeExhausted = "port_range_exhausted"

// PreparedOutbound is an outbound ready for one target under one tag.
type PreparedOutbound struct {
	Tag        string
	Normalized normalize.Normalized
	JSON       outbound.Outbound
}

// Tunnel is one SOCKS inbound routed to one outbound of a target.
type Tunnel struct {
	// SourceID is the id of the source the tunnel comes from.
	SourceID string
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

// Topology is the tunnel list of one source on its target, ordered by tag.
type Topology struct {
	Tunnels []Tunnel
}

// BuildSource allocates tunnels for every manifest entry of source src.
// Entries are ordered by tag (then line), so ports follow tag order and not
// line order; port = src.portRange.start + index, listen =
// target.inboundSocks.listen. Only the outbound tag gets the observatory
// prefix. The entries beyond the portRange capacity (the last ones in tag
// order) get no tunnel and are returned as port_range_exhausted skips.
// Without inboundSocks or portRange the topology is empty.
func BuildSource(m manifest.Manifest, src config.SourceConfig, t config.SubscriptionTargetConfig) (Topology, []vless.Skip) {
	if t.InboundSocks == nil || src.PortRange == nil {
		return Topology{}, nil
	}
	pr := *src.PortRange
	entries := slices.Clone(m.Entries)
	slices.SortStableFunc(entries, func(a, b manifest.Entry) int {
		return cmp.Or(strings.Compare(a.Tag, b.Tag), cmp.Compare(a.Line, b.Line), strings.Compare(a.Normalized.Raw, b.Normalized.Raw))
	})
	capacity := max(pr.End-pr.Start+1, 0)
	var skipped []vless.Skip
	if len(entries) > capacity {
		detail := fmt.Sprintf("portRange %d-%d can allocate only %d ports, but %d tunnels were generated", pr.Start, pr.End, capacity, len(entries))
		for _, e := range entries[capacity:] {
			label := e.Label
			skipped = append(skipped, vless.Skip{
				Line: e.Line, Raw: e.Normalized.Raw, Label: &label, Code: CodePortRangeExhausted,
				Reason: "The source portRange has no free port for this line.", Details: []string{detail},
			})
		}
		entries = entries[:capacity]
	}
	tunnels := make([]Tunnel, len(entries))
	for i, e := range entries {
		initial := t.ObservatorySubjectSelectorPrefix + e.Tag
		tn := Tunnel{
			SourceID:              src.ID,
			BaseTunnelID:          e.Tag,
			DisplayName:           e.Label,
			BaseOutboundTag:       e.Tag,
			OutboundTagInitial:    initial,
			OutboundTagCurrent:    initial,
			InboundTag:            "in-" + e.Tag,
			RouteTag:              "route-" + e.Tag,
			Listen:                t.InboundSocks.Listen,
			Port:                  pr.Start + i,
			OutboundInitial:       prepare(e, t, initial),
			OutboundWithoutPrefix: prepare(e, t, e.Tag),
		}
		if e.Country != nil {
			tn.CountryISO2 = e.Country.ISO2
		}
		tunnels[i] = tn
	}
	return Topology{Tunnels: tunnels}, skipped
}

func prepare(e manifest.Entry, t config.SubscriptionTargetConfig, tag string) PreparedOutbound {
	n := outbound.ForTarget(e.Normalized, tag, t.VisionUDP443Override)
	return PreparedOutbound{Tag: tag, Normalized: n, JSON: outbound.Build(n)}
}
