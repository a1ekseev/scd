// Package state is the in-memory runtime state of scd (03 §1): per target
// and source the saved manifest hashes and the tunnel topology, the monitor
// state of every tunnel and the balancer monitor state, plus the sync overlap guard
// and the per-target mutation lock shared by sync apply, repair and rejoin
// (03 §1.1).
//
// All fields of a Target are read and written only inside View or Update,
// which hold the state lock; no I/O happens under it.
package state

import (
	"fmt"
	"maps"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/a1ekseev/scd/internal/scd/apply"
	"github.com/a1ekseev/scd/internal/scd/manifest"
	"github.com/a1ekseev/scd/internal/scd/normalize"
	"github.com/a1ekseev/scd/internal/scd/selection"
	"github.com/a1ekseev/scd/internal/scd/topology"
	"github.com/a1ekseev/scd/internal/scd/xrayapi"
)

// Tunnel monitor states.
const (
	TunnelIdle      = "idle"
	TunnelHealthy   = "healthy"
	TunnelDegraded  = "degraded"
	TunnelRepairing = "repairing"
)

// Balancer monitor states.
const (
	BalancerIdle     = "idle"
	BalancerHealthy  = "healthy"
	BalancerDegraded = "degraded"
)

// Remote ping (push) states of the balancer monitor.
const (
	RemotePingIdle    = "idle"
	RemotePingPending = "pending"
	RemotePingOK      = "ok"
	RemotePingFailed  = "failed"
)

// Balancer selection states (D22, 10 §6.4). The zero value "" means the
// selection was never polled (the feature is off or its first tick has not
// run yet); status shows it as idle.
const (
	SelectionIdle  = "idle"
	SelectionOK    = "ok"
	SelectionError = "error"
)

// Mode is how the Xray balancer currently selects (10 §6.2).
type Mode string

// Selection modes.
const (
	// ModeOverride: OverrideBalancerTarget is set and always wins.
	ModeOverride Mode = "override"
	// ModeSingle: one principle target (leastPing, leastLoad with one node).
	ModeSingle Mode = "single"
	// ModePool: several principle targets, chosen per connection (random,
	// roundRobin, leastLoad with expected > 1).
	ModePool Mode = "pool"
	// ModeNone: no alive candidate; traffic goes to fallbackTag or the
	// default outbound.
	ModeNone Mode = "none"
	// ModeUnknown: Xray returned no principle target.
	ModeUnknown Mode = "unknown"
)

// SelectionState is the balancer selection state of a target (D22, 10
// §6.4), written by the balancer selection tick. It survives topology
// replacements. On a GetBalancerInfo error the last selection is kept.
type SelectionState struct {
	// State is "" (never polled), ok or error.
	State       string
	BalancerTag string
	Mode        Mode
	// SelectedTag is the effective selection: the override or the single
	// principle target; "" for pool, none and unknown.
	SelectedTag      string
	OverrideTarget   string
	PrincipleTargets []string
	// SelectedSince is when SelectedTag last changed.
	SelectedSince       *time.Time
	PreviousSelectedTag string

	LastCheckedAt, LastSuccessAt, LastFailureAt *time.Time
	// LastError is the GetBalancerInfo error of the last failed check.
	LastError           string
	ConsecutiveFailures int

	// Observatory is the last GetOutboundStatus answer by tag; nil when it
	// is disabled or failed (no decisions on stale liveness).
	Observatory map[string]xrayapi.OutboundObservation
	// ObservatoryError is the last GetOutboundStatus error.
	ObservatoryError string
	// ObservatoryCheckedAt is the time of the last successful
	// GetOutboundStatus.
	ObservatoryCheckedAt *time.Time
}

// TunnelMonitor is the monitor state of one tunnel (TS TunnelMonitorState).
// Nil pointers are values that were never set or were cleared.
type TunnelMonitor struct {
	// State is idle, healthy, degraded or repairing.
	State                                       string
	LastCheckedAt, LastSuccessAt, LastFailureAt *time.Time
	LastStatusCode, LastLatencyMs               *int
	LastSuccessStatusCode, LastSuccessLatencyMs *int
	LastError                                   string
	ConsecutiveFailures                         int
	// FailuresInRow counts the failed checks in a row; unlike
	// ConsecutiveFailures a repair does not reset it, only a successful
	// check (and a new tunnel) does. It drives replaceAfterFailures.
	FailuresInRow int
}

