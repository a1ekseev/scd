// Package status is the status layer of scd (03 §6–§7): snapshots of the
// tunnel and balancer monitor state, their grouping by subscription and
// target, the aggregation with remote scd instances, the runtime-state
// diagnostic and the HTTP handler of the JSON API and dashboard. The JSON
// API is a contract (D3): field names and schema follow the TS version, new
// fields are additive only; key order follows the Go structs (D23).
package status

import (
	"cmp"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/a1ekseev/scd/internal/scd/config"
	"github.com/a1ekseev/scd/internal/scd/state"
	"github.com/a1ekseev/scd/internal/scd/topology"
)

// SnapshotVersion is the version of InstanceSnapshot (D3: stays 1).
const SnapshotVersion = 1

// timeLayout is the JSON timestamp format (03 §1).
const timeLayout = "2006-01-02T15:04:05.000Z"

// formatTime formats t as a JSON timestamp in UTC.
func formatTime(t time.Time) string { return t.UTC().Format(timeLayout) }

// formatOptTime formats t, "" when nil.
func formatOptTime(t *time.Time) string {
	if t == nil {
		return ""
	}
	return formatTime(*t)
}

// StatusTunnel is one tunnel of /api/status (TS StatusSnapshotTunnel,
// 03 §6.4). Optional fields are omitted when unset.
//
//nolint:revive // the name is the plan interface (task 6.4), used by 6.5/6.6
type StatusTunnel struct {
	SubscriptionID string `json:"subscriptionId"`
	TargetAddress  string `json:"targetAddress"`
	DisplayName    string `json:"displayName"`
	CountryISO2    string `json:"countryIso2,omitempty"`
	Endpoint       string `json:"endpoint"`
	// State is healthy, degraded, repairing or idle.
	State                 string `json:"state"`
	LastHTTPStatus        *int   `json:"lastHttpStatus,omitempty"`
	LastLatencyMs         *int   `json:"lastLatencyMs,omitempty"`
	LastError             string `json:"lastError,omitempty"`
	LastCheckedAt         string `json:"lastCheckedAt,omitempty"`
	LastSuccessAt         string `json:"lastSuccessAt,omitempty"`
	LastFailureAt         string `json:"lastFailureAt,omitempty"`
	LastSuccessHTTPStatus *int   `json:"lastSuccessHttpStatus,omitempty"`
	LastSuccessLatencyMs  *int   `json:"lastSuccessLatencyMs,omitempty"`
	// Balanced reports whether the tunnel's prefixed outbound is current
	// (03 §4.4); nil when it cannot be told.
	Balanced *bool `json:"balanced,omitempty"`

	BalancerMonitorState                        string `json:"balancerMonitorState,omitempty"`
	BalancerMonitorLastStatusCode               *int   `json:"balancerMonitorLastStatusCode,omitempty"`
	BalancerMonitorLastLatencyMs                *int   `json:"balancerMonitorLastLatencyMs,omitempty"`
	BalancerMonitorLastError                    string `json:"balancerMonitorLastError,omitempty"`
	BalancerMonitorLastCheckedAt                string `json:"balancerMonitorLastCheckedAt,omitempty"`
	BalancerMonitorLastSuccessAt                string `json:"balancerMonitorLastSuccessAt,omitempty"`
	BalancerMonitorLastFailureAt                string `json:"balancerMonitorLastFailureAt,omitempty"`
	BalancerMonitorLastSuccessStatusCode        *int   `json:"balancerMonitorLastSuccessStatusCode,omitempty"`
	BalancerMonitorLastSuccessLatencyMs         *int   `json:"balancerMonitorLastSuccessLatencyMs,omitempty"`
	BalancerMonitorRemotePingState              string `json:"balancerMonitorRemotePingState,omitempty"`
	BalancerMonitorRemotePingLastRunAt          string `json:"balancerMonitorRemotePingLastRunAt,omitempty"`
	BalancerMonitorRemotePingLastSuccessAt      string `json:"balancerMonitorRemotePingLastSuccessAt,omitempty"`
	BalancerMonitorRemotePingLastFailureAt      string `json:"balancerMonitorRemotePingLastFailureAt,omitempty"`
	BalancerMonitorRemotePingLastStatusCode     *int   `json:"balancerMonitorRemotePingLastStatusCode,omitempty"`
	BalancerMonitorRemotePingLastError          string `json:"balancerMonitorRemotePingLastError,omitempty"`
	BalancerMonitorRemotePingLastReportedStatus string `json:"balancerMonitorRemotePingLastReportedStatus,omitempty"`
	BalancerMonitorRemotePingLastReportedMsg    string `json:"balancerMonitorRemotePingLastReportedMsg,omitempty"`
	BalancerMonitorRemotePingLastReportedPingMs *int   `json:"balancerMonitorRemotePingLastReportedPingMs,omitempty"`
}

