package status

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"golang.org/x/net/html"
)

// dashboard serves "/" for fixture name and returns the HTML body.
func dashboard(t *testing.T, name, path string) string {
	t.Helper()
	l := loadFixture(t, name)
	rec := get(t, NewHandler(l.deps()), path)
	if rec.Code != 200 || rec.Header().Get("Content-Type") != contentTypeHTML {
		t.Fatalf("%s %s: %d %q", name, path, rec.Code, rec.Header().Get("Content-Type"))
	}
	return rec.Body.String()
}

// bodyOf returns the part of page after <body> (the CSS names every class,
// so markup checks must not look at <head>).
func bodyOf(t *testing.T, page string) string {
	t.Helper()
	_, body, ok := strings.Cut(page, "<body>")
	if !ok {
		t.Fatal("no <body>")
	}
	return body
}

func assertContains(t *testing.T, s string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(s, w) {
			t.Errorf("missing %q", w)
		}
	}
}

func assertNotContains(t *testing.T, s string, unwanted ...string) {
	t.Helper()
	for _, w := range unwanted {
		if strings.Contains(s, w) {
			t.Errorf("unexpected %q", w)
		}
	}
}

// 03 §10.4 item 1.
func TestDashboardCardsAndClasses(t *testing.T) {
	page := dashboard(t, "remote-ping-states", "/")
	body := bodyOf(t, page)
	assertContains(t, page, "<title>Subscription Control Daemon Status</title>")
	assertContains(t, body,
		`class="balancer-card state-degraded"`, `class="node-card state-healthy"`,
		">Last Check<", ">42 ms<", ">Remote Push<", ">failed down<",
		`class="metric metric-state-healthy"`, `class="metric metric-state-degraded"`,
		`class="metric metric-balancer-active"`)
	assertNotContains(t, page, "HTTP 204", "Last success", "Remote Ping ok", "Kuma",
		"Current runtime state (JSON)", "/api/runtime-state", "<table>",
		`<link rel="stylesheet"`, "<script src=", "<link ")
	b, n := strings.Index(body, "balancer-card"), strings.Index(body, "nodes-grid")
	if b < 0 || n < 0 || b > n {
		t.Errorf("balancer-card (%d) must precede nodes-grid (%d)", b, n)
	}
}

// 03 §10.4 item 2.
func TestDashboardDegradedCurrentErrorAndBalancerRemoved(t *testing.T) {
	body := bodyOf(t, dashboard(t, "degraded-repaired", "/status"))
	assertContains(t, body, `class="node-card state-degraded"`, "Error: probe failed", ">removed<",
		`class="metric metric-balancer-removed"`)
	assertNotContains(t, body, "HTTP 204")
}

// 03 §10.4 item 3.
func TestDashboardTargetWithoutTunnels(t *testing.T) {
	body := bodyOf(t, dashboard(t, "balancer-only", "/"))
	assertContains(t, body, "127.0.0.1:8080", `class="balancer-card state-healthy"`, "No tunnel data.",
		">17 ms<", ">delivered up<", `class="metric metric-remote-up"`)
	assertNotContains(t, body, "HTTP 204")
}

// 03 §10.4 item 4.
func TestDashboardSkipsDisabledSubscriptions(t *testing.T) {
	body := bodyOf(t, dashboard(t, "disabled-subscription", "/"))
	assertContains(t, body, "sub-1")
	assertNotContains(t, body, "sub-disabled", "127.0.0.1:9999")
}

// attrs returns the attributes of the element with id, nil if absent.
func attrs(doc *html.Node, id string) map[string]string {
	for n := range doc.Descendants() {
		if n.Type != html.ElementNode {
			continue
		}
		m := map[string]string{}
		for _, a := range n.Attr {
			m[a.Key] = a.Val
		}
		if m["id"] == id {
			return m
		}
	}
	return nil
}