// BalancerMonitor is the balancer monitor state of a target (TS
// TargetBalancerMonitorState).
type BalancerMonitor struct {
	// State is idle, healthy or degraded; RemotePingState is idle, pending,
	// ok or failed.
	State, RemotePingState string
	// RemotePingLastReportedStatus is "", up or down.
	RemotePingLastReportedStatus, RemotePingLastReportedMsg string
	RemotePingLastReportedPingMs                            *int

	LastCheckedAt, LastSuccessAt, LastFailureAt *time.Time
	LastStatusCode, LastLatencyMs               *int
	LastSuccessStatusCode, LastSuccessLatencyMs *int
	LastError                                   string

	RemotePingLastRunAt, RemotePingLastSuccessAt, RemotePingLastFailureAt *time.Time
	RemotePingLastStatusCode                                              *int
	RemotePingLastError                                                   string

	ConsecutiveFailures int
}

// Tunnel is the runtime state of one tunnel: its mapping (whose
// OutboundTagCurrent repair and rejoin change) and its monitor state.
type Tunnel struct {
	Mapping topology.Tunnel
	Monitor TunnelMonitor
}

// Source is the state of one source of a target.
type Source struct {
	// Hashes are the manifest hashes of the source's last fully successful
	// apply.
	Hashes map[apply.Kind]string
	// Topology is the source's topology of that apply.
	Topology *topology.Topology
	// Warnings are the source-level warnings of the last successful sync
	// (port_range_exhausted); nil when there are none.
	Warnings []normalize.Warning
	// CandidateCount is the number of candidates of a wireguard or masque source after
	// its filters at the last successful sync; 0 for VLESS.
	CandidateCount int

	// The rest is wireguard and masque only. ConfigHash is apply.ConfigHash of the
	// last full apply. Candidates are the filtered candidates of the last
	// sync, ranked: the monitor replaces a failed tunnel from them.
	ConfigHash string
	Candidates []manifest.Entry
	// Selection is the quarantine and hysteresis state; it survives a full
	// apply.
	Selection selection.State
	// NoReplacement holds the base tags of the tunnels with no replacement
	// outside quarantine: kept in repair, or removed with their slot left
	// empty.
	NoReplacement map[string]bool
}

// CodeNoReplacement is the code of the source warning for NoReplacement.
const CodeNoReplacement = "no_replacement"

// AllWarnings returns Warnings plus a no_replacement warning when
// NoReplacement is not empty.
func (s *Source) AllWarnings() []normalize.Warning {
	out := slices.Clone(s.Warnings)
	if n := len(s.NoReplacement); n > 0 {
		out = append(out, normalize.Warning{
			Code:    CodeNoReplacement,
			Message: fmt.Sprintf("%d tunnels have no replacement candidate outside quarantine.", n),
		})
	}
	return out
}

// Target is the state of one subscription target.
type Target struct {
	// SyncSeen reports that a sync reached this target (03 §1: in TS the
	// target state exists only from then on), even if its apply failed. The
	// balancer monitor waits for it; the balancer selection tick may create
	// the target earlier (10 §6.3) without setting it.
	SyncSeen bool
	// Sources holds, by source id, every source with a fully successful
	// apply.
	Sources map[string]*Source
	// Generation is incremented on every source topology replacement or
	// drop.
	Generation int
	// Tunnels are the tunnels of every source, ordered by source id, then
	// in topology (tag) order (the monitor relies on it).
	Tunnels []*Tunnel
	// Balancer survives topology replacements.
	Balancer BalancerMonitor
	// BalancerSelection survives topology replacements (10 §6.4).
	BalancerSelection SelectionState
}

// Memory is the state of every target of this process.
type Memory struct {
	mu      sync.RWMutex
	targets map[string]*Target

	syncing atomic.Bool

	locksMu sync.Mutex
	locks   map[string]*sync.Mutex
}