// BalancerMonitorView is the balancer monitor of a grouped target (TS
// GroupedStatusTarget.balancerMonitor, without consecutiveFailures).
type BalancerMonitorView struct {
	// State is idle, healthy or degraded.
	State                        string `json:"state"`
	LastStatusCode               *int   `json:"lastStatusCode,omitempty"`
	LastLatencyMs                *int   `json:"lastLatencyMs,omitempty"`
	LastError                    string `json:"lastError,omitempty"`
	LastCheckedAt                string `json:"lastCheckedAt,omitempty"`
	LastSuccessAt                string `json:"lastSuccessAt,omitempty"`
	LastFailureAt                string `json:"lastFailureAt,omitempty"`
	LastSuccessStatusCode        *int   `json:"lastSuccessStatusCode,omitempty"`
	LastSuccessLatencyMs         *int   `json:"lastSuccessLatencyMs,omitempty"`
	RemotePingState              string `json:"remotePingState,omitempty"`
	RemotePingLastRunAt          string `json:"remotePingLastRunAt,omitempty"`
	RemotePingLastSuccessAt      string `json:"remotePingLastSuccessAt,omitempty"`
	RemotePingLastFailureAt      string `json:"remotePingLastFailureAt,omitempty"`
	RemotePingLastStatusCode     *int   `json:"remotePingLastStatusCode,omitempty"`
	RemotePingLastError          string `json:"remotePingLastError,omitempty"`
	RemotePingLastReportedStatus string `json:"remotePingLastReportedStatus,omitempty"`
	RemotePingLastReportedMsg    string `json:"remotePingLastReportedMsg,omitempty"`
	RemotePingLastReportedPingMs *int   `json:"remotePingLastReportedPingMs,omitempty"`
}

// GroupedStatusTarget is one target of a grouped subscription; its tunnels
// are sorted by latency (idle or unmeasured last), then display name.
type GroupedStatusTarget struct {
	SubscriptionID  string              `json:"subscriptionId"`
	TargetAddress   string              `json:"targetAddress"`
	Tunnels         []StatusTunnel      `json:"tunnels"`
	BalancerMonitor BalancerMonitorView `json:"balancerMonitor"`
}

// GroupedSubscription is a subscription with its targets, sorted by address.
type GroupedSubscription struct {
	SubscriptionID string                `json:"subscriptionId"`
	Targets        []GroupedStatusTarget `json:"targets"`
}

// InstanceSnapshot is /api/instance-status (TS InstanceStatusSnapshot):
// the grouped view plus the flat tunnel list.
type InstanceSnapshot struct {
	Version       int                   `json:"version"`
	InstanceID    string                `json:"instanceId"`
	CapturedAt    string                `json:"capturedAt"`
	Subscriptions []GroupedSubscription `json:"subscriptions"`
	Tunnels       []StatusTunnel        `json:"tunnels"`
}

// RemoteView is the collection state of one remote instance for the
// dashboard. Snapshot is nil until the first successful collection.
type RemoteView struct {
	ID, APIURL                              string
	LastAttemptAt, LastSuccessAt, LastError string
	Snapshot                                *InstanceSnapshot
}

// AggregatedView is what the dashboard shows (03 §6.6): the local snapshot
// and the remote instances sorted by id.
type AggregatedView struct {
	Local   InstanceSnapshot
	Remotes []RemoteView
}