// 03 §10.4 item 8.
func TestDashboardInstanceTabs(t *testing.T) {
	page := dashboard(t, "remote-instances", "/")
	body := bodyOf(t, page)
	assertContains(t, body, "edge-eu", "Last collection failed", `role="tablist" aria-label="Instances"`,
		"Waiting for first successful collection", "No snapshot available.")
	for _, key := range []string{"ArrowRight", "ArrowLeft", "Home", "End"} {
		assertContains(t, page, "event.key === '"+key+"'")
	}
	if l, e := strings.Index(body, ">local-main<"), strings.Index(body, ">edge-eu<"); l < 0 || e < 0 || l > e {
		t.Errorf("local-main (%d) must precede edge-eu (%d)", l, e)
	}

	doc, err := html.Parse(strings.NewReader(page))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		id   string
		want map[string]string
		// hidden reports whether the hidden attribute is expected.
		hidden bool
	}{
		{"instance-tab-0", map[string]string{"role": "tab", "aria-selected": "true", "tabindex": "0", "aria-controls": "instance-panel-0", "class": "instance-tab is-active"}, false},
		{"instance-tab-1", map[string]string{"role": "tab", "aria-selected": "false", "tabindex": "-1", "aria-controls": "instance-panel-1", "class": "instance-tab"}, false},
		{"instance-panel-0", map[string]string{"role": "tabpanel", "tabindex": "0", "aria-labelledby": "instance-tab-0"}, false},
		{"instance-panel-1", map[string]string{"role": "tabpanel", "tabindex": "0", "aria-labelledby": "instance-tab-1"}, true},
		{"instance-panel-2", map[string]string{"role": "tabpanel", "tabindex": "0", "aria-labelledby": "instance-tab-2"}, true},
	}
	for _, c := range cases {
		t.Run(c.id, func(t *testing.T) {
			got := attrs(doc, c.id)
			if got == nil {
				t.Fatal("element not found")
			}
			for k, v := range c.want {
				if got[k] != v {
					t.Errorf("%s = %q, want %q", k, got[k], v)
				}
			}
			if _, hidden := got["hidden"]; hidden != c.hidden {
				t.Errorf("hidden = %v, want %v", hidden, c.hidden)
			}
		})
	}
}

// Review Focus 5: hostile strings from local state and remote snapshots
// are escaped.
func TestDashboardEscapesHostileStrings(t *testing.T) {
	page := dashboard(t, "hostile-strings", "/")
	assertNotContains(t, page, "<script>alert", "<img src=x")
	assertContains(t, bodyOf(t, page), "&lt;script&gt;alert(", "&lt;img src=x onerror=alert(1)&gt;")
	if n := strings.Count(page, "<script"); n != 1 {
		t.Errorf("%d <script elements, want 1", n)
	}
}

// visibleText is the text of <body> outside <script>/<style>, whitespace
// collapsed.
func visibleText(t *testing.T, page []byte) string {
	t.Helper()
	doc, err := html.Parse(bytes.NewReader(page))
	if err != nil {
		t.Fatal(err)
	}
	var parts []string
	var walk func(n *html.Node, inBody bool)
	walk = func(n *html.Node, inBody bool) {
		if n.Type == html.ElementNode {
			switch n.Data {
			case "script", "style":
				return
			case "body":
				inBody = true
			}
		}
		if n.Type == html.TextNode && inBody {
			parts = append(parts, n.Data)
		}
		for c := range n.ChildNodes() {
			walk(c, inBody)
		}
	}
	walk(doc, false)
	return strings.Join(strings.Fields(strings.Join(parts, " ")), " ")
}

// D3: the visible text of every fixture equals the TS dashboard.
func TestDashboardTextMatchesTS(t *testing.T) {
	for _, name := range fixtureNames(t) {
		t.Run(name, func(t *testing.T) {
			want, err := os.ReadFile(refStatusDir + name + "/index.html")
			if err != nil {
				t.Fatal(err)
			}
			got := dashboard(t, name, "/")
			if g, w := visibleText(t, []byte(got)), visibleText(t, want); g != w {
				t.Errorf("text differs\n--- got\n%s\n--- want\n%s", g, w)
			}
		})
	}
}

// Without any instance (no local snapshot stored) the page shows the TS
// placeholder and no tab list.
func TestDashboardWithoutInstances(t *testing.T) {
	var buf bytes.Buffer
	if err := RenderDashboard(&buf, AggregatedView{}); err != nil {
		t.Fatal(err)
	}
	body := bodyOf(t, buf.String())
	assertContains(t, body, "<h2>No subscriptions</h2>")
	assertNotContains(t, body, "instance-tabs", "instance-panel")
}

