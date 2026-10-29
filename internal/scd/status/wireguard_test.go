package status

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/a1ekseev/scd/internal/scd/config"
	"github.com/a1ekseev/scd/internal/scd/state"
	"github.com/a1ekseev/scd/internal/scd/syncer"
	"github.com/a1ekseev/scd/internal/scd/xrayapi/xrayfake"
)

// No secret key of a wireguard source reaches any status output, even with
// includeRaw and includeSecrets; the wireguard fields are set.
func TestWireGuardSecretsGuard(t *testing.T) {
	b, err := os.ReadFile("../../../docker/fixtures/wireguard/candidates.txt")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "wg")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	tg := testTarget()
	sub := config.SubscriptionConfig{ID: "sub", Enabled: true, Target: tg, Sources: []config.SourceConfig{{
		ID: "wg", Type: config.SourceWireGuard, Input: path, Enabled: true, MaxInputBytes: 1 << 20,
		PortRange: &config.PortRange{Start: 20100, End: 20109}, MaxActive: 3,
		Selection: &config.SelectionConfig{Strategy: config.StrategyLoad, SwitchConfirmChecks: 1, DeadPairCooldownMinutes: 4, ReplaceAfterFailures: 1},
		WireGuard: &config.WireGuardConfig{EndpointDomainStrategy: "UseIPv4"},
	}}}
	on := config.ResourceToggle{Enabled: true}
	cfg := config.Loaded{Path: "/cfg/config.yml", Config: config.AppConfig{
		Subscriptions: []config.SubscriptionConfig{sub},
		Resources:     config.ResourceConfig{Outbounds: on, Inbounds: on, Routing: on},
		StatusServer: config.StatusServerConfig{Enabled: true, InstanceID: "local", RuntimeState: config.RuntimeStateConfig{
			Enabled: true, IncludeRaw: true, IncludeSecrets: true,
		}},
	}}
	fake, _ := xrayfake.Start(t)
	var logs bytes.Buffer
	mem := state.New()
	s := &syncer.Syncer{Config: cfg, Memory: mem, Log: slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})), Dial: fake.Dialer()}
	rep, err := s.Run(t.Context())
	if err != nil || rep.Failed != 0 {
		t.Fatalf("sync %+v %v", rep, err)
	}
	repJSON, _ := json.Marshal(rep)
	printed, _ := json.Marshal(cfg.Config.Printed(true))

	h := NewHandler(Deps{Config: cfg, Memory: mem, Dial: fake.Dialer(), Now: func() time.Time { return t0 }, Log: discardLogger()})
	get := func(p string) string {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), "GET", p, nil))
		body, _ := io.ReadAll(rec.Body)
		if rec.Code != 200 {
			t.Fatalf("%s: %d %s", p, rec.Code, body)
		}
		return string(body)
	}
	rs := get(pathRuntimeState + "?subscriptionId=sub&targetAddress=" + url.QueryEscape(tg.Address))
	outputs := map[string]string{
		"logs": logs.String(), "sync report": string(repJSON), "print-config": string(printed),
		"/api/status": get(pathStatus), "/api/instance-status": get(pathInstanceStatus), "/api/runtime-state": rs, "dashboard": get("/"),
	}
	for name, out := range outputs {
		for _, by := range []byte{0x11, 0x12, 0x14, 0x15, 0x16, 0x17, 0x18, 0x19, 0x35} {
			k := bytes.Repeat([]byte{by}, 32)
			for _, form := range []string{base64.StdEncoding.EncodeToString(k), base64.RawURLEncoding.EncodeToString(k), hex.EncodeToString(k)} {
				if strings.Contains(out, form) {
					t.Errorf("%s holds the secret key %s", name, form)
				}
			}
		}
	}

	var snap struct {
		Expected struct {
			Outbounds []expectedOutbound `json:"outbounds"`
			Error     string             `json:"error"`
		} `json:"expected"`
		Runtime struct {
			Outbounds []runtimeItem `json:"outbounds"`
		} `json:"runtime"`
		Diff rsDiff `json:"diff"`
	}
	if err := json.Unmarshal([]byte(rs), &snap); err != nil {
		t.Fatal(err)
	}
	if len(snap.Expected.Outbounds) != 3 || snap.Expected.Error != "" || len(snap.Diff.Outbounds.Matched) != 3 || len(snap.Runtime.Outbounds) != 3 {
		t.Fatalf("runtime-state %s", rs)
	}
	for _, o := range snap.Runtime.Outbounds {
		if o.RawBase64 != "" || o.Parsed["protocol"] != "wireguard" || o.Parsed["noKernelTun"] != true || o.Parsed["publicKey"] == nil {
			t.Errorf("runtime outbound %+v", o)
		}
	}

	var inst struct {
		Tunnels       []StatusTunnel `json:"tunnels"`
		Subscriptions []struct {
			Targets []struct {
				Sources []SourceView `json:"sources"`
			} `json:"targets"`
		} `json:"subscriptions"`
	}
	if err := json.Unmarshal([]byte(outputs["/api/instance-status"]), &inst); err != nil {
		t.Fatal(err)
	}
	if len(inst.Tunnels) != 3 || inst.Tunnels[0].Protocol != "wireguard" {
		t.Fatalf("tunnels %+v", inst.Tunnels)
	}
	if len(inst.Subscriptions) != 1 || len(inst.Subscriptions[0].Targets) != 1 || len(inst.Subscriptions[0].Targets[0].Sources) != 1 {
		t.Fatalf("targets %s", outputs["/api/instance-status"])
	}
	if v := inst.Subscriptions[0].Targets[0].Sources[0]; v.MaxActive != 3 || v.ActiveCount != 3 || v.CandidateCount != 7 {
		t.Fatalf("source view %+v", v)
	}
}
