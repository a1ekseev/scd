package status

import (
	"cmp"
	"embed"
	"html/template"
	"io"
	"strconv"
	"strings"

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
	"metric":                 func(label, value string) metricView { return metricView{label, value} },
	"orIdle":                 func(s string) string { return cmp.Or(s, state.BalancerIdle) },
	"orDash":                 func(s string) string { return cmp.Or(s, "-") },
}).ParseFS(web, "web/dashboard.html.tmpl"))

// dashboardCSS and dashboardJS are inlined into the page, as in TS: the
// handler serves the dashboard on every non-API path, so the page cannot
// load assets by relative URL.
var dashboardCSS, dashboardJS = mustRead("web/dashboard.css"), mustRead("web/dashboard.js")

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

// metricView is one metric pill.
type metricView struct{ Label, Value string }

// RenderDashboard writes the HTML status dashboard of agg (03 §6.6): one
// tab per instance (the local one, if stored, first and active), each with
// its subscription → target → balancer-first cards. CSS and JS are inlined;
// the page has no external resources. The snapshots of agg are not
// modified.
func RenderDashboard(w io.Writer, agg AggregatedView) error {
	data := dashboardData{CSS: template.CSS(dashboardCSS), JS: template.JS(dashboardJS)}
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
