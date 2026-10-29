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
	"github.com/a1ekseev/scd/internal/scd/selection"
)

// CodePortRangeExhausted is the skip reason code of a line that gets no port
// because the source's portRange is full.
const CodePortRangeExhausted = "port_range_exhausted"

// PreparedOutbound is an outbound ready for one target under one tag.
type PreparedOutbound struct {
	Tag        string
	Normalized normalize.Normalized
	JSON       outbound.Spec
}

// Tunnel is one SOCKS inbound routed to one outbound of a target.
type Tunnel struct {
	// SourceID is the id of the source the tunnel comes from.
	SourceID string
	// Protocol is "wireguard" or "masque" for a candidate tunnel, "" for
	// VLESS.
	Protocol string
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

// BuildSource allocates tunnels for the manifest entries of source src;
// port = src.portRange.start + index, listen = target.inboundSocks.listen.
// Only the outbound tag gets the observatory prefix. Without inboundSocks or
// portRange the topology is empty.
//
// VLESS: every entry, ordered by tag (then line), so ports follow tag order
// and not line order. The entries beyond the portRange capacity (the last
// ones in tag order) get no tunnel and are returned as port_range_exhausted
// skips.
//
// wireguard and masque: the active set, at most maxActive entries ranked
// by selection.strategy without two of one key (the lowest priority of a
// key first); slot i is rank position i.
// The other candidates are inactive, not skipped.
func BuildSource(m manifest.Manifest, src config.SourceConfig, t config.SubscriptionTargetConfig) (Topology, []vless.Skip) {
	if t.InboundSocks == nil || src.PortRange == nil {
		return Topology{}, nil
	}
	pr := *src.PortRange
	var (
		entries []manifest.Entry
		skipped []vless.Skip
	)
	switch {
	case config.CandidateType(src.Type) && src.Selection == nil:
		// Not a valid config (apply.PlanForSource fails it): nothing is active.
	case config.CandidateType(src.Type):
		ranked := selection.Rank(m.Entries, src.Selection.Strategy, manifest.Entry.SelectionCandidate)
		entries = selection.Pick(ranked, src.MaxActive, manifest.Entry.SelectionCandidate)
	default:
		entries, skipped = byTag(m.Entries, pr)
	}
	tunnels := make([]Tunnel, len(entries))
	for i, e := range entries {
		tunnels[i] = NewTunnel(e, src.ID, t, pr.Start+i)
	}
	return Topology{Tunnels: tunnels}, skipped
}

// NewTunnel is the tunnel of entry e of source sourceID on target t (with
// inboundSocks) at port.
func NewTunnel(e manifest.Entry, sourceID string, t config.SubscriptionTargetConfig, port int) Tunnel {
	initial := t.ObservatorySubjectSelectorPrefix + e.Tag
	tn := Tunnel{
		SourceID:              sourceID,
		Protocol:              e.Protocol,
		BaseTunnelID:          e.Tag,
		DisplayName:           e.Label,
		BaseOutboundTag:       e.Tag,
		OutboundTagInitial:    initial,
		OutboundTagCurrent:    initial,
		InboundTag:            "in-" + e.Tag,
		RouteTag:              "route-" + e.Tag,
		Listen:                t.InboundSocks.Listen,
		Port:                  port,
		OutboundInitial:       Prepare(e, t, initial),
		OutboundWithoutPrefix: Prepare(e, t, e.Tag),
	}
	if e.Country != nil {
		tn.CountryISO2 = e.Country.ISO2
	}
	return tn
}

// byTag orders VLESS entries by tag, line and raw link and cuts them to the
// capacity of pr; the rest are returned as port_range_exhausted skips.
func byTag(all []manifest.Entry, pr config.PortRange) ([]manifest.Entry, []vless.Skip) {
	entries := slices.Clone(all)
	slices.SortStableFunc(entries, func(a, b manifest.Entry) int {
		return cmp.Or(strings.Compare(a.Tag, b.Tag), cmp.Compare(a.Line, b.Line), strings.Compare(a.Normalized.Raw, b.Normalized.Raw))
	})
	capacity := max(pr.End-pr.Start+1, 0)
	if len(entries) <= capacity {
		return entries, nil
	}
	var skipped []vless.Skip
	detail := fmt.Sprintf("portRange %d-%d can allocate only %d ports, but %d tunnels were generated", pr.Start, pr.End, capacity, len(entries))
	for _, e := range entries[capacity:] {
		label := e.Label
		skipped = append(skipped, vless.Skip{
			Line: e.Line, Raw: e.Normalized.Raw, Label: &label, Code: CodePortRangeExhausted,
			Reason: "The source portRange has no free port for this line.", Details: []string{detail},
		})
	}
	return entries[:capacity], skipped
}

// Prepare returns the outbound of entry e for target t under tag: a VLESS
// entry with the target's flow override, a wireguard one with its outbound
// retagged.
func Prepare(e manifest.Entry, t config.SubscriptionTargetConfig, tag string) PreparedOutbound {
	if e.Protocol != "" {
		n := e.Normalized
		n.Tag = tag
		return PreparedOutbound{Tag: tag, Normalized: n, JSON: e.JSONOutbound.WithTag(tag)}
	}
	n := outbound.ForTarget(e.Normalized, tag, t.VisionUDP443Override)
	return PreparedOutbound{Tag: tag, Normalized: n, JSON: outbound.Build(n)}
}