// New returns an empty Memory.
func New() *Memory {
	return &Memory{targets: map[string]*Target{}, locks: map[string]*sync.Mutex{}}
}

// Key is the target key "<subscriptionId>::<targetAddress>".
func Key(subscriptionID, targetAddress string) string {
	return subscriptionID + "::" + targetAddress
}

// newTarget is the initial state of a target (03 §1).
func newTarget() *Target {
	return &Target{
		Sources:  map[string]*Source{},
		Balancer: BalancerMonitor{State: BalancerIdle, RemotePingState: RemotePingIdle},
	}
}

// View calls fn with the target of key under the read lock and reports
// whether the target exists. fn must not keep the pointer or do I/O.
func (m *Memory) View(key string, fn func(t *Target)) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	t, ok := m.targets[key]
	if ok {
		fn(t)
	}
	return ok
}

// Update calls fn with the target of key under the write lock and reports
// whether the target exists; a missing target is not created.
func (m *Memory) Update(key string, fn func(t *Target)) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.targets[key]
	if ok {
		fn(t)
	}
	return ok
}

// Ensure creates the initial state of key if it does not exist.
func (m *Memory) Ensure(key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.targets[key]; !ok {
		m.targets[key] = newTarget()
	}
}

// MarkSynced creates the target of key if needed and records that a sync
// reached it (SyncSeen).
func (m *Memory) MarkSynced(key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.targets[key]
	if !ok {
		t = newTarget()
		m.targets[key] = t
	}
	t.SyncSeen = true
}

// ReplaceSourceTopology installs topo and hashes of source sourceID after
// its fully successful apply (03 §2.3): SyncSeen is set, Generation++, the
// source's Hashes become a copy of hashes and its tunnels are rebuilt in
// topology order with idle monitors (Mapping.SourceID = sourceID). The tunnels of the other sources (with
// their monitor state), the balancer monitor and the balancer selection are
// kept. The target is created if it does not exist; the *Target stays the
// same object.
func (m *Memory) ReplaceSourceTopology(key, sourceID string, topo topology.Topology, hashes map[apply.Kind]string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.targets[key]
	if !ok {
		t = newTarget()
		m.targets[key] = t
	}
	t.SyncSeen = true
	src := &Source{Hashes: maps.Clone(hashes)}
	if old := t.Sources[sourceID]; old != nil {
		// Quarantine and no_replacement survive; the hysteresis starts over.
		src.Selection = selection.State{Quarantine: old.Selection.Quarantine}
		src.NoReplacement = old.NoReplacement
	}
	if src.Hashes == nil {
		src.Hashes = map[apply.Kind]string{}
	}
	topo.Tunnels = slices.Clone(topo.Tunnels)
	for i := range topo.Tunnels {
		topo.Tunnels[i].SourceID = sourceID
	}
	src.Topology = &topo
	t.Sources[sourceID] = src
	fresh := make([]*Tunnel, 0, len(topo.Tunnels))
	for _, tn := range topo.Tunnels {
		fresh = append(fresh, &Tunnel{Mapping: tn, Monitor: TunnelMonitor{State: TunnelIdle}})
	}
	t.replaceTunnels(sourceID, fresh)
}

