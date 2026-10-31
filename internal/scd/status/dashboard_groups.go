package status

import (
	"cmp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/a1ekseev/scd/internal/scd/balancer"
	"github.com/a1ekseev/scd/internal/scd/state"
)

// recoveredShown is how long "Recovered · failures reset" stays on a source
// row after the end of its wait (resetAt), relative to the snapshot time.
const recoveredShown = 10 * time.Minute

// targetView is a target of the dashboard: its address, the summary line
// and its balancer groups.
type targetView struct {
	Address, Summary string
	Groups           []groupView
}

// groupView is one balancer group: the head (orb state, title, source
// badges, rows Source / Check / Selection), the tunnel summary and grid.
type groupView struct {
	// Class is the orb state: healthy, degraded, repairing or idle.
	Class   string
	Title   string
	Badges  []string
	Rows    []rowView
	Summary string
	Tunnels []tunnelCard
	// Empty is the text instead of the grid when the group has no tunnel.
	Empty string
}

// rowView is one row of a group head: a label and its pills.
type rowView struct {
	Label   string
	Metrics []metricView
}

// plural is "<n> <word>" with an "s" unless n is 1.
func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return strconv.Itoa(n) + " " + word + "s"
}

// targetCard is the dashboard view of target t of a snapshot captured at
// capturedAt: one group per targets[].balancers[] entry, or, for a snapshot
// without them (an older scd, a config without balancers), one group of the
// target-level balancer monitor and selection.
func targetCard(t GroupedStatusTarget, capturedAt string) targetView {
	now, _ := time.Parse(time.RFC3339, capturedAt)
	v := targetView{Address: t.TargetAddress}
	summary := []string{plural(len(t.Tunnels), "tunnel")}
	if len(t.Balancers) > 0 {
		summary = append(summary, plural(len(t.Balancers), "balancer"))
	}
	if len(t.Sources) > 0 {
		summary = append(summary, plural(len(t.Sources), "source"))
	}
	v.Summary = strings.Join(summary, " · ")

	groups := t.Balancers
	legacy := len(groups) == 0
	if legacy {
		g := BalancerGroupView{Monitor: &t.BalancerMonitor, Selection: t.BalancerSelection}
		if t.BalancerSelection != nil {
			g.BalancerTag = t.BalancerSelection.BalancerTag
		}
		for _, s := range t.Sources {
			g.SourceIDs = append(g.SourceIDs, s.SourceID)
		}
		groups = []BalancerGroupView{g}
	}
	sources := map[string]SourceView{}
	for _, s := range t.Sources {
		sources[s.SourceID] = s
	}
	for i, g := range groups {
		mine := func(tn StatusTunnel) bool {
			return slices.Contains(g.SourceIDs, tn.SourceID) || (tn.SourceID == "" && i == 0)
		}
		v.Groups = append(v.Groups, groupCard(t, g, sources, mine, legacy, now))
	}
	return v
}

