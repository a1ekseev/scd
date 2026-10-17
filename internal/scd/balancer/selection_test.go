package balancer

import (
	"slices"
	"testing"
	"time"

	"github.com/a1ekseev/scd/internal/scd/state"
	"github.com/a1ekseev/scd/internal/scd/topology"
	"github.com/a1ekseev/scd/internal/scd/xrayapi"
)

// 10 §6.9 p.2: override × len(principle) 0/1/3 × PrincipleKnown.
func TestResolveSelectionTable(t *testing.T) {
	one := []string{"x-observe-a"}
	three := []string{"x-observe-a", "x-observe-b", "x-observe-c"}
	cases := []struct {
		name         string
		info         xrayapi.BalancerInfo
		mode         state.Mode
		selected     string
		pool         []string
		poolIsNilOut bool
	}{
		{"override/0/known", xrayapi.BalancerInfo{OverrideTarget: "o", PrincipleTargets: []string{}, PrincipleKnown: true}, state.ModeOverride, "o", []string{}, false},
		{"override/1/known", xrayapi.BalancerInfo{OverrideTarget: "o", PrincipleTargets: one, PrincipleKnown: true}, state.ModeOverride, "o", one, false},
		{"override/3/known", xrayapi.BalancerInfo{OverrideTarget: "o", PrincipleTargets: three, PrincipleKnown: true}, state.ModeOverride, "o", three, false},
		{"override/unknown", xrayapi.BalancerInfo{OverrideTarget: "o"}, state.ModeOverride, "o", nil, true},
		{"none/0/known", xrayapi.BalancerInfo{PrincipleTargets: []string{}, PrincipleKnown: true}, state.ModeNone, "", nil, true},
		{"single/1/known", xrayapi.BalancerInfo{PrincipleTargets: one, PrincipleKnown: true}, state.ModeSingle, "x-observe-a", one, false},
		{"pool/3/known", xrayapi.BalancerInfo{PrincipleTargets: three, PrincipleKnown: true}, state.ModePool, "", three, false},
		{"unknown/0", xrayapi.BalancerInfo{}, state.ModeUnknown, "", nil, true},
		{"unknown/ignores-targets", xrayapi.BalancerInfo{PrincipleTargets: one}, state.ModeUnknown, "", nil, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			mode, selected, pool := ResolveSelection(c.info)
			if mode != c.mode || selected != c.selected || !slices.Equal(pool, c.pool) || (pool == nil) != c.poolIsNilOut {
				t.Errorf("got (%q, %q, %#v), want (%q, %q, %#v)", mode, selected, pool, c.mode, c.selected, c.pool)
			}
		})
	}
}

// tunnelOf is a tunnel whose current outbound tag is current (initial tag
// prefix+id).
func tunnelOf(id, prefix, current string) *state.Tunnel {
	return &state.Tunnel{Mapping: topology.Tunnel{
		BaseTunnelID: id, BaseOutboundTag: id, DisplayName: "Name " + id,
		OutboundTagInitial: prefix + id, OutboundTagCurrent: current,
		OutboundInitial: topology.PreparedOutbound{Tag: prefix + id}, OutboundWithoutPrefix: topology.PreparedOutbound{Tag: id},
	}}
}

