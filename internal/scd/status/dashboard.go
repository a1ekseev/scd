package status

import (
	"cmp"
	"embed"
	"html/template"
	"io"
	"strconv"
	"strings"

	"github.com/a1ekseev/scd/internal/scd/balancer"
	"github.com/a1ekseev/scd/internal/scd/state"
)

// web holds the dashboard template and its CSS and JS, copied verbatim from
// the TS dashboard (03 §6.6, D3).
//
//go:embed web/*
var web embed.FS

// dashboardTemplate is the HTML dashboard; html/template escapes every
// value (Review Focus 5).
var dashboardTemplate = template.Must(template.New("dashboard.html.tmpl").Funcs(template.FuncMap{
	"stateClass":             stateClass,
	"formatLastCheck":        formatLastCheck,
	"formatBalancerStatus":   formatBalancerStatus,
	"formatRemotePushStatus": formatRemotePushStatus,
	"metricClass":            metricClass,
	"metric":                 func(label, value string) metricView { return metricView{Label: label, Value: value} },
	"selectionCard":          selectionCard,
	"sourcesCard":            sourcesCard,
	"tunnelCards":            tunnelCards,
	"orIdle":                 func(s string) string { return cmp.Or(s, state.BalancerIdle) },
	"orDash":                 func(s string) string { return cmp.Or(s, "-") },
}).ParseFS(web, "web/dashboard.html.tmpl"))

// dashboardCSS and dashboardJS are inlined into the page, as in TS: the
// handler serves the dashboard on every non-API path, so the page cannot
// load assets by relative URL.
var dashboardCSS, dashboardJS = mustRead("web/dashboard.css"), mustRead("web/dashboard.js")

// selectionCSS styles the balancer selection view (D22); it is inlined only
// when a target has a selection, so pages without one stay as in TS (D3).
var selectionCSS = mustRead("web/selection.css")

func mustRead(name string) string {
	b, err := web.ReadFile(name)
	if err != nil {
		panic(err)
	}
	return strings.TrimRight(string(b), "\n")
}

// dashboardData is the template input.
type dashboardData struct {
	CSS       template.CSS
	JS        template.JS
	Instances []instanceView
}

// instanceView is one instance tab: the local instance first, then the
// remotes. Snapshot is shared with the aggregator and only read.
type instanceView struct {
	ID, Source, Freshness string
	Snapshot              *InstanceSnapshot
}

// metricView is one metric pill; Title, when set, becomes its tooltip.
type metricView struct{ Label, Value, Title string }

// RenderDashboard writes the HTML status dashboard of agg (03 §6.6): one
// tab per instance (the local one, if stored, first and active), each with
// its subscription → target → balancer-first cards. CSS and JS are inlined;
// the page has no external resources. The snapshots of agg are not
// modified.
func RenderDashboard(w io.Writer, agg AggregatedView) error {
	css := dashboardCSS
	if hasSelection(agg) {
		css += "\n" + selectionCSS
	}
	data := dashboardData{CSS: template.CSS(css), JS: template.JS(dashboardJS)}
	if agg.Local.CapturedAt != "" {
		local := agg.Local
		data.Instances = append(data.Instances, instanceView{
			ID: local.InstanceID, Source: "local", Freshness: freshness("", local.CapturedAt), Snapshot: &local,
		})
	}
	for _, r := range agg.Remotes {
		data.Instances = append(data.Instances, instanceView{
			ID: r.ID, Source: "remote", Freshness: freshness(r.LastError, r.LastSuccessAt), Snapshot: r.Snapshot,
		})
	}
	return dashboardTemplate.Execute(w, data)
}

// freshness is the collection status line of an instance; an error wins
// over an older success.
func freshness(lastError, lastSuccessAt string) string {
	switch {
	case lastError != "":
		return "Last collection failed: " + lastError
	case lastSuccessAt != "":
		return "Updated " + lastSuccessAt
	default:
		return "Waiting for first successful collection"
	}
}

// stateClass maps a monitor state to its CSS state; unknown states are idle.
func stateClass(s string) string {
	switch s {
	case state.TunnelHealthy, state.TunnelDegraded, state.TunnelRepairing:
		return s
	default:
		return state.TunnelIdle
	}
}

// formatLatency is "<n> ms", "-" when unmeasured.
func formatLatency(ms *int) string {
	if ms == nil {
		return "-"
	}
	return strconv.Itoa(*ms) + " ms"
}

// formatLastCheck is the latency, else the error, else "-".
func formatLastCheck(latency *int, lastError string) string {
	switch {
	case latency != nil:
		return formatLatency(latency)
	case lastError != "":
		return "Error: " + lastError
	default:
		return "-"
	}
}

// formatBalancerStatus is active, removed or n/a.
func formatBalancerStatus(balanced *bool) string {
	switch {
	case balanced == nil:
		return "n/a"
	case *balanced:
		return "active"
	default:
		return "removed"
	}
}

