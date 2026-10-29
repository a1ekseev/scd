package status

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/a1ekseev/scd/internal/scd/config"
)

const wgpStatusOK = `{"version": 1, "updatedAt": "2026-08-15T09:58:00Z", "futureField": true, "providers": [
  {"id": "acc", "output": "wg", "type": "account", "state": "needs_login", "error": "refresh rejected",
   "lastTickAt": "2026-08-15T09:58:00Z", "issued": 3, "limit": 10, "expiresAt": "2026-08-15T16:00:00Z"}]}`

// wgpLoaded is the sources-wireguard fixture with its wireguard source
// reading dir; dir/status.json gets data and mode when data is not nil.
func wgpLoaded(t *testing.T, data []byte, mode os.FileMode) (loaded, string) {
	t.Helper()
	l := loadFixture(t, "sources-wireguard")
	dir := t.TempDir()
	l.cfg.Config.Subscriptions[0].Sources[1].Input = filepath.Join(dir, "wg")
	if data != nil {
		p := filepath.Join(dir, "status.json")
		if err := os.WriteFile(p, data, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
	}
	return l, dir
}

// The wgp block of /api/status and /api/instance-status for each state of
// status.json: always 200, an unreadable file gives a block with an error
// that does not reveal the path.
func TestWGPBlock(t *testing.T) {
	big := `{"version": 1, "providers": [], "pad": "` + strings.Repeat("x", wgpMaxBytes) + `"}`
	tests := []struct {
		name    string
		data    []byte
		mode    os.FileMode
		want    string // the JSON of "wgp", "" = no key
		wantErr string
	}{
		{name: "present", data: []byte(wgpStatusOK), mode: 0o600, want: `[{"sources":[{"subscriptionId":"sub-1","sourceId":"wg"}],` +
			`"updatedAt":"2026-08-15T09:58:00.000Z","providers":[{"id":"acc","output":"wg","type":"account","state":"needs_login",` +
			`"error":"refresh rejected","lastTickAt":"2026-08-15T09:58:00.000Z","issued":3,"limit":10,"expiresAt":"2026-08-15T16:00:00.000Z"}]}]`},
		{name: "missing"},
		{name: "0644", data: []byte(wgpStatusOK), mode: 0o644, wantErr: "group and others must have no access"},
		{name: "broken JSON", data: []byte(`{"version": 1,`), mode: 0o600, wantErr: "parse status.json"},
		{name: "version 2", data: []byte(`{"version": 2}`), mode: 0o600, wantErr: "unsupported version 2"},
		{name: "over limit", data: []byte(big), mode: 0o600, wantErr: "exceeds 65536 bytes"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			l, dir := wgpLoaded(t, tc.data, tc.mode)
			h := NewHandler(l.deps())
			for _, ep := range []string{"/api/status", "/api/instance-status"} {
				rec := get(t, h, ep)
				if rec.Code != http.StatusOK {
					t.Fatalf("%s: %d", ep, rec.Code)
				}
				body := rec.Body.String()
				if strings.Contains(body, dir) {
					t.Errorf("%s reveals the directory: %s", ep, body)
				}
				var got struct {
					Tunnels []any           `json:"tunnels"`
					WGP     json.RawMessage `json:"wgp"`
				}
				if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
					t.Fatal(err)
				}
				if len(got.Tunnels) != 4 {
					t.Errorf("%s: %d tunnels, want the full response", ep, len(got.Tunnels))
				}
				switch {
				case tc.wantErr != "":
					var blocks []WGPBlock
					if err := json.Unmarshal(got.WGP, &blocks); err != nil {
						t.Fatal(err)
					}
					if len(blocks) != 1 || !strings.Contains(blocks[0].Error, tc.wantErr) || len(blocks[0].Providers) != 0 {
						t.Errorf("%s: wgp %s, want error %q", ep, got.WGP, tc.wantErr)
					}
				case tc.want == "":
					if got.WGP != nil {
						t.Errorf("%s: unexpected wgp %s", ep, got.WGP)
					}
				default:
					jsonEqual(t, decode(t, got.WGP), decode(t, []byte(tc.want)))
				}
			}
			if strings.Contains(get(t, h, "/api/runtime-state?subscriptionId=x&targetAddress=y").Body.String(), "wgp") {
				t.Error("runtime-state mentions wgp")
			}
		})
	}
}