// 10 §6.9 p.3, §4 p.2.
func TestMapTag(t *testing.T) {
	const p = "x-observe-"
	tunnels := []*state.Tunnel{
		tunnelOf("de-1", p, p+"de-1"),    // balanced
		tunnelOf("nl-1", p, "nl-1"),      // repaired (unprefixed)
		tunnelOf("at-1", p, p+"at-1"),    // balanced
		tunnelOf("custom", "", "custom"), // no prefix
	}
	cases := []struct {
		name, tag, prefix string
		id                string
		managed           bool
	}{
		{"prefixed current", p + "de-1", p, "de-1", true},
		{"repaired by initial tag (race)", p + "nl-1", p, "nl-1", true},
		{"repaired current tag", "nl-1", p, "nl-1", true},
		{"unmanaged static", p + "static", p, "", false},
		{"foreign", "direct", p, "", false},
		{"empty", "", p, "", false},
		{"empty prefix exact", "custom", "", "custom", true},
		{"empty prefix no strip", p + "gone", "", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			id, managed := MapTag(c.tag, c.prefix, tunnels)
			if id != c.id || managed != c.managed {
				t.Errorf("MapTag(%q, %q) = (%q, %v), want (%q, %v)", c.tag, c.prefix, id, managed, c.id, c.managed)
			}
		})
	}

	// Prefix strip finds a tunnel whose current tag changed meanwhile (race
	// with a topology replacement).
	race := []*state.Tunnel{tunnelOf("be-1", p, "be-1-new")}
	if id, ok := MapTag(p+"be-1", p, race); id != "be-1" || !ok {
		t.Errorf("prefix strip = (%q, %v)", id, ok)
	}
}

// 10 §6.9 p.3: per-tunnel selection and observatory view.
func TestTunnelObservatory(t *testing.T) {
	const p = "x-observe-"
	delay := int64(42)
	seen := time.Date(2026, 10, 17, 9, 0, 0, 0, time.UTC)
	sel := state.SelectionState{
		State: state.SelectionOK, Mode: state.ModeSingle, SelectedTag: p + "de-1",
		PrincipleTargets: []string{p + "de-1"},
		Observatory: map[string]xrayapi.OutboundObservation{
			p + "de-1":   {Tag: p + "de-1", Alive: true, DelayMs: &delay, LastSeenAt: &seen},
			p + "nl-1":   {Tag: p + "nl-1", Alive: true, DelayMs: &delay}, // stale: nl-1 is repaired
			p + "gone":   {Tag: p + "gone", Alive: true},                  // stale: removed tunnel
			p + "static": {Tag: p + "static", Alive: false, LastError: "dead"},
		},
	}
	cases := []struct {
		name           string
		tn             *state.Tunnel
		selected, cand bool
		status         string
		wantObs        bool
	}{
		{"selected alive", tunnelOf("de-1", p, p+"de-1"), true, true, ObservatoryAlive, true},
		{"repaired", tunnelOf("nl-1", p, "nl-1"), false, false, ObservatoryNA, false},
		{"rejoin window", tunnelOf("at-1", p, p+"at-1"), false, false, ObservatoryAwaiting, false},
		{"no prefix outside selector", tunnelOf("custom", "", "custom"), false, false, ObservatoryNA, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v := ViewTunnel(c.tn, &sel)
			if v.Selected != c.selected || v.Candidate != c.cand || v.Status != c.status || (v.Observation != nil) != c.wantObs {
				t.Errorf("view = %+v", v)
			}
		})
	}

	// Observatory unavailable: every tunnel is n/a.
	off := sel
	off.Observatory, off.ObservatoryError = nil, "Xray API service ObservatoryService is not enabled (add it to api.services)."
	if v := ViewTunnel(tunnelOf("at-1", p, p+"at-1"), &off); v.Status != ObservatoryNA || v.Observation != nil {
		t.Errorf("unavailable view = %+v", v)
	}
	// Dead: delay is unset, the error is kept.
	dead := sel
	dead.Observatory = map[string]xrayapi.OutboundObservation{p + "de-1": {Tag: p + "de-1", LastError: "GET failed"}}
	if v := ViewTunnel(tunnelOf("de-1", p, p+"de-1"), &dead); v.Status != ObservatoryDead || v.Observation.LastError != "GET failed" {
		t.Errorf("dead view = %+v", v)
	}
	// No prefix: a principle candidate without observation is awaiting.
	np := state.SelectionState{
		State: state.SelectionOK, Mode: state.ModePool, PrincipleTargets: []string{"custom", "x"},
		Observatory: map[string]xrayapi.OutboundObservation{},
	}
	if v := ViewTunnel(tunnelOf("custom", "", "custom"), &np); !v.Candidate || v.Status != ObservatoryAwaiting {
		t.Errorf("no-prefix candidate view = %+v", v)
	}
}