// formatRemotePushStatus is the remote ping delivery state, followed by the
// reported status when there is one.
func formatRemotePushStatus(pingState, reported string) string {
	delivery := "idle"
	switch pingState {
	case "ok":
		delivery = "delivered"
	case "failed", "pending":
		delivery = pingState
	}
	if reported != "" {
		return delivery + " " + reported
	}
	return delivery
}

// metricClass is the extra CSS class of a metric pill (with a leading
// space), "" for metrics without one.
func metricClass(label, value string) string {
	switch label {
	case "State":
		return " metric-state-" + stateClass(value)
	case "Balancer":
		switch value {
		case "active":
			return " metric-balancer-active"
		case "removed":
			return " metric-balancer-removed"
		default:
			return " metric-balancer-na"
		}
	case "Observatory":
		switch {
		case strings.HasPrefix(value, balancer.ObservatoryAlive):
			return " metric-observatory-alive"
		case value == balancer.ObservatoryDead:
			return " metric-observatory-dead"
		case value == balancer.ObservatoryAwaiting:
			return " metric-observatory-awaiting"
		case value == balancer.ObservatoryNA, value == "unavailable":
			return " metric-observatory-na"
		default:
			return ""
		}
	case "Error":
		return " metric-state-degraded"
	case "Warning":
		return " metric-state-repairing"
	case "Remote Push":
		switch {
		case strings.Contains(value, "delivered up"):
			return " metric-remote-up"
		case strings.Contains(value, "delivered down"), strings.Contains(value, "failed"):
			return " metric-remote-down"
		case strings.Contains(value, "pending"):
			return " metric-remote-pending"
		default:
			return " metric-remote-idle"
		}
	default:
		return ""
	}
}

// hasSelection reports whether any target of agg has a balancer selection.
func hasSelection(agg AggregatedView) bool {
	snaps := []*InstanceSnapshot{&agg.Local}
	for _, r := range agg.Remotes {
		snaps = append(snaps, r.Snapshot)
	}
	for _, s := range snaps {
		if s == nil {
			continue
		}
		for _, sub := range s.Subscriptions {
			for _, t := range sub.Targets {
				if t.BalancerSelection != nil {
					return true
				}
			}
		}
	}
	return false
}

// selectionCardView is the "Xray selection" card of a target (10 §6.7).
type selectionCardView struct {
	// Class is healthy, degraded or idle.
	Class       string
	BalancerTag string
	Metrics     []metricView
}

// selectionCard is the selection card of t, nil without a selection.
func selectionCard(t GroupedStatusTarget) *selectionCardView {
	v := t.BalancerSelection
	if v == nil {
		return nil
	}
	latency := "-"
	if v.SelectedDelayMs != nil {
		latency = strconv.FormatInt(*v.SelectedDelayMs, 10) + " ms"
	}
	obs, obsTitle := observatorySummary(v, t.Tunnels)
	c := &selectionCardView{
		Class:       selectionClass(v, t.Tunnels),
		BalancerTag: v.BalancerTag,
		Metrics: []metricView{
			{Label: "Selected", Value: selectedText(v)},
			{Label: "Latency", Value: latency},
			{Label: "Since", Value: cmp.Or(v.SelectedSince, "-")},
			{Label: "Observatory", Value: obs, Title: obsTitle},
		},
	}
	if v.State == state.SelectionError {
		c.Metrics = append(c.Metrics, metricView{Label: "Error", Value: v.LastError})
	}
	return c
}

// selectedText is the Selected metric: the tunnel ("<country> · <name>")
// or the raw tag of an unmanaged outbound, "override → <tag>", "pool (<n>)"
// with the strategy hint, or "none (fallback/default outbound)"; while the
// check fails the last value is marked stale.
func selectedText(v *BalancerSelectionView) string {
	var s string
	switch state.Mode(v.Mode) {
	case state.ModeOverride:
		s = "override → " + v.SelectedOutboundTag
	case state.ModeSingle:
		s = v.SelectedOutboundTag
		if v.SelectedTunnelID != "" {
			s = cmp.Or(v.SelectedCountryISO2, "-") + " · " + v.SelectedDisplayName
		}
	case state.ModePool:
		s = "pool (" + strconv.Itoa(len(v.PrincipleTargets)) + ")"
		if v.Strategy != "" {
			s += " · " + v.Strategy
		}
	case state.ModeNone:
		s = "none (fallback/default outbound)"
	case state.ModeUnknown:
		s = "unknown"
	default:
		return "-"
	}
	if v.State == state.SelectionError {
		s += " (stale)"
	}
	return s
}

