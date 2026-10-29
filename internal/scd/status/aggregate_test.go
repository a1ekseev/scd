package status

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/a1ekseev/scd/internal/scd/config"
)

const remoteSnapshotJSON = `{
  "version": 1,
  "instanceId": "edge-eu",
  "capturedAt": "2026-08-15T09:58:00.000Z",
  "subscriptions": [{"subscriptionId": "sub-eu", "targets": [{"subscriptionId": "sub-eu", "targetAddress": "10.0.0.2:8080",
    "tunnels": [{"subscriptionId": "sub-eu", "targetAddress": "10.0.0.2:8080", "displayName": "<script>alert(\"x\")</script> & 'q'",
      "endpoint": "10.0.0.2:20000", "state": "healthy", "lastLatencyMs": 64, "futureTunnelField": {"nested": [1, 2]}}],
    "balancerMonitor": {"state": "healthy", "remotePingState": "idle"}}]}],
  "tunnels": [{"subscriptionId": "sub-eu", "targetAddress": "10.0.0.2:8080", "displayName": "Frankfurt", "endpoint": "10.0.0.2:20000",
    "state": "degraded", "lastError": "` + "\\u003cb\\u003e" + `"}],
  "futureField": {"kept": true}
}`

// clock returns successive times one minute apart, starting at start.
func clock(start time.Time) func() time.Time {
	var n atomic.Int64
	return func() time.Time { return start.Add(time.Duration(n.Add(1)-1) * time.Minute) }
}

var t0 = time.Date(2026, 8, 15, 10, 0, 0, 0, time.UTC)

func remoteServer(t *testing.T, h http.HandlerFunc) (config.RemoteStatusInstanceConfig, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return config.RemoteStatusInstanceConfig{ID: "edge-eu", APIURL: srv.URL + "/api/instance-status", Schedule: "* * * * *", TimeoutMs: 5000}, srv
}

// instancesJSON returns /api/instances of a as decoded JSON.
func instancesJSON(t *testing.T, a *Aggregator, now time.Time) map[string]any {
	t.Helper()
	b, err := marshalJSON(a.Instances(now))
	if err != nil {
		t.Fatal(err)
	}
	return decode(t, b).(map[string]any)
}

// 03 §10.3 item 1.
func TestCollectStoresLatestSnapshot(t *testing.T) {
	var accept atomic.Value
	r, srv := remoteServer(t, func(w http.ResponseWriter, req *http.Request) {
		accept.Store(req.Header.Get("Accept"))
		_, _ = w.Write([]byte(remoteSnapshotJSON))
	})
	var logs bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	a := NewAggregator([]config.RemoteStatusInstanceConfig{r})
	a.Collect(context.Background(), r, srv.Client(), log, clock(t0))

	if accept.Load() != "application/json" {
		t.Errorf("Accept %v", accept.Load())
	}
	inst := instancesJSON(t, a, t0)["instances"].([]any)
	if len(inst) != 1 {
		t.Fatalf("instances %v", inst)
	}
	got := inst[0].(map[string]any)
	if got["id"] != "edge-eu" || got["source"] != "remote" || got["apiUrl"] != r.APIURL ||
		got["lastAttemptAt"] != "2026-08-15T10:00:00.000Z" || got["lastSuccessAt"] != "2026-08-15T10:01:00.000Z" {
		t.Fatalf("%v", got)
	}
	if _, ok := got["lastError"]; ok {
		t.Fatalf("lastError present: %v", got)
	}
	jsonEqual(t, got["snapshot"], decode(t, []byte(remoteSnapshotJSON)))
	if !strings.Contains(logs.String(), `"event":"remote_status_collected"`) || !strings.Contains(logs.String(), `"remoteInstanceId":"edge-eu"`) ||
		!strings.Contains(logs.String(), `"level":"DEBUG"`) {
		t.Errorf("logs: %s", logs.String())
	}
}

