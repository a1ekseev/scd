package status

import (
	"strings"
	"testing"
	"time"

	"github.com/a1ekseev/scd/internal/scd/state"
)

// groupHTML returns the balancer group of body whose title is tag.
func groupHTML(t *testing.T, body, tag string) string {
	t.Helper()
	i := strings.Index(body, "<h4>"+tag+"</h4>")
	if i < 0 {
		t.Fatalf("no group %s", tag)
	}
	start := strings.LastIndex(body[:i], `<section class="bal-group">`)
	end := strings.Index(body[i:], "</section>")
	return body[start : i+end]
}

// The group orb follows its balancer; waiting, the pause, a recent reset,
// errors and the dialer proxy are on the source row; the tunnels of a
// waiting source say why they are not replaced and go last.
func TestDashboardChain(t *testing.T) {
	down := bodyOf(t, dashboard(t, "chain-upstream-down", "/"))
	assertContains(t, down, "<p>7 tunnels · 3 balancers · 3 sources</p>")
	assertContains(t, groupHTML(t, down, "x-balancer"), `class="bal-head state-degraded"`)
	warp := groupHTML(t, down, "x-warp-balancer")
	assertContains(t, warp, `class="bal-head state-repairing"`,
		`<span class="metric-label">via</span><span class="metric-value">chain-blanc</span>`,
		`<span class="metric-label">Waiting for</span><span class="metric-value">vless · balancer down since <time datetime="2026-10-31T08:26:10.000Z">`,
		`<span class="metric-label">Replace paused</span><span class="metric-value">waiting for vless</span>`,
		`<span class="metric-label">Failures</span><span class="metric-value">4 in row</span>`,
		`<div class="node-meta">MASQUE h3 · 127.0.0.1:21100</div>`, `title="☁️ WARP #1"`)

	rec := groupHTML(t, bodyOf(t, dashboard(t, "chain-recovering", "/")), "x-proton-balancer")
	assertContains(t, rec, `class="bal-head state-healthy"`,
		`<span class="metric-label">Recovering</span><span class="metric-value">replace paused until <time datetime="2026-10-31T08:32:00.000Z">`)
	assertContains(t, rec, `<span class="metric-label">Replace paused</span><span class="metric-value">until <time datetime="2026-10-31T08:32:00.000Z">`)
	assertNotContains(t, rec, "waiting for vless")
	// After graceUntil the pause is over: no Recovering, no Replace paused.
	l := loadFixture(t, "chain-recovering")
	l.now = l.now.Add(3 * time.Minute)
	after := groupHTML(t, bodyOf(t, get(t, NewHandler(l.deps()), "/").Body.String()), "x-proton-balancer")
	assertNotContains(t, after, "Recovering", "Replace paused")

	done := groupHTML(t, bodyOf(t, dashboard(t, "chain-recovered", "/")), "x-warp-balancer")
	assertContains(t, done, `<span class="metric-label">Recovered</span><span class="metric-value"><time datetime="2026-10-31T08:27:00.000Z">2026-10-31T08:27:00.000Z</time> · failures reset</span>`)
	assertNotContains(t, done, "Replace paused", "Waiting for")
	// Ten minutes after the reset the pill is gone.
	l = loadFixture(t, "chain-recovered")
	l.now = l.now.Add(8 * 60e9)
	page := get(t, NewHandler(l.deps()), "/").Body.String()
	assertNotContains(t, bodyOf(t, page), "failures reset")

	errs := bodyOf(t, dashboard(t, "chain-config-error", "/"))
	assertContains(t, groupHTML(t, errs, "x-warp-balancer"), `class="bal-head state-degraded"`, "Source not applied.",
		`title="warp: dialer_proxy_missing"><span class="metric-label">Error</span>`)
	assertContains(t, groupHTML(t, errs, "x-edge-balancer"), `<span class="metric-label">Depends on</span><span class="metric-value">up · balancer · not applied</span>`)
	assertContains(t, groupHTML(t, errs, "x-proton-balancer"), `class="bal-head state-healthy"`, `title="proton: balancer_not_found"`)

	shared := bodyOf(t, dashboard(t, "chain-shared-balancer", "/"))
	vpn := groupHTML(t, shared, "x-vpn-balancer")
	assertContains(t, vpn, `class="bal-head state-healthy"`, `<span class="metric-label">via</span><span class="metric-value">chain-edge</span>`,
		`<span class="metric-label">Waiting for</span>`)
	b, e, w := strings.Index(shared, "<h4>x-balancer</h4>"), strings.Index(shared, "<h4>x-edge-balancer</h4>"), strings.Index(shared, "<h4>x-vpn-balancer</h4>")
	if b > e || e > w {
		t.Errorf("group order x-balancer %d, x-edge-balancer %d, x-vpn-balancer %d", b, e, w)
	}

	// A waiting source's tunnels go last, even a healthy fast one.
	l = loadFixture(t, "chain-shared-balancer")
	l.mem.Update("main::127.0.0.1:10085", func(tg *state.Target) {
		for _, tn := range tg.Tunnels {
			if tn.Mapping.SourceID == "warp" {
				tn.Monitor.State, tn.Monitor.LastLatencyMs = state.TunnelHealthy, new(1)
			}
		}
	})
	vpn = groupHTML(t, bodyOf(t, get(t, NewHandler(l.deps()), "/").Body.String()), "x-vpn-balancer")
	if p, w := strings.Index(vpn, `title="🇳🇱 Amsterdam · NL-FREE#214"`), strings.Index(vpn, `title="☁️ WARP #1"`); p < 0 || w < 0 || w < p {
		t.Errorf("tunnel order: proton %d, waiting warp %d", p, w)
	}
}