// Sources that share a directory share one block; disabled sources and
// subscriptions are not read.
func TestWGPBlockPerDirectory(t *testing.T) {
	l, dir := wgpLoaded(t, []byte(wgpStatusOK), 0o600)
	subs := l.cfg.Config.Subscriptions
	subs[0].Sources = append(subs[0].Sources, config.SourceConfig{ID: "mq", Type: "masque", Enabled: true, Input: filepath.Join(dir, "mq")})
	other := t.TempDir()
	if err := os.WriteFile(filepath.Join(other, "status.json"), []byte(wgpStatusOK), 0o600); err != nil {
		t.Fatal(err)
	}
	subs = append(subs,
		config.SubscriptionConfig{ID: "off", Enabled: false, Sources: []config.SourceConfig{{ID: "wg", Type: "wireguard", Enabled: true, Input: filepath.Join(other, "wg")}}},
		config.SubscriptionConfig{ID: "on", Enabled: true, Sources: []config.SourceConfig{{ID: "wg", Type: "wireguard", Enabled: false, Input: filepath.Join(other, "wg")}}})
	l.cfg.Config.Subscriptions = subs
	blocks := buildWGP(l.cfg.Config)
	if len(blocks) != 1 || len(blocks[0].Sources) != 2 || blocks[0].Sources[0].SourceID != "mq" || blocks[0].Sources[1].SourceID != "wg" {
		t.Fatalf("blocks %+v", blocks)
	}
}

// The central scd shows the wgp block of a remote scd collected over HTTP.
func TestWGPBlockFromRemote(t *testing.T) {
	remote, _ := wgpLoaded(t, []byte(wgpStatusOK), 0o600)
	remoteHandler := NewHandler(remote.deps())
	r, srv := remoteServer(t, func(w http.ResponseWriter, req *http.Request) { remoteHandler.ServeHTTP(w, req) })

	central := loadFixture(t, "healthy-single")
	central.cfg.Config.StatusServer.RemoteInstances = []config.RemoteStatusInstanceConfig{r}
	central.agg = NewAggregator(central.cfg.Config.StatusServer.RemoteInstances)
	central.agg.Collect(context.Background(), r, srv.Client(), nil, clock(t0))
	h := NewHandler(central.deps())

	var inst struct {
		Instances []struct {
			Source   string `json:"source"`
			Snapshot struct {
				WGP []WGPBlock `json:"wgp"`
			} `json:"snapshot"`
		} `json:"instances"`
	}
	if err := json.Unmarshal(get(t, h, "/api/instances").Body.Bytes(), &inst); err != nil {
		t.Fatal(err)
	}
	if len(inst.Instances) != 2 || len(inst.Instances[0].Snapshot.WGP) != 0 || len(inst.Instances[1].Snapshot.WGP) != 1 {
		t.Fatalf("instances %+v", inst)
	}
	view := central.agg.View()
	if s := view.Remotes[0].Snapshot; s == nil || len(s.WGP) != 1 || s.WGP[0].Providers[0].Limit != 10 || s.WGP[0].Providers[0].ExpiresAt == "" {
		t.Fatalf("typed remote snapshot %+v", s)
	}
	body := bodyOf(t, get(t, h, "/").Body.String())
	_, remotePanel, _ := strings.Cut(body, `id="instance-panel-1"`)
	assertContains(t, remotePanel, ">wgp<", ">acc · wg<", ">needs_login<")
}

