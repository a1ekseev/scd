package apply

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/a1ekseev/scd/internal/common/vless"
	"github.com/a1ekseev/scd/internal/scd/config"
	"github.com/a1ekseev/scd/internal/scd/manifest"
	"github.com/a1ekseev/scd/internal/scd/outbound"
	"github.com/a1ekseev/scd/internal/scd/topology"
	"github.com/a1ekseev/scd/internal/scd/xrayapi"
)

// Plan is what one kind of one source applies to its target (the TS
// ResourcePlan after preparePlanForTarget, 02 §2.1): the sync loop compares
// ManifestHash with the hash it saved for the source and, on a change,
// applies the plan.
type Plan struct {
	Kind Kind
	// SourceID is the id of the source; its namespace is what Apply replaces.
	SourceID string
	// ManifestHash is the hex sha256 the sync loop diffs on (02 §2.1 p.3).
	ManifestHash string
	// ManagedIDs are the outbound tags, inbound tags or route tags the plan
	// creates, in apply order.
	ManagedIDs []string
	// Outbounds are the source's outbounds, sorted by tag (outbound only).
	Outbounds []outbound.Outbound
	// Topology is the source's tunnel topology (empty without inboundSocks
	// or portRange); never nil.
	Topology *topology.Topology
	// Skipped and Summary come from the manifest, plus the entries beyond
	// the portRange.
	Skipped []vless.Skip
	Summary *manifest.Summary
}

// PlanForSource prepares the plan of kind for source src on target t from
// the source manifest m:
//
//   - outbound: with inboundSocks the tunnels' initial outbounds, else every
//     entry for tag prefix+tag with the target's flow override; ordered by
//     tag, line and raw link; hash = outbound.Hash;
//   - inbound: hash = sha256(JSON [{inboundTag, listen, port}]), ids = the
//     inbound tags;
//   - routing: hash = sha256(JSON [{routeTag, inboundTag, outboundTag}]) with
//     the initial outbound tag, ids = the route tags.
//
// Entries beyond the portRange get no resource of any kind: they are
// appended to Skipped (port_range_exhausted) and counted in
// Summary.Skipped and Summary.PortRangeExhausted.
func PlanForSource(kind Kind, m manifest.Manifest, src config.SourceConfig, t config.SubscriptionTargetConfig) (Plan, error) {
	topo, overflow := topology.BuildSource(m, src, t)
	sourceID := src.ID
	summary := m.Summary
	summary.Skipped += len(overflow)
	summary.PortRangeExhausted = len(overflow)
	skipped := append(append([]vless.Skip{}, m.Skipped...), overflow...)
	p := Plan{Kind: kind, SourceID: sourceID, Topology: &topo, Skipped: skipped, Summary: &summary}

	switch kind {
	case KindOutbound:
		p.Outbounds = targetOutbounds(m, t, topo, t.InboundSocks != nil && src.PortRange != nil)
		p.ManagedIDs = make([]string, 0, len(p.Outbounds))
		for _, o := range p.Outbounds {
			p.ManagedIDs = append(p.ManagedIDs, o.Tag)
		}
		p.ManifestHash = outbound.Hash(p.Outbounds)
	case KindInbound:
		type item struct {
			InboundTag string `json:"inboundTag"`
			Listen     string `json:"listen"`
			Port       int    `json:"port"`
		}
		items := make([]item, 0, len(topo.Tunnels))
		p.ManagedIDs = make([]string, 0, len(topo.Tunnels))
		for _, tn := range topo.Tunnels {
			items = append(items, item{InboundTag: tn.InboundTag, Listen: tn.Listen, Port: tn.Port})
			p.ManagedIDs = append(p.ManagedIDs, tn.InboundTag)
		}
		p.ManifestHash = jsonHash(items)
	case KindRouting:
		type item struct {
			RouteTag    string `json:"routeTag"`
			InboundTag  string `json:"inboundTag"`
			OutboundTag string `json:"outboundTag"`
		}
		items := make([]item, 0, len(topo.Tunnels))
		p.ManagedIDs = make([]string, 0, len(topo.Tunnels))
		for _, tn := range topo.Tunnels {
			items = append(items, item{RouteTag: tn.RouteTag, InboundTag: tn.InboundTag, OutboundTag: tn.OutboundTagCurrent})
			p.ManagedIDs = append(p.ManagedIDs, tn.RouteTag)
		}
		p.ManifestHash = jsonHash(items)
	default:
		return Plan{}, fmt.Errorf("unknown resource kind %q", kind)
	}
	return p, nil
}