// observatorySummary is the Observatory metric of the card: "<alive>/<n>
// alive" over the tunnels the observatory can see, "unavailable" with the
// error as tooltip, "n/a" when no tunnel is observable and "-" before the
// first check.
func observatorySummary(v *BalancerSelectionView, tunnels []StatusTunnel) (value, title string) {
	if v.ObservatoryError != "" {
		return "unavailable", v.ObservatoryError
	}
	if v.State == state.SelectionIdle {
		return "-", ""
	}
	alive, total := 0, 0
	for _, tn := range tunnels {
		switch tn.ObservatoryStatus {
		case balancer.ObservatoryAlive:
			alive++
			total++
		case balancer.ObservatoryDead, balancer.ObservatoryAwaiting:
			total++
		}
	}
	if total == 0 {
		return balancer.ObservatoryNA, ""
	}
	return strconv.Itoa(alive) + "/" + strconv.Itoa(total) + " alive", ""
}

// selectionClass is the card state (10 §6.7, D22 p.5): degraded on a failed
// check, without alive candidates, for an override outside the target's
// tunnels and for a selected tunnel observed dead; idle before the first
// check and for an unknown principle target; healthy otherwise.
func selectionClass(v *BalancerSelectionView, tunnels []StatusTunnel) string {
	mode := state.Mode(v.Mode)
	switch {
	case v.State == state.SelectionError || (v.State == state.SelectionOK && mode == state.ModeNone):
		return state.TunnelDegraded
	case v.State != state.SelectionOK || mode == state.ModeUnknown:
		return state.TunnelIdle
	case mode == state.ModeOverride && (v.SelectedManaged == nil || !*v.SelectedManaged):
		return state.TunnelDegraded
	}
	for _, tn := range tunnels {
		if tn.BalancerSelected != nil && *tn.BalancerSelected && tn.ObservatoryStatus == balancer.ObservatoryDead {
			return state.TunnelDegraded
		}
	}
	return state.TunnelHealthy
}

// sourcesCardView is the "Sources" card of a target with sources.
type sourcesCardView struct {
	// Class is repairing with a source warning, else idle.
	Class   string
	Title   string
	Metrics []metricView
}

// sourcesCard is the sources card of t, nil without sources: a metric per
// source ("<id>": "<type>", "· <active>/<maxActive> active of
// <candidates>" for wireguard, "· disabled" when off), followed by its
// warnings (the code as tooltip).
func sourcesCard(t GroupedStatusTarget) *sourcesCardView {
	if len(t.Sources) == 0 {
		return nil
	}
	c := &sourcesCardView{Class: state.TunnelIdle, Title: strconv.Itoa(len(t.Sources)) + " source(s)"}
	for _, s := range t.Sources {
		v := s.Type
		if s.MaxActive > 0 {
			v += " · " + strconv.Itoa(s.ActiveCount) + "/" + strconv.Itoa(s.MaxActive) + " active of " + strconv.Itoa(s.CandidateCount)
		}
		if !s.Enabled {
			v += " · disabled"
		}
		c.Metrics = append(c.Metrics, metricView{Label: s.SourceID, Value: v})
		for _, w := range s.Warnings {
			c.Class = state.TunnelRepairing
			c.Metrics = append(c.Metrics, metricView{Label: "Warning", Value: w.Message, Title: s.SourceID + ": " + w.Code})
		}
	}
	return c
}

// tunnelCard is a tunnel of the dashboard with its balancer selection
// decoration (only when the target has a polled selection).
type tunnelCard struct {
	StatusTunnel
	Selected    bool
	Badge       string
	Observatory *metricView
}

// tunnelCards returns the tunnels of t in order with their selection badge
// (Selected, or Pool for a candidate in pool mode) and Observatory metric.
func tunnelCards(t GroupedStatusTarget) []tunnelCard {
	cards := make([]tunnelCard, 0, len(t.Tunnels))
	for _, tn := range t.Tunnels {
		c := tunnelCard{StatusTunnel: tn}
		if v := t.BalancerSelection; v != nil && tn.ObservatoryStatus != "" {
			c.Selected = tn.BalancerSelected != nil && *tn.BalancerSelected
			switch {
			case c.Selected:
				c.Badge = "Selected"
			case state.Mode(v.Mode) == state.ModePool && tn.BalancerCandidate != nil && *tn.BalancerCandidate:
				c.Badge = "Pool"
			}
			m := metricView{Label: "Observatory", Value: tn.ObservatoryStatus}
			switch tn.ObservatoryStatus {
			case balancer.ObservatoryAlive:
				if tn.ObservatoryDelayMs != nil {
					m.Value += " · " + strconv.FormatInt(*tn.ObservatoryDelayMs, 10) + " ms"
				}
			case balancer.ObservatoryDead:
				m.Title = tn.ObservatoryLastError
			}
			c.Observatory = &m
		}
		cards = append(cards, c)
	}
	return cards
}