// 03 §10.3 item 2.
func TestCollectKeepsLastSnapshotAfterError(t *testing.T) {
	var fail atomic.Bool
	r, srv := remoteServer(t, func(w http.ResponseWriter, _ *http.Request) {
		if fail.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(remoteSnapshotJSON))
	})
	var logs bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logs, nil))
	a := NewAggregator([]config.RemoteStatusInstanceConfig{r})
	now := clock(t0)
	a.Collect(context.Background(), r, srv.Client(), log, now)
	fail.Store(true)
	a.Collect(context.Background(), r, srv.Client(), log, now)

	got := instancesJSON(t, a, t0)["instances"].([]any)[0].(map[string]any)
	if got["lastError"] != "Remote status API returned HTTP 503." || got["lastSuccessAt"] != "2026-08-15T10:01:00.000Z" ||
		got["lastAttemptAt"] != "2026-08-15T10:02:00.000Z" || got["snapshot"] == nil {
		t.Fatalf("%v", got)
	}
	if !strings.Contains(logs.String(), `"event":"remote_status_collection_failed"`) {
		t.Errorf("logs: %s", logs.String())
	}
	view := a.View()
	if len(view.Remotes) != 1 || view.Remotes[0].Snapshot == nil || view.Remotes[0].LastError == "" {
		t.Fatalf("view %+v", view)
	}

	// A later success clears lastError.
	fail.Store(false)
	a.Collect(context.Background(), r, srv.Client(), log, now)
	got = instancesJSON(t, a, t0)["instances"].([]any)[0].(map[string]any)
	if _, ok := got["lastError"]; ok {
		t.Fatalf("lastError kept after success: %v", got)
	}
}

// 03 §10.3 item 3.
func TestCollectRejectsIncompatiblePayload(t *testing.T) {
	for _, tc := range []struct {
		name, body, want string
	}{
		{"tunnels only", `{"tunnels": []}`, "Remote status response has an unsupported schema."},
		{"array", `[]`, "Remote status response must be a JSON object."},
		{"null", `null`, "Remote status response must be a JSON object."},
		{"bad tunnel state", strings.Replace(remoteSnapshotJSON, `"state": "degraded"`, `"state": "broken"`, 1), "Remote status response has an unsupported schema."},
		{"target without tunnels", `{"version":1,"instanceId":"x","capturedAt":"y","subscriptions":[{"subscriptionId":"s","targets":[{"targetAddress":"a"}]}],"tunnels":[]}`, "Remote status response has an unsupported schema."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, srv := remoteServer(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(tc.body)) })
			a := NewAggregator([]config.RemoteStatusInstanceConfig{r})
			a.Collect(context.Background(), r, srv.Client(), discardLogger(), clock(t0))
			got := instancesJSON(t, a, t0)["instances"].([]any)[0].(map[string]any)
			if got["lastError"] != tc.want {
				t.Fatalf("lastError %v", got["lastError"])
			}
			if _, ok := got["snapshot"]; ok {
				t.Fatalf("snapshot stored: %v", got)
			}
		})
	}
}

func TestRemoteSnapshotVersionMustBe1(t *testing.T) {
	for _, v := range []string{`2`, `"1"`, `0`} {
		body := strings.Replace(remoteSnapshotJSON, `"version": 1`, `"version": `+v, 1)
		if _, err := parseRemoteSnapshot([]byte(body)); err == nil || err.Error() != "Remote status response has an unsupported schema." {
			t.Errorf("version %s: %v", v, err)
		}
	}
	if _, err := parseRemoteSnapshot([]byte(remoteSnapshotJSON)); err != nil {
		t.Fatal(err)
	}
}

// Review Focus 5: unknown fields and hostile strings pass through
// unchanged.
func TestRemoteSnapshotPassthroughKeepsUnknownFields(t *testing.T) {
	r, srv := remoteServer(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(remoteSnapshotJSON)) })
	a := NewAggregator([]config.RemoteStatusInstanceConfig{r})
	a.Collect(context.Background(), r, srv.Client(), discardLogger(), clock(t0))

	b, err := marshalJSON(a.Instances(t0))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Instances []struct {
			Snapshot json.RawMessage `json:"snapshot"`
		} `json:"instances"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	jsonEqual(t, decode(t, doc.Instances[0].Snapshot), decode(t, []byte(remoteSnapshotJSON)))
	for _, s := range []string{`"futureField"`, `"futureTunnelField"`, `<script>alert(\"x\")</script> & 'q'`} {
		if !bytes.Contains(b, []byte(s)) {
			t.Errorf("missing %s in %s", s, b)
		}
	}

	// The typed snapshot (for the dashboard) is parsed too.
	snap := a.View().Remotes[0].Snapshot
	if snap == nil || snap.InstanceID != "edge-eu" || len(snap.Subscriptions) != 1 ||
		snap.Subscriptions[0].Targets[0].Tunnels[0].DisplayName != `<script>alert("x")</script> & 'q'` ||
		snap.Tunnels[0].LastError != "<b>" {
		t.Fatalf("typed snapshot %+v", snap)
	}
}

