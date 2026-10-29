package status

import (
	"bytes"
	"crypto/x509"
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

	"github.com/a1ekseev/scd/internal/common/candidate"
	"github.com/a1ekseev/scd/internal/scd/config"
	"github.com/a1ekseev/scd/internal/scd/state"
	"github.com/a1ekseev/scd/internal/scd/syncer"
	"github.com/a1ekseev/scd/internal/scd/xrayapi/xrayfake"
)

// No device key of a masque source reaches any status output, even with
// includeRaw and includeSecrets; the masque fields are set.
func TestMasqueSecretsGuard(t *testing.T) {
	b, err := os.ReadFile("../../../docker/fixtures/masque/candidates.txt")
	if err != nil {
		t.Fatal(err)
	}
	var forms []string
	for l := range strings.SplitSeq(string(b), "\n") {
		if c, err := candidate.ParseMasque(l); err == nil {
			p8, _ := x509.MarshalPKCS8PrivateKey(c.PrivateKey)
			sec1, _ := x509.MarshalECPrivateKey(c.PrivateKey)
			raw, _ := c.PrivateKey.Bytes()
			forms = append(forms, base64.RawURLEncoding.EncodeToString(p8), base64.StdEncoding.EncodeToString(p8),
				base64.StdEncoding.EncodeToString(sec1), hex.EncodeToString(raw))
		}
	}
	path := filepath.Join(t.TempDir(), "mq")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	tg := testTarget()
	sub := config.SubscriptionConfig{ID: "sub", Enabled: true, Target: tg, Sources: []config.SourceConfig{{
		ID: "mq", Type: config.SourceMasque, Input: path, Enabled: true, MaxInputBytes: 1 << 20,
		PortRange: &config.PortRange{Start: 20100, End: 20109}, MaxActive: 2,
		Selection: &config.SelectionConfig{Strategy: config.StrategyLabel, SwitchConfirmChecks: 1, DeadPairCooldownMinutes: 4, ReplaceAfterFailures: 1},
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
		for _, form := range forms {
			if strings.Contains(out, form) {
				t.Errorf("%s holds a device key", name)
			}
		}
	}

	var snap struct {
		Runtime struct {
			Outbounds []runtimeItem `json:"outbounds"`
		} `json:"runtime"`
		Diff rsDiff `json:"diff"`
	}
	if err := json.Unmarshal([]byte(rs), &snap); err != nil {
		t.Fatal(err)
	}
	if len(snap.Diff.Outbounds.Matched) != 2 || len(snap.Runtime.Outbounds) != 2 {
		t.Fatalf("runtime-state %s", rs)
	}
	for _, o := range snap.Runtime.Outbounds {
		p := o.Parsed
		if _, leak := p["privateKey"]; leak || o.RawBase64 != "" || p["protocol"] != "masque" || p["network"] != "masque" ||
			p["security"] != "tls" || p["sni"] == nil || p["publicKey"] == nil || p["endpoint"] == nil || p["address"] == nil {
			t.Errorf("runtime outbound %+v", o)
		}
	}
	if !strings.Contains(outputs["/api/instance-status"], `"protocol": "masque"`) {
		t.Fatalf("instance-status %s", outputs["/api/instance-status"])
	}
}