// The wgp cards of the fixtures: common fields only, degraded for
// needs_login, a near expiry and a broken file; none without wgp.
func TestDashboardWGP(t *testing.T) {
	body := bodyOf(t, dashboard(t, "wgp-ok", "/"))
	local, remote, _ := strings.Cut(body, `id="instance-panel-1"`)
	assertContains(t, local, "<h2>wgp</h2>", ">wgp · account<", ">acc · wg<", ">dev · mq<",
		`<span class="metric metric-state-healthy"><span class="metric-label">Status</span><span class="metric-value">ok</span></span>`,
		`<span class="metric-label">Issued</span><span class="metric-value">3 of 10</span>`,
		`<span class="metric-label">Issued</span><span class="metric-value">2</span>`,
		`<span class="metric"><span class="metric-label">Expires</span><span class="metric-value">2026-08-22T09:00:00.000Z</span></span>`,
		`<span class="metric-label">Last Success</span><span class="metric-value">2026-08-15T09:58:00.000Z</span>`)
	assertNotContains(t, local, "state-degraded")
	if w, s := strings.Index(local, "<h2>wgp</h2>"), strings.Index(local, "<h2>sub-1</h2>"); w < 0 || w > s {
		t.Errorf("wgp section (%d) must precede the subscriptions (%d)", w, s)
	}
	assertNotContains(t, remote, ">wgp<")

	body = bodyOf(t, dashboard(t, "wgp-needs-login", "/"))
	_, acc, _ := strings.Cut(body, ">acc · wg<")
	acc, _, _ = strings.Cut(acc, "</article>")
	assertContains(t, acc,
		`<span class="metric metric-state-degraded"><span class="metric-label">Status</span><span class="metric-value">needs_login</span></span>`,
		`<span class="metric"><span class="metric-label">Expires</span>`,
		`<span class="metric metric-state-degraded"><span class="metric-label">Error</span><span class="metric-value">session refresh rejected (HTTP 422)</span></span>`)
	assertContains(t, body, `class="balancer-card state-degraded"`, ">soon · wg2<", ">sub-1/mq<",
		`<span class="metric-label">Error</span><span class="metric-value">parse status.json: unexpected end of JSON input</span>`)
	_, section, _ := strings.Cut(body, "<h2>wgp</h2>")
	section, _, _ = strings.Cut(section, "</section>")
	if n := strings.Count(section, `class="balancer-card state-degraded"`); n != 3 || strings.Contains(section, "balancer-card state-healthy") {
		t.Errorf("want 3 degraded wgp cards, got %d:\n%s", n, section)
	}

	for _, name := range fixtureNames(t) {
		assertNotContains(t, dashboard(t, name, "/"), "<h2>wgp</h2>")
	}
}

// Only an expiry closer than wgpExpirySoon degrades an ok card: a
// certificate renewed on time is hours away from it.
func TestWGPCardExpiry(t *testing.T) {
	for expires, want := range map[string]string{"2026-08-16T04:00:00Z": "healthy", "2026-08-15T11:00:00Z": "degraded"} {
		cards := wgpCards(&InstanceSnapshot{CapturedAt: "2026-08-15T10:00:00.000Z", WGP: []WGPBlock{{Providers: []WGPProvider{{ID: "x", State: "ok", ExpiresAt: expires}}}}})
		if len(cards) != 1 || cards[0].Class != want {
			t.Errorf("expires %s: %+v, want %s", expires, cards, want)
		}
	}
}

func TestWGPCardUnknownState(t *testing.T) {
	cards := wgpCards(&InstanceSnapshot{CapturedAt: "2026-08-15T10:00:00.000Z", WGP: []WGPBlock{{Providers: []WGPProvider{{ID: "x", State: "future"}}}}})
	if len(cards) != 1 || cards[0].Class != "degraded" {
		t.Fatalf("%+v", cards)
	}
}