// ReplaceSourceTunnel puts next in place of tunnel oldID of source
// sourceID (same slot, a new idle monitor), drops the tunnel when next is
// nil (the slot is empty), or adds next to an empty slot when oldID is "".
// The source's tunnels stay in port (slot) order, its hashes are
// recomputed for the new tunnels (apply.TopologyPlan) and oldID leaves
// NoReplacement. Generation++. It must run inside Update.
func (t *Target) ReplaceSourceTunnel(sourceID, oldID string, next *topology.Tunnel) {
	src := t.Sources[sourceID]
	if src == nil || src.Topology == nil {
		return
	}
	byID := map[string]*Tunnel{}
	for _, tn := range t.Tunnels {
		if tn.Mapping.SourceID == sourceID {
			byID[tn.Mapping.BaseTunnelID] = tn
		}
	}
	var tunnels []topology.Tunnel
	for _, tn := range src.Topology.Tunnels {
		if oldID == "" || tn.BaseTunnelID != oldID {
			tunnels = append(tunnels, tn)
		}
	}
	if next != nil {
		nt := *next
		nt.SourceID = sourceID
		tunnels = append(tunnels, nt)
		byID[nt.BaseTunnelID] = &Tunnel{Mapping: nt, Monitor: TunnelMonitor{State: TunnelIdle}}
	}
	slices.SortStableFunc(tunnels, func(a, b topology.Tunnel) int { return a.Port - b.Port })
	src.Topology = &topology.Topology{Tunnels: tunnels}
	for k := range src.Hashes {
		if p, err := apply.TopologyPlan(k, sourceID, *src.Topology); err == nil {
			src.Hashes[k] = p.ManifestHash
		}
	}
	delete(src.NoReplacement, oldID)

	fresh := make([]*Tunnel, 0, len(tunnels))
	for _, tn := range tunnels {
		fresh = append(fresh, byID[tn.BaseTunnelID])
	}
	t.replaceTunnels(sourceID, fresh)
}

// DropSourceTunnels forgets the topology and tunnels of source sourceID
// and resets its hashes, keeping its selection state: after a failed full
// apply Xray no longer matches them, so nothing may act on them until the
// next full apply. It must run inside Update.
func (t *Target) DropSourceTunnels(sourceID string) {
	src := t.Sources[sourceID]
	if src == nil {
		return
	}
	src.Topology = &topology.Topology{}
	src.Hashes = map[apply.Kind]string{}
	t.replaceTunnels(sourceID, nil)
}

// ResetSourceHashes empties the hashes of source sourceID: its next sync
// applies it in full. It must run inside Update.
func (t *Target) ResetSourceHashes(sourceID string) {
	if src := t.Sources[sourceID]; src != nil {
		src.Hashes = map[apply.Kind]string{}
	}
}

// DropSource forgets source sourceID of key (its resources are gone from
// Xray): its hashes, topology and tunnels. Generation++ when it existed.
func (m *Memory) DropSource(key, sourceID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.targets[key]
	if !ok {
		return
	}
	if _, ok := t.Sources[sourceID]; !ok {
		return
	}
	delete(t.Sources, sourceID)
	t.replaceTunnels(sourceID, nil)
}

// replaceTunnels replaces the tunnels of sourceID with fresh and keeps the
// order by source id; Generation++.
func (t *Target) replaceTunnels(sourceID string, fresh []*Tunnel) {
	t.Generation++
	out := make([]*Tunnel, 0, len(t.Tunnels)+len(fresh))
	inserted := false
	for _, tn := range t.Tunnels {
		id := tn.Mapping.SourceID
		if id == sourceID {
			continue
		}
		if !inserted && id > sourceID {
			out, inserted = append(out, fresh...), true
		}
		out = append(out, tn)
	}
	if !inserted {
		out = append(out, fresh...)
	}
	t.Tunnels = out
}

// SourceIDs returns the ids of the sources in t, sorted.
func (t *Target) SourceIDs() []string {
	return slices.Sorted(maps.Keys(t.Sources))
}

// Keys returns the target keys, sorted.
func (m *Memory) Keys() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return slices.Sorted(maps.Keys(m.targets))
}

// LockTarget acquires the mutation lock of key (03 §1.1) and returns its
// release. It is not reentrant: locking the same key twice in one call
// chain deadlocks.
func (m *Memory) LockTarget(key string) (unlock func()) {
	m.locksMu.Lock()
	l, ok := m.locks[key]
	if !ok {
		l = &sync.Mutex{}
		m.locks[key] = l
	}
	m.locksMu.Unlock()
	l.Lock()
	return l.Unlock
}

// TryStartSync marks a sync as running; it returns false when one already
// is (the overlap guard, 03 §0 p.1).
func (m *Memory) TryStartSync() bool { return m.syncing.CompareAndSwap(false, true) }

// EndSync clears the running-sync mark.
func (m *Memory) EndSync() { m.syncing.Store(false) }