// A remote snapshot without balancer monitor or country renders like TS
// with the fallbacks idle and "-".
func TestDashboardRemoteFallbacks(t *testing.T) {
	snap := &InstanceSnapshot{Subscriptions: []GroupedSubscription{{
		SubscriptionID: "sub-r",
		Targets: []GroupedStatusTarget{{TargetAddress: "10.0.0.9:1", Tunnels: []StatusTunnel{{
			DisplayName: "R", Endpoint: "10.0.0.9:2", State: "weird",
		}}}},
	}}}
	view := AggregatedView{Remotes: []RemoteView{{ID: "r", LastSuccessAt: "2026-01-01T00:00:00.000Z", Snapshot: snap}}}
	var buf bytes.Buffer
	if err := RenderDashboard(&buf, view); err != nil {
		t.Fatal(err)
	}
	body := bodyOf(t, buf.String())
	assertContains(t, body, `class="balancer-card state-idle"`, `class="node-card state-idle"`,
		`class="metric metric-state-idle"><span class="metric-label">State</span><span class="metric-value">weird<`,
		">- · 10.0.0.9:2<", "remote · Updated 2026-01-01T00:00:00.000Z", `class="metric metric-balancer-na"`)
}

func TestDashboardHelpers(t *testing.T) {
	t.Run("stateClass", func(t *testing.T) {
		for in, want := range map[string]string{"healthy": "healthy", "degraded": "degraded", "repairing": "repairing", "idle": "idle", "": "idle", "x": "idle"} {
			if got := stateClass(in); got != want {
				t.Errorf("stateClass(%q) = %q, want %q", in, got, want)
			}
		}
	})
	t.Run("formatLastCheck", func(t *testing.T) {
		cases := []struct {
			lat  *int
			err  string
			want string
		}{{new(42), "boom", "42 ms"}, {new(0), "", "0 ms"}, {nil, "boom", "Error: boom"}, {nil, "", "-"}}
		for _, c := range cases {
			if got := formatLastCheck(c.lat, c.err); got != c.want {
				t.Errorf("formatLastCheck(%v, %q) = %q, want %q", c.lat, c.err, got, c.want)
			}
		}
		if formatLatency(nil) != "-" {
			t.Error("formatLatency(nil)")
		}
	})
	t.Run("formatBalancerStatus", func(t *testing.T) {
		if formatBalancerStatus(new(true)) != "active" || formatBalancerStatus(new(false)) != "removed" || formatBalancerStatus(nil) != "n/a" {
			t.Error("unexpected balancer status")
		}
	})
	t.Run("formatRemotePushStatus", func(t *testing.T) {
		cases := []struct{ state, reported, want string }{
			{"ok", "up", "delivered up"},
			{"ok", "", "delivered"},
			{"failed", "down", "failed down"},
			{"pending", "", "pending"},
			{"idle", "", "idle"},
			{"", "up", "idle up"},
		}
		for _, c := range cases {
			if got := formatRemotePushStatus(c.state, c.reported); got != c.want {
				t.Errorf("formatRemotePushStatus(%q, %q) = %q, want %q", c.state, c.reported, got, c.want)
			}
		}
	})
	t.Run("metricClass", func(t *testing.T) {
		cases := []struct{ label, value, want string }{
			{"State", "healthy", " metric-state-healthy"},
			{"State", "x", " metric-state-idle"},
			{"Balancer", "active", " metric-balancer-active"},
			{"Balancer", "removed", " metric-balancer-removed"},
			{"Balancer", "n/a", " metric-balancer-na"},
			{"Remote Push", "delivered up", " metric-remote-up"},
			{"Remote Push", "delivered down", " metric-remote-down"},
			{"Remote Push", "failed", " metric-remote-down"},
			{"Remote Push", "failed up", " metric-remote-down"},
			{"Remote Push", "pending down", " metric-remote-pending"},
			{"Remote Push", "idle", " metric-remote-idle"},
			{"Last Check", "42 ms", ""},
		}
		for _, c := range cases {
			if got := metricClass(c.label, c.value); got != c.want {
				t.Errorf("metricClass(%q, %q) = %q, want %q", c.label, c.value, got, c.want)
			}
		}
	})
}
