package status

import (
	"bytes"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/a1ekseev/scd/internal/scd/xrayapi"
	"github.com/a1ekseev/scd/internal/scd/xrayapi/xrayfake"
)

const jsonType = "application/json; charset=utf-8"

// D3: the JSON API of every 0.4 fixture equals the TS reference.
func TestJSONMatchesTSReference(t *testing.T) {
	for _, name := range fixtureNames(t) {
		t.Run(name, func(t *testing.T) {
			l := loadFixture(t, name)
			h := NewHandler(l.deps())
			for _, ep := range []string{"status", "instance-status", "instances"} {
				rec := get(t, h, "/api/"+ep)
				if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != jsonType {
					t.Fatalf("%s: %d %q", ep, rec.Code, rec.Header().Get("Content-Type"))
				}
				if bytes.HasSuffix(rec.Body.Bytes(), []byte("\n")) {
					t.Errorf("%s: trailing newline", ep)
				}
				if !jsonEqual(t, decode(t, rec.Body.Bytes()), readRef(t, name+"/api-"+ep+".json")) {
					t.Errorf("%s/api-%s.json differs", name, ep)
				}
			}
		})
	}
}

// 03 §10.4 item 5.
func TestAPIStatusIsFlatTunnelsArray(t *testing.T) {
	l := loadFixture(t, "healthy-single")
	got := decode(t, get(t, NewHandler(l.deps()), "/api/status").Body.Bytes()).(map[string]any)
	if _, ok := got["subscriptions"]; ok || len(got) != 1 || len(got["tunnels"].([]any)) != 1 {
		t.Fatalf("%v", got)
	}
}

// 03 §10.4 item 6.
func TestAPIStatusExposesHistoryAndBalancerFields(t *testing.T) {
	l := loadFixture(t, "degraded-repaired")
	got := decode(t, get(t, NewHandler(l.deps()), "/api/status").Body.Bytes()).(map[string]any)
	tn := got["tunnels"].([]any)[0].(map[string]any)
	for _, k := range []string{"lastHttpStatus", "lastLatencyMs"} {
		if _, ok := tn[k]; ok {
			t.Errorf("%s present", k)
		}
	}
	if tn["lastError"] != "probe failed" || tn["lastSuccessHttpStatus"] != 204.0 || tn["lastSuccessLatencyMs"] != 42.0 || tn["balanced"] != false {
		t.Fatalf("%v", tn)
	}
	if _, ok := tn["balancerMonitorRemotePingState"]; !ok {
		t.Fatalf("remote ping fields missing: %v", tn)
	}
}

// 03 §10.4 item 7: instanceId falls back to the hostname.
func TestInstanceStatusUsesHostname(t *testing.T) {
	l := loadFixture(t, "empty")
	l.cfg.Config.StatusServer.InstanceID = ""
	d := l.deps()
	d.Hostname = func() (string, error) { return "host-1", nil }
	d.Now = time.Now
	got := decode(t, get(t, NewHandler(d), "/api/instance-status").Body.Bytes()).(map[string]any)
	if got["version"] != 1.0 || got["instanceId"] != "host-1" {
		t.Fatalf("%v", got)
	}
	if _, err := time.Parse(time.RFC3339, got["capturedAt"].(string)); err != nil {
		t.Fatal(err)
	}
	if subs := got["subscriptions"].([]any); subs[0].(map[string]any)["subscriptionId"] != "sub-1" {
		t.Fatalf("%v", subs)
	}
	if tn := got["tunnels"].([]any); len(tn) != 0 {
		t.Fatalf("%v", tn)
	}
}

// 03 §6.2: every request stores the local snapshot in the aggregator.
func TestEveryRequestStoresLocalSnapshot(t *testing.T) {
	l := loadFixture(t, "healthy-single")
	h := NewHandler(l.deps())
	if l.agg.View().Local.InstanceID != "" {
		t.Fatal("local stored before a request")
	}
	_ = get(t, h, "/favicon.ico")
	if v := l.agg.View(); v.Local.InstanceID != "local-main" || len(v.Local.Tunnels) != 1 {
		t.Fatalf("%+v", v.Local)
	}
}

// O8: routing uses the path; the query is ignored.
func TestQueryIgnoredForRouting(t *testing.T) {
	l := loadFixture(t, "healthy-single")
	h := NewHandler(l.deps())
	for _, p := range []string{"/api/status?x=1", "/api/instance-status?a=b", "/api/instances?"} {
		rec := get(t, h, p)
		if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != jsonType {
			t.Errorf("%s: %d %q", p, rec.Code, rec.Header().Get("Content-Type"))
		}
	}
}

func TestOtherPathsServeHTML(t *testing.T) {
	l := loadFixture(t, "healthy-single")
	h := NewHandler(l.deps())
	for _, p := range []string{"/", "/status", "/favicon.ico", "/api/status/", "/api"} {
		rec := get(t, h, p)
		if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "text/html; charset=utf-8" {
			t.Errorf("%s: %d %q", p, rec.Code, rec.Header().Get("Content-Type"))
		}
	}
}

// The method is not checked (03 §6.3).
func TestAnyMethodSameResponse(t *testing.T) {
	l := loadFixture(t, "healthy-single")
	h := NewHandler(l.deps())
	want := get(t, h, "/api/status").Body.String()
	for _, m := range []string{http.MethodPost, http.MethodDelete, http.MethodPut} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), m, "/api/status", strings.NewReader("x")))
		if rec.Code != http.StatusOK || rec.Body.String() != want {
			t.Errorf("%s: %d", m, rec.Code)
		}
	}
}