// targetOutbounds returns the outbounds of target t ordered by tag, line and
// raw link: the tunnels' initial outbounds when the source is tunnelled
// (already in that order), else every entry built for prefix+tag. The
// prefix is the same for every tag, so it does not change the order.
func targetOutbounds(m manifest.Manifest, t config.SubscriptionTargetConfig, topo topology.Topology, tunnelled bool) []outbound.Outbound {
	if tunnelled {
		out := make([]outbound.Outbound, 0, len(topo.Tunnels))
		for _, tn := range topo.Tunnels {
			out = append(out, tn.OutboundInitial.JSON)
		}
		return out
	}
	entries := slices.Clone(m.Entries)
	slices.SortStableFunc(entries, func(a, b manifest.Entry) int {
		return cmp.Or(strings.Compare(a.Tag, b.Tag), cmp.Compare(a.Line, b.Line), strings.Compare(a.Normalized.Raw, b.Normalized.Raw))
	})
	out := make([]outbound.Outbound, 0, len(entries))
	for _, e := range entries {
		n := outbound.ForTarget(e.Normalized, t.ObservatorySubjectSelectorPrefix+e.Tag, t.VisionUDP443Override)
		out = append(out, outbound.Build(n))
	}
	return out
}

// jsonHash is the hex sha256 of v serialized like JSON.stringify: no HTML
// escaping, no trailing newline.
func jsonHash(v any) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		// v holds only strings and ints.
		panic(fmt.Sprintf("apply: marshal: %v", err))
	}
	sum := sha256.Sum256(bytes.TrimSuffix(buf.Bytes(), []byte("\n")))
	return hex.EncodeToString(sum[:])
}

// Apply applies p to target t through c with the algorithm of its kind,
// replacing only the resources in the namespace of p.SourceID (Namespace),
// and always returns a report: an error the algorithm returns (a List RPC)
// becomes a FailedReport carrying p.Skipped (02 §2.1 p.5). The report's
// sourceId is p.SourceID and its targetAddress t.Address; m supplies the
// subscription id and the clock. As in TS, only the outbound report carries
// p.Skipped; the inbound and routing reports carry [].
func Apply(ctx context.Context, c xrayapi.Client, p Plan, t config.SubscriptionTargetConfig, m Meta) Report {
	m.SourceID = p.SourceID
	m.TargetAddress = t.Address
	var tunnels []topology.Tunnel
	if p.Topology != nil {
		tunnels = p.Topology.Tunnels
	}
	skipped := p.Skipped
	if skipped == nil {
		skipped = []vless.Skip{}
	}

	var (
		r    Report
		err  error
		owns = Namespace{ObsPrefix: t.ObservatorySubjectSelectorPrefix, SourceID: p.SourceID}.Owns
	)
	switch p.Kind {
	case KindOutbound:
		r, err = Outbounds(ctx, c, t.FixedOutbounds, owns, p.Outbounds, m)
		r.Skipped = skipped
	case KindInbound:
		r, err = Inbounds(ctx, c, t.FixedInbounds, owns, tunnels, m)
	case KindRouting:
		r, err = Routing(ctx, c, t.FixedRouting, owns, tunnels, m)
	default:
		err = fmt.Errorf("unknown resource kind %q", p.Kind)
	}
	if err != nil {
		return FailedReport(p.Kind, m, skipped, err)
	}
	return r
}

// Owner reports whether an apply owns the resource of kind with tag: only
// owned resources are removed. Fixed resources are never removed either way.
type Owner func(kind Kind, tag string) bool