// groupCard builds the view of group g of target t.
func groupCard(t GroupedStatusTarget, g BalancerGroupView, sources map[string]SourceView, mine func(StatusTunnel) bool, legacy bool, now time.Time) groupView {
	v := groupView{Title: g.BalancerTag}
	if v.Title == "" && !legacy && len(g.SourceIDs) > 0 {
		v.Title = "Sources without balancer"
	}
	var tunnels []StatusTunnel
	for _, tn := range t.Tunnels {
		if mine(tn) {
			tunnels = append(tunnels, tn)
		}
	}

	paused := map[string]metricView{} // source → the Replace paused pill of its tunnels
	allWaiting, allBroken := len(g.SourceIDs) > 0, len(g.SourceIDs) > 0
	for _, id := range g.SourceIDs {
		s, ok := sources[id]
		if !ok {
			allWaiting = false
			continue
		}
		v.Badges = append(v.Badges, id)
		row, pause := sourceRow(s, countOf(tunnels, id), now)
		v.Rows = append(v.Rows, row)
		if pause != nil {
			paused[id] = *pause
		} else {
			allWaiting = false
		}
		if s.Error == nil && hasLive(tunnels, id) {
			allBroken = false
		}
	}

	monitorData, selectionData := false, false
	if m := g.Monitor; m != nil {
		st := cmp.Or(m.State, state.BalancerIdle)
		monitorData = st != state.BalancerIdle
		v.Rows = append(v.Rows, rowView{Label: "Check", Metrics: []metricView{
			{Label: "State", Value: st},
			{Label: "Last Check", Value: formatLastCheck(m.LastLatencyMs, m.LastError)},
			{Label: "Remote Push", Value: formatRemotePushStatus(m.RemotePingState, m.RemotePingLastReportedStatus)},
		}})
	} else if !legacy {
		v.Rows = append(v.Rows, rowView{Label: "Check", Metrics: []metricView{{Label: "State", Value: "off"}}})
	}
	selClass := ""
	if s := g.Selection; s != nil {
		c := selectionCard(s, tunnels)
		selClass, selectionData = c.Class, c.Class != state.TunnelIdle
		v.Rows = append(v.Rows, rowView{Label: "Selection", Metrics: c.Metrics})
	}

	// The orb follows the balancer itself; the sources decide only without
	// its data.
	switch {
	case monitorData || selectionData:
		v.Class = state.TunnelHealthy
		if (g.Monitor != nil && g.Monitor.State == state.BalancerDegraded) || selClass == state.TunnelDegraded {
			v.Class = state.TunnelDegraded
			if allWaiting {
				v.Class = state.TunnelRepairing
			}
		}
	case allWaiting:
		v.Class = state.TunnelRepairing
	case allBroken:
		v.Class = state.TunnelDegraded
	case len(tunnels) == 0:
		v.Class = state.TunnelIdle
	case len(g.SourceIDs) == 0 && !hasLive(tunnels, ""):
		// No sources to tell: by the tunnels.
		v.Class = state.TunnelIdle
		if slices.ContainsFunc(tunnels, func(tn StatusTunnel) bool {
			return tn.State == state.TunnelDegraded || tn.State == state.TunnelRepairing
		}) {
			v.Class = state.TunnelDegraded
		}
	default:
		v.Class = state.TunnelHealthy
	}

	cards := tunnelCards(g.Selection, tunnels)
	for i := range cards {
		if p, ok := paused[cards[i].SourceID]; ok {
			cards[i].Paused = &p
		}
	}
	// The tunnels of a waiting source go last.
	slices.SortStableFunc(cards, func(a, b tunnelCard) int {
		switch {
		case a.Paused == nil && b.Paused != nil:
			return -1
		case a.Paused != nil && b.Paused == nil:
			return 1
		}
		return 0
	})
	v.Tunnels = cards
	v.Summary = tunnelSummary(tunnels)
	if len(tunnels) == 0 {
		v.Empty = "No tunnel data."
		if len(g.SourceIDs) > 0 && !slices.ContainsFunc(g.SourceIDs, func(id string) bool { return sources[id].Error == nil }) {
			v.Empty = "Source not applied."
		}
	}
	return v
}

// countOf is the number of tunnels of source id.
func countOf(tunnels []StatusTunnel, id string) int {
	n := 0
	for _, tn := range tunnels {
		if tn.SourceID == id {
			n++
		}
	}
	return n
}

// hasLive reports whether a tunnel of source id ("" any) is healthy.
func hasLive(tunnels []StatusTunnel, id string) bool {
	return slices.ContainsFunc(tunnels, func(tn StatusTunnel) bool {
		return (id == "" || tn.SourceID == id) && tn.State == state.TunnelHealthy
	})
}

// tunnelSummary is "<n> tunnels · a healthy · b repairing · c degraded"
// (zero counts left out).
func tunnelSummary(tunnels []StatusTunnel) string {
	if len(tunnels) == 0 {
		return ""
	}
	count := map[string]int{}
	for _, tn := range tunnels {
		count[stateClass(tn.State)]++
	}
	parts := []string{plural(len(tunnels), "tunnel")}
	for _, s := range []string{state.TunnelHealthy, state.TunnelRepairing, state.TunnelDegraded, state.TunnelIdle} {
		if count[s] > 0 {
			parts = append(parts, strconv.Itoa(count[s])+" "+s)
		}
	}
	return strings.Join(parts, " · ")
}

