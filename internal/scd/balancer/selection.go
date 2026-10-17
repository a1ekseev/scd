// Package balancer is the balancer selection view of scd (D22, 10): a
// read-only tick polls which outbound the target's Xray balancer currently
// selects (RoutingService.GetBalancerInfo) and the observatory health
// (ObservatoryService.GetOutboundStatus), records them in the target state
// and maps the outbound tags onto scd tunnels for the status API and the
// dashboard.
package balancer

import (
	"slices"
	"strings"

	"github.com/a1ekseev/scd/internal/scd/state"
	"github.com/a1ekseev/scd/internal/scd/xrayapi"
)

// Observatory statuses of a tunnel (10 §6.7).
const (
	// ObservatoryAlive: the tunnel's current outbound is observed alive.
	ObservatoryAlive = "alive"
	// ObservatoryDead: it is observed and the last probe failed.
	ObservatoryDead = "dead"
	// ObservatoryAwaiting: it is in the balancer selector but has no
	// observation yet (rejoin window, 10 §4 p.4).
	ObservatoryAwaiting = "awaiting"
	// ObservatoryNA: not observable — repaired (unprefixed) tunnel, outside
	// the selector, or the observatory is unavailable.
	ObservatoryNA = "n/a"
)

// ResolveSelection derives the selection mode, the effective selected tag
// and the principle pool from a GetBalancerInfo answer (10 §6.2). The
// override always wins (it is what PickOutbound uses); random/roundRobin
// with a single candidate is single as well.
func ResolveSelection(info xrayapi.BalancerInfo) (mode state.Mode, selected string, pool []string) {
	switch {
	case info.OverrideTarget != "":
		return state.ModeOverride, info.OverrideTarget, info.PrincipleTargets
	case !info.PrincipleKnown:
		return state.ModeUnknown, "", nil
	case len(info.PrincipleTargets) == 0:
		return state.ModeNone, "", nil
	case len(info.PrincipleTargets) == 1:
		return state.ModeSingle, info.PrincipleTargets[0], info.PrincipleTargets
	default:
		return state.ModePool, "", info.PrincipleTargets
	}
}

// MapTag maps an Xray outbound tag onto a tunnel of the target (10 §4 p.2),
// in order: the tunnel's current outbound tag; the initial tag of a
// repaired tunnel (a race: the balancer cannot see unprefixed tags); the
// tag without prefix as base tunnel id. Anything else — a static outbound
// of the Xray config or a foreign one — is unmanaged ("", false).
func MapTag(tag, prefix string, tunnels []*state.Tunnel) (tunnelID string, managed bool) {
	if tag == "" {
		return "", false
	}
	for _, tn := range tunnels {
		if tn.Mapping.OutboundTagCurrent == tag {
			return tn.Mapping.BaseTunnelID, true
		}
	}
	for _, tn := range tunnels {
		if m := tn.Mapping; m.OutboundTagInitial == tag && m.OutboundTagCurrent != m.OutboundTagInitial {
			return m.BaseTunnelID, true
		}
	}
	if prefix != "" {
		if base, ok := strings.CutPrefix(tag, prefix); ok {
			for _, tn := range tunnels {
				if tn.Mapping.BaseTunnelID == base {
					return base, true
				}
			}
		}
	}
	return "", false
}

// TunnelView is the balancer selection of one tunnel.
type TunnelView struct {
	// Selected: the tunnel's current outbound is the effective selection.
	Selected bool
	// Candidate: it is one of the principle targets.
	Candidate bool
	// Observation is the observatory status of the current outbound; nil
	// when there is none (stale entries of other tags are never used).
	Observation *xrayapi.OutboundObservation
	// Status is one of the Observatory* constants.
	Status string
}

// ViewTunnel returns the selection view of tn from s (10 §4, §6.6). Only
// the observation of the tunnel's current outbound tag counts, so a stale
// entry of a removed or repaired prefixed tag is filtered out. A tunnel
// counts as in the selector when its current tag has the tunnel's
// observatory prefix (its initial tag minus the base tag) or, without a
// prefix, when it is a principle candidate.
func ViewTunnel(tn *state.Tunnel, s *state.SelectionState) TunnelView {
	tag := tn.Mapping.OutboundTagCurrent
	v := TunnelView{
		Selected:  s.SelectedTag != "" && s.SelectedTag == tag,
		Candidate: slices.Contains(s.PrincipleTargets, tag),
		Status:    ObservatoryNA,
	}
	if s.Observatory == nil {
		return v
	}
	if o, ok := s.Observatory[tag]; ok {
		v.Observation = &o
		v.Status = ObservatoryDead
		if o.Alive {
			v.Status = ObservatoryAlive
		}
		return v
	}
	inSelector := v.Candidate
	if prefix, ok := strings.CutSuffix(tn.Mapping.OutboundTagInitial, tn.Mapping.BaseOutboundTag); ok && prefix != "" {
		inSelector = strings.HasPrefix(tag, prefix)
	}
	if inSelector {
		v.Status = ObservatoryAwaiting
	}
	return v
}