// Balanced reports whether t still uses its prefixed (balancer) outbound
// (03 §4.4); nil when there is no prefix to tell.
func Balanced(t topology.Tunnel) *bool {
	unprefixed := cmp.Or(t.OutboundWithoutPrefix.Tag, t.BaseOutboundTag)
	if unprefixed == "" || t.OutboundTagInitial == unprefixed {
		return nil
	}
	prefix, ok := strings.CutSuffix(t.OutboundTagInitial, unprefixed)
	if !ok || prefix == "" {
		return nil
	}
	b := strings.HasPrefix(t.OutboundTagCurrent, prefix)
	return &b
}

// clone returns a copy of p.
func clone[T any](p *T) *T {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

// BuildTunnels returns the flat tunnel list of every target in m, sorted by
// subscription id, target address and display name (strings.Compare, D23).
// Only targets with state (after a successful sync) contribute.
func BuildTunnels(m *state.Memory) []StatusTunnel {
	tunnels := []StatusTunnel{}
	for _, key := range m.Keys() {
		subID, addr, ok := strings.Cut(key, "::")
		if !ok {
			addr = key
		}
		m.View(key, func(t *state.Target) {
			for _, tn := range t.Tunnels {
				tunnels = append(tunnels, statusTunnel(subID, addr, tn, &t.Balancer))
			}
		})
	}
	slices.SortStableFunc(tunnels, func(a, b StatusTunnel) int {
		return cmp.Or(strings.Compare(a.SubscriptionID, b.SubscriptionID),
			strings.Compare(a.TargetAddress, b.TargetAddress),
			strings.Compare(a.DisplayName, b.DisplayName))
	})
	return tunnels
}

// statusTunnel copies the state of tn and its target's balancer monitor.
func statusTunnel(subID, addr string, tn *state.Tunnel, b *state.BalancerMonitor) StatusTunnel {
	mon, mp := tn.Monitor, tn.Mapping
	return StatusTunnel{
		SubscriptionID: subID, TargetAddress: addr,
		DisplayName: mp.DisplayName, CountryISO2: mp.CountryISO2,
		Endpoint: mp.Listen + ":" + strconv.Itoa(mp.Port),
		State:    mon.State,

		LastHTTPStatus: clone(mon.LastStatusCode), LastLatencyMs: clone(mon.LastLatencyMs), LastError: mon.LastError,
		LastCheckedAt: formatOptTime(mon.LastCheckedAt), LastSuccessAt: formatOptTime(mon.LastSuccessAt), LastFailureAt: formatOptTime(mon.LastFailureAt),
		LastSuccessHTTPStatus: clone(mon.LastSuccessStatusCode), LastSuccessLatencyMs: clone(mon.LastSuccessLatencyMs),
		Balanced: Balanced(mp),

		BalancerMonitorState:                        b.State,
		BalancerMonitorLastStatusCode:               clone(b.LastStatusCode),
		BalancerMonitorLastLatencyMs:                clone(b.LastLatencyMs),
		BalancerMonitorLastError:                    b.LastError,
		BalancerMonitorLastCheckedAt:                formatOptTime(b.LastCheckedAt),
		BalancerMonitorLastSuccessAt:                formatOptTime(b.LastSuccessAt),
		BalancerMonitorLastFailureAt:                formatOptTime(b.LastFailureAt),
		BalancerMonitorLastSuccessStatusCode:        clone(b.LastSuccessStatusCode),
		BalancerMonitorLastSuccessLatencyMs:         clone(b.LastSuccessLatencyMs),
		BalancerMonitorRemotePingState:              b.RemotePingState,
		BalancerMonitorRemotePingLastRunAt:          formatOptTime(b.RemotePingLastRunAt),
		BalancerMonitorRemotePingLastSuccessAt:      formatOptTime(b.RemotePingLastSuccessAt),
		BalancerMonitorRemotePingLastFailureAt:      formatOptTime(b.RemotePingLastFailureAt),
		BalancerMonitorRemotePingLastStatusCode:     clone(b.RemotePingLastStatusCode),
		BalancerMonitorRemotePingLastError:          b.RemotePingLastError,
		BalancerMonitorRemotePingLastReportedStatus: b.RemotePingLastReportedStatus,
		BalancerMonitorRemotePingLastReportedMsg:    b.RemotePingLastReportedMsg,
		BalancerMonitorRemotePingLastReportedPingMs: clone(b.RemotePingLastReportedPingMs),
	}
}

// balancerViewOf is the balancer monitor of a group taken from the
// balancerMonitor* fields of its first (unsorted) tunnel.
func balancerViewOf(t StatusTunnel) BalancerMonitorView {
	return BalancerMonitorView{
		State:                        cmp.Or(t.BalancerMonitorState, state.BalancerIdle),
		LastStatusCode:               t.BalancerMonitorLastStatusCode,
		LastLatencyMs:                t.BalancerMonitorLastLatencyMs,
		LastError:                    t.BalancerMonitorLastError,
		LastCheckedAt:                t.BalancerMonitorLastCheckedAt,
		LastSuccessAt:                t.BalancerMonitorLastSuccessAt,
		LastFailureAt:                t.BalancerMonitorLastFailureAt,
		LastSuccessStatusCode:        t.BalancerMonitorLastSuccessStatusCode,
		LastSuccessLatencyMs:         t.BalancerMonitorLastSuccessLatencyMs,
		RemotePingState:              t.BalancerMonitorRemotePingState,
		RemotePingLastRunAt:          t.BalancerMonitorRemotePingLastRunAt,
		RemotePingLastSuccessAt:      t.BalancerMonitorRemotePingLastSuccessAt,
		RemotePingLastFailureAt:      t.BalancerMonitorRemotePingLastFailureAt,
		RemotePingLastStatusCode:     t.BalancerMonitorRemotePingLastStatusCode,
		RemotePingLastError:          t.BalancerMonitorRemotePingLastError,
		RemotePingLastReportedStatus: t.BalancerMonitorRemotePingLastReportedStatus,
		RemotePingLastReportedMsg:    t.BalancerMonitorRemotePingLastReportedMsg,
		RemotePingLastReportedPingMs: t.BalancerMonitorRemotePingLastReportedPingMs,
	}
}

// balancerView is the balancer monitor state b without consecutiveFailures.
func balancerView(b *state.BalancerMonitor) BalancerMonitorView {
	return BalancerMonitorView{
		State:                        b.State,
		LastStatusCode:               clone(b.LastStatusCode),
		LastLatencyMs:                clone(b.LastLatencyMs),
		LastError:                    b.LastError,
		LastCheckedAt:                formatOptTime(b.LastCheckedAt),
		LastSuccessAt:                formatOptTime(b.LastSuccessAt),
		LastFailureAt:                formatOptTime(b.LastFailureAt),
		LastSuccessStatusCode:        clone(b.LastSuccessStatusCode),
		LastSuccessLatencyMs:         clone(b.LastSuccessLatencyMs),
		RemotePingState:              b.RemotePingState,
		RemotePingLastRunAt:          formatOptTime(b.RemotePingLastRunAt),
		RemotePingLastSuccessAt:      formatOptTime(b.RemotePingLastSuccessAt),
		RemotePingLastFailureAt:      formatOptTime(b.RemotePingLastFailureAt),
		RemotePingLastStatusCode:     clone(b.RemotePingLastStatusCode),
		RemotePingLastError:          b.RemotePingLastError,
		RemotePingLastReportedStatus: b.RemotePingLastReportedStatus,
		RemotePingLastReportedMsg:    b.RemotePingLastReportedMsg,
		RemotePingLastReportedPingMs: clone(b.RemotePingLastReportedPingMs),
	}
}

// latencyKey orders tunnels by latency; idle or unmeasured tunnels last.
func latencyKey(t StatusTunnel) float64 {
	if t.State == state.TunnelIdle || t.LastLatencyMs == nil {
		return math.Inf(1)
	}
	return float64(*t.LastLatencyMs)
}

// compareLatencyThenName is the tunnel order inside a target (03 §6.4).
func compareLatencyThenName(a, b StatusTunnel) int {
	return cmp.Or(cmp.Compare(latencyKey(a), latencyKey(b)), strings.Compare(a.DisplayName, b.DisplayName))
}

// GroupTunnels groups a flat tunnel list by subscription and target (TS
// groupStatusSnapshot): subscriptions and targets sorted by
// strings.Compare, tunnels by latency then name; the balancer monitor of a
// target comes from its first tunnel in input order.
func GroupTunnels(tunnels []StatusTunnel) []GroupedSubscription {
	type key struct{ sub, addr string }
	byTarget := map[key][]StatusTunnel{}
	targetsOf := map[string][]string{}
	for _, t := range tunnels {
		k := key{t.SubscriptionID, t.TargetAddress}
		if _, ok := byTarget[k]; !ok {
			targetsOf[t.SubscriptionID] = append(targetsOf[t.SubscriptionID], t.TargetAddress)
		}
		byTarget[k] = append(byTarget[k], t)
	}

	subs := make([]GroupedSubscription, 0, len(targetsOf))
	for subID, addrs := range targetsOf {
		g := GroupedSubscription{SubscriptionID: subID, Targets: make([]GroupedStatusTarget, 0, len(addrs))}
		for _, addr := range addrs {
			list := byTarget[key{subID, addr}]
			sorted := slices.Clone(list)
			slices.SortStableFunc(sorted, compareLatencyThenName)
			g.Targets = append(g.Targets, GroupedStatusTarget{
				SubscriptionID: subID, TargetAddress: addr, Tunnels: sorted, BalancerMonitor: balancerViewOf(list[0]),
			})
		}
		subs = append(subs, g)
	}
	sortGroups(subs)
	return subs
}

// sortGroups sorts subscriptions by id and their targets by address.
func sortGroups(subs []GroupedSubscription) {
	slices.SortStableFunc(subs, func(a, b GroupedSubscription) int { return strings.Compare(a.SubscriptionID, b.SubscriptionID) })
	for i := range subs {
		slices.SortStableFunc(subs[i].Targets, func(a, b GroupedStatusTarget) int { return strings.Compare(a.TargetAddress, b.TargetAddress) })
	}
}

// BuildConfiguredGroups groups tunnels and makes sure that every enabled
// subscription of cfg and its target are present, even without tunnels or
// memory state (TS buildConfiguredStatusGroups). A configured target's
// balancer monitor is its memory state, else the group's, else idle.
func BuildConfiguredGroups(tunnels []StatusTunnel, cfg config.AppConfig, m *state.Memory) []GroupedSubscription {
	subs := GroupTunnels(tunnels)
	index := make(map[string]int, len(subs))
	for i, s := range subs {
		index[s.SubscriptionID] = i
	}
	for _, c := range cfg.Subscriptions {
		if !c.Enabled {
			continue
		}
		i, ok := index[c.ID]
		if !ok {
			i = len(subs)
			index[c.ID] = i
			subs = append(subs, GroupedSubscription{SubscriptionID: c.ID, Targets: []GroupedStatusTarget{}})
		}
		addr := c.Target.Address
		j := slices.IndexFunc(subs[i].Targets, func(t GroupedStatusTarget) bool { return t.TargetAddress == addr })

		var bm *BalancerMonitorView
		m.View(state.Key(c.ID, addr), func(t *state.Target) {
			v := balancerView(&t.Balancer)
			bm = &v
		})
		if j >= 0 {
			if bm != nil {
				subs[i].Targets[j].BalancerMonitor = *bm
			}
			continue
		}
		if bm == nil {
			bm = &BalancerMonitorView{State: state.BalancerIdle}
		}
		subs[i].Targets = append(subs[i].Targets, GroupedStatusTarget{
			SubscriptionID: c.ID, TargetAddress: addr, Tunnels: []StatusTunnel{}, BalancerMonitor: *bm,
		})
	}
	sortGroups(subs)
	return subs
}

// BuildInstanceSnapshot is the local /api/instance-status snapshot.
func BuildInstanceSnapshot(m *state.Memory, cfg config.AppConfig, instanceID string, now time.Time) InstanceSnapshot {
	tunnels := BuildTunnels(m)
	return InstanceSnapshot{
		Version: SnapshotVersion, InstanceID: instanceID, CapturedAt: formatTime(now),
		Subscriptions: BuildConfiguredGroups(tunnels, cfg, m), Tunnels: tunnels,
	}
}