// sourceRow is the Source row of source s with n tunnels: its chip, its
// dialer proxy, its dependencies (Depends on, Waiting for, Recovering), a
// recent end of its wait (Recovered, recoveredShown after resetAt), its error
// and warnings. pause is the Replace paused pill of the tunnels of a waiting
// source ("waiting for <id>" while a dependency is unhealthy, else "until
// <graceUntil>" in the pause), nil when it does not wait.
func sourceRow(s SourceView, n int, now time.Time) (row rowView, pause *metricView) {
	chip := s.Type
	switch {
	case s.MaxActive > 0:
		chip += " · " + strconv.Itoa(s.ActiveCount) + "/" + strconv.Itoa(s.MaxActive) + " active of " + strconv.Itoa(s.CandidateCount)
	default:
		chip += " · " + plural(n, "tunnel")
	}
	class := " source-chip " + sourceColor(s.SourceID)
	if !s.Enabled {
		chip += " · disabled"
		class += " is-off"
	}
	row = rowView{Label: "Source", Metrics: []metricView{{Label: s.SourceID, Value: chip, Class: class}}}
	if s.DialerProxy != "" {
		row.Metrics = append(row.Metrics, metricView{Label: "via", Value: s.DialerProxy, Title: "sockopt.dialerProxy: the outbounds of " + s.SourceID + " dial through this outbound"})
	}
	for _, d := range s.DependsOn {
		title := "health: " + d.Health + " — the balancer of " + d.SourceID + " selects an outbound (GetBalancerInfo)"
		switch d.State {
		case state.DepUnhealthy:
			pause = &metricView{Label: "Replace paused", Value: "waiting for " + d.SourceID}
			m := metricView{
				Label: "Waiting for", Value: d.SourceID + " · " + d.Health + " down", Class: " metric-state-repairing",
				Title: title + ". Its tunnels are only repaired, not replaced.",
			}
			if d.Since != "" {
				m.Value += " since "
				m.Time = d.Since
			}
			row.Metrics = append(row.Metrics, m)
		case state.DepHealthy:
			row.Metrics = append(row.Metrics, metricView{Label: "Depends on", Value: d.SourceID + " · " + d.Health + " · healthy", Class: " metric-state-healthy", Title: title})
			if until, err := time.Parse(time.RFC3339, d.GraceUntil); err == nil && now.Before(until) {
				if pause == nil {
					pause = &metricView{Label: "Replace paused", Value: "until ", Time: d.GraceUntil}
				}
				row.Metrics = append(row.Metrics, metricView{
					Label: "Recovering", Value: "replace paused until ", Time: d.GraceUntil, Class: " metric-state-repairing",
					Title: "recoverGraceSeconds: failed tunnels are only repaired until the pause ends",
				})
			}
		default:
			value := d.SourceID + " · " + d.Health + " · " + strings.ReplaceAll(d.State, "_", " ")
			row.Metrics = append(row.Metrics, metricView{Label: "Depends on", Value: value, Class: " metric-state-idle", Title: title})
		}
	}
	if at, err := time.Parse(time.RFC3339, s.ResetAt); err == nil && pause == nil && !now.Before(at) && now.Sub(at) < recoveredShown {
		row.Metrics = append(row.Metrics, metricView{
			Label: "Recovered", Time: s.ResetAt, After: " · failures reset", Class: " metric-state-healthy",
			Title: "the dependency recovered: the failed checks of the source's tunnels were reset",
		})
	}
	if e := s.Error; e != nil {
		row.Metrics = append(row.Metrics, metricView{Label: "Error", Value: e.Message, Title: s.SourceID + ": " + e.Code})
	}
	for _, w := range s.Warnings {
		row.Metrics = append(row.Metrics, metricView{Label: "Warning", Value: w.Message, Title: s.SourceID + ": " + w.Code})
	}
	return row, pause
}

// protocolLabel is the protocol of a tunnel for the meta line, with the
// transport of a MASQUE tunnel.
func protocolLabel(tn StatusTunnel) string {
	var p string
	switch tn.Protocol {
	case "":
		p = "VLESS"
	case "wireguard":
		p = "WireGuard"
	case "masque":
		p = "MASQUE"
	default:
		p = tn.Protocol
	}
	if tn.Transport != "" {
		p += " " + tn.Transport
	}
	return p
}

// tunnelMeta is the meta line of a tunnel card: country (when known),
// protocol and endpoint.
func tunnelMeta(tn StatusTunnel) string {
	parts := []string{protocolLabel(tn), tn.Endpoint}
	if tn.CountryISO2 != "" {
		parts = append([]string{tn.CountryISO2}, parts...)
	}
	return strings.Join(parts, " · ")
}

// probeTitle and observatoryTitle explain the two latencies of a tunnel.
const (
	probeTitle       = "scd probe: request to the response headers through the tunnel's SOCKS inbound"
	observatoryTitle = "Xray observatory: its own probe of the outbound"
)

// observatoryMetric is the Observatory pill of a tunnel of a group with a
// polled selection.
func observatoryMetric(tn StatusTunnel) metricView {
	m := metricView{Label: "Observatory", Value: tn.ObservatoryStatus, Title: observatoryTitle}
	switch tn.ObservatoryStatus {
	case balancer.ObservatoryAlive:
		if tn.ObservatoryDelayMs != nil {
			m.Value += " · " + strconv.FormatInt(*tn.ObservatoryDelayMs, 10) + " ms"
		}
	case balancer.ObservatoryDead:
		if tn.ObservatoryLastError != "" {
			m.Title = tn.ObservatoryLastError
		}
	}
	return m
}
