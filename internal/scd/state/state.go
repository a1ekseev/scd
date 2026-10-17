// Package state is the in-memory runtime state of scd (03 §1): per target
// the saved manifest hashes, the tunnel topology with the monitor state of
// every tunnel and the balancer monitor state, plus the sync overlap guard
// and the per-target mutation lock shared by sync apply, repair and rejoin
// (03 §1.1).
//
// All fields of a Target are read and written only inside View or Update,
// which hold the state lock; no I/O happens under it.
package state

import (
	"maps"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/a1ekseev/scd/internal/scd/apply"
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

// Target is the state of one subscription target.
type Target struct {
	// Hashes are the manifest hashes of the last fully successful apply.
	Hashes map[apply.Kind]string
	// Generation is incremented on every topology replacement.
	Generation int
	// Topology is nil until the first fully successful apply.
	Topology *topology.Topology
	// Tunnels are in topology order (the monitor relies on it).
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
		Hashes:   map[apply.Kind]string{},
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

// ReplaceTopology installs topo and hashes after a fully successful apply
// (03 §2.3): Generation++, Hashes become a copy of hashes, Tunnels are
// rebuilt in topology order with idle monitors. The balancer monitor and
// the balancer selection are kept. The target is created if it does not exist; the *Target stays the
// same object.
func (m *Memory) ReplaceTopology(key string, topo topology.Topology, hashes map[apply.Kind]string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.targets[key]
	if !ok {
		t = newTarget()
		m.targets[key] = t
	}
	t.Generation++
	t.Hashes = maps.Clone(hashes)
	if t.Hashes == nil {
		t.Hashes = map[apply.Kind]string{}
	}
	topo.Tunnels = slices.Clone(topo.Tunnels)
	t.Topology = &topo
	t.Tunnels = make([]*Tunnel, 0, len(topo.Tunnels))
	for _, tn := range topo.Tunnels {
		t.Tunnels = append(t.Tunnels, &Tunnel{Mapping: tn, Monitor: TunnelMonitor{State: TunnelIdle}})
	}
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