func runtimeStateJSON(t *testing.T, h http.Handler, path string) (int, map[string]any) {
	t.Helper()
	rec := get(t, h, path)
	if rec.Header().Get("Content-Type") != jsonType {
		t.Fatalf("content type %q", rec.Header().Get("Content-Type"))
	}
	return rec.Code, decode(t, rec.Body.Bytes()).(map[string]any)
}

func TestRuntimeStateDisabled(t *testing.T) {
	l := loadFixture(t, "healthy-single")
	l.cfg.Config.StatusServer.RuntimeState.Enabled = false
	code, body := runtimeStateJSON(t, NewHandler(l.deps()), "/api/runtime-state?subscriptionId=sub-1&targetAddress=127.0.0.1:8080")
	if code != http.StatusNotFound || body["error"] != "Runtime state endpoint is disabled." {
		t.Fatalf("%d %v", code, body)
	}
}

// 03 §10.4 item 9.
func TestRuntimeStateRequiresParams(t *testing.T) {
	l := loadFixture(t, "healthy-single")
	h := NewHandler(l.deps())
	for _, p := range []string{"/api/runtime-state", "/api/runtime-state?subscriptionId=sub-1", "/api/runtime-state?subscriptionId=+&targetAddress=x"} {
		code, body := runtimeStateJSON(t, h, p)
		if code != http.StatusBadRequest || body["error"] != "subscriptionId and targetAddress query parameters are required." {
			t.Errorf("%s: %d %v", p, code, body)
		}
	}
}

// 03 §10.4 item 10: Xray is not contacted.
func TestRuntimeStateTargetNotFound(t *testing.T) {
	l := loadFixture(t, "healthy-single")
	d := l.deps()
	d.Dial = func(string, time.Duration) (xrayapi.Client, error) {
		t.Fatal("Xray dialled")
		return nil, nil
	}
	code, body := runtimeStateJSON(t, NewHandler(d), "/api/runtime-state?subscriptionId=sub-1&targetAddress=127.0.0.1%3A9999")
	if code != http.StatusNotFound || body["error"] != "Target not found in loaded config." {
		t.Fatalf("%d %v", code, body)
	}
}

// 03 §10.4 item 11 and the 500 branch.
func TestRuntimeStateXrayErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		code int
	}{
		{"api error", &xrayapi.Error{Message: "Xray API unavailable"}, http.StatusBadGateway},
		{"other error", errors.New("boom"), http.StatusInternalServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := loadFixture(t, "healthy-single")
			d := l.deps()
			d.Dial = func(string, time.Duration) (xrayapi.Client, error) { return nil, tc.err }
			code, body := runtimeStateJSON(t, NewHandler(d), "/api/runtime-state?subscriptionId=+sub-1+&targetAddress=127.0.0.1%3A8080")
			if code != tc.code || body["error"] != tc.err.Error() {
				t.Fatalf("%d %v", code, body)
			}
		})
	}
}

// 03 §10.4 item 12: the endpoint serves the redacted builder output.
func TestRuntimeStateEndpointRedacts(t *testing.T) {
	srv, _ := startFake(t, xrayfake.WithStaticOutbounds("direct"), xrayfake.WithStaticInbounds("api-in"),
		xrayfake.WithStaticRules(xrayapi.Rule{Tag: "direct", RuleTag: "api-rule"}))
	l := loadFixture(t, "healthy-single")
	l.cfg.Config.Subscriptions[0].Sources[0].Input = "https://subscription.example.test/tokenized/path?secret=abc"
	d := l.deps()
	d.Dial = srv.Dialer()
	code, body := runtimeStateJSON(t, NewHandler(d), "/api/runtime-state?subscriptionId=sub-1&targetAddress=127.0.0.1:8080")
	if code != http.StatusOK {
		t.Fatalf("%d %v", code, body)
	}
	sub := body["config"].(map[string]any)["subscription"].(map[string]any)
	if in := sub["sources"].([]any)[0].(map[string]any)["input"]; in != "https://subscription.example.test/<redacted>" {
		t.Fatalf("input %v", in)
	}
	if bm := body["serviceState"].(map[string]any)["balancerMonitor"].(map[string]any); bm["state"] != "healthy" || bm["consecutiveFailures"] != 0.0 {
		t.Fatalf("serviceState %v", bm)
	}
	for _, kind := range []string{"outbounds", "inbounds", "routingRules"} {
		items := body["runtime"].(map[string]any)[kind].([]any)
		if len(items) == 0 {
			t.Fatalf("%s: no items", kind)
		}
		for _, it := range items {
			if _, ok := it.(map[string]any)["rawBase64"]; ok {
				t.Errorf("rawBase64 in %s", kind)
			}
		}
	}
}

type panicMemoryKey struct{}

// D23: a panic becomes a 500 and an error log; the server keeps serving.
func TestPanicReturns500(t *testing.T) {
	l := loadFixture(t, "healthy-single")
	var logs bytes.Buffer
	d := l.deps()
	d.Log = slog.New(slog.NewJSONHandler(&logs, nil))
	d.Now = func() time.Time { panic(panicMemoryKey{}) }
	rec := get(t, NewHandler(d), "/api/status")
	if rec.Code != http.StatusInternalServerError || !strings.Contains(logs.String(), `"event":"http_handler_panic"`) {
		t.Fatalf("%d %s", rec.Code, logs.String())
	}
}

// Without an aggregator the handler creates one (TS default state).
func TestHandlerWithoutAggregator(t *testing.T) {
	l := loadFixture(t, "remote-instances")
	d := l.deps()
	d.Aggregator = nil
	got := decode(t, get(t, NewHandler(d), "/api/instances").Body.Bytes()).(map[string]any)
	if n := len(got["instances"].([]any)); n != 3 {
		t.Fatalf("instances %d", n)
	}
}