// A value of an unexpected type in an unchecked field is passed through
// and does not reject the snapshot (TS checks only the listed fields).
func TestRemoteSnapshotToleratesUncheckedFieldTypes(t *testing.T) {
	body := strings.Replace(remoteSnapshotJSON, `"lastLatencyMs": 64`, `"lastLatencyMs": "fast"`, 1)
	snap, err := parseRemoteSnapshot([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if snap.Subscriptions[0].Targets[0].Tunnels[0].LastLatencyMs != nil || snap.Tunnels[0].State != "degraded" {
		t.Fatalf("%+v", snap)
	}
}

// The remote timeout covers reading the body (03 §7).
func TestCollectTimeoutCoversBody(t *testing.T) {
	release := make(chan struct{})
	r, srv := remoteServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"version":1,`))
		w.(http.Flusher).Flush()
		<-release
	})
	defer close(release)
	r.TimeoutMs = 100
	a := NewAggregator([]config.RemoteStatusInstanceConfig{r})
	start := time.Now()
	a.Collect(context.Background(), r, srv.Client(), discardLogger(), clock(t0))
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("collect took %v", d)
	}
	if got := a.View().Remotes[0]; got.LastError == "" || got.Snapshot != nil {
		t.Fatalf("%+v", got)
	}
}

func TestInstancesOrderLocalFirstRemotesByID(t *testing.T) {
	a := NewAggregator([]config.RemoteStatusInstanceConfig{
		{ID: "edge-us", APIURL: "https://edge-us.example.test/api/instance-status"},
		{ID: "edge-eu", APIURL: "https://edge.example.test/api/instance-status"},
	})
	a.StoreLocal(InstanceSnapshot{
		Version: 1, InstanceID: "local-main", CapturedAt: "2026-08-15T10:00:00.000Z",
		Subscriptions: []GroupedSubscription{}, Tunnels: []StatusTunnel{},
	})
	got := instancesJSON(t, a, t0.Add(time.Minute))
	if got["capturedAt"] != "2026-08-15T10:01:00.000Z" {
		t.Fatalf("capturedAt %v", got["capturedAt"])
	}
	var ids []string
	for _, i := range got["instances"].([]any) {
		ids = append(ids, i.(map[string]any)["id"].(string))
	}
	if strings.Join(ids, ",") != "local-main,edge-eu,edge-us" {
		t.Fatalf("ids %v", ids)
	}
	local := got["instances"].([]any)[0].(map[string]any)
	if local["source"] != "local" || local["lastSuccessAt"] != "2026-08-15T10:00:00.000Z" {
		t.Fatalf("local %v", local)
	}
	us := got["instances"].([]any)[2].(map[string]any)
	if len(us) != 3 {
		t.Fatalf("placeholder %v", us)
	}
}

// Final review M4: a collection cut short by shutdown records no error and
// logs no failure.
func TestCollectCanceledIsNotAFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	r, srv := remoteServer(t, func(_ http.ResponseWriter, req *http.Request) {
		cancel()
		<-req.Context().Done()
	})
	var logs bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logs, nil))
	a := NewAggregator([]config.RemoteStatusInstanceConfig{r})
	a.Collect(ctx, r, srv.Client(), log, clock(t0))

	got := instancesJSON(t, a, t0)["instances"].([]any)[0].(map[string]any)
	if _, ok := got["lastError"]; ok {
		t.Errorf("lastError set on cancel: %v", got)
	}
	if strings.Contains(logs.String(), "remote_status_collection_failed") {
		t.Errorf("failure logged on cancel: %s", logs.String())
	}
}
