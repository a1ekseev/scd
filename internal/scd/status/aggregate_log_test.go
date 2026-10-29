package status

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/a1ekseev/scd/internal/scd/config"
)

// logLevels returns "LEVEL event" of every JSON log line of buf.
func logLevels(t *testing.T, buf *bytes.Buffer) []string {
	t.Helper()
	var out []string
	for line := range strings.Lines(buf.String()) {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatal(err)
		}
		out = append(out, m["level"].(string)+" "+m["event"].(string))
	}
	return out
}

// The same collection failure twice is one warn line, then debug; the
// next success logs remote_status_recovered.
func TestCollectFailureEdge(t *testing.T) {
	var fail atomic.Bool
	r, srv := remoteServer(t, func(w http.ResponseWriter, _ *http.Request) {
		if fail.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(remoteSnapshotJSON))
	})
	var logs bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	a := NewAggregator([]config.RemoteStatusInstanceConfig{r})
	now := clock(t0)
	fail.Store(true)
	a.Collect(t.Context(), r, srv.Client(), log, now)
	a.Collect(t.Context(), r, srv.Client(), log, now)
	fail.Store(false)
	a.Collect(t.Context(), r, srv.Client(), log, now)
	a.Collect(t.Context(), r, srv.Client(), log, now)
	want := []string{
		"WARN remote_status_collection_failed", "DEBUG remote_status_collection_failed",
		"INFO remote_status_recovered", "DEBUG remote_status_collected", "DEBUG remote_status_collected",
	}
	if got := logLevels(t, &logs); !slices.Equal(got, want) {
		t.Fatalf("log %q, want %q", got, want)
	}
	if strings.Contains(logs.String(), "/api/instance-status") {
		t.Fatalf("the URL path is in the log: %s", logs.String())
	}
}

// Guard (B1): the path, query and userinfo of a remote apiUrl reach
// neither the log nor lastError of /api/instances, for a closed port, a
// timeout and a bad status.
func TestCollectRedactsAPIURL(t *testing.T) {
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closed := ln.Addr().String()
	_ = ln.Close()
	block := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-block:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() { close(block); slow.Close() })
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadGateway) }))
	t.Cleanup(bad.Close)

	var logs bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	var texts []string
	for _, host := range []string{closed, strings.TrimPrefix(slow.URL, "http://"), strings.TrimPrefix(bad.URL, "http://")} {
		r := config.RemoteStatusInstanceConfig{
			ID: "edge", APIURL: "http://user:Pass-SECRET@" + host + "/secret-path?token=SECRET", Schedule: "* * * * *", TimeoutMs: 200,
		}
		a := NewAggregator([]config.RemoteStatusInstanceConfig{r})
		a.Collect(context.Background(), r, http.DefaultClient, log, clock(t0))
		inst := instancesJSON(t, a, t0)["instances"].([]any)[0].(map[string]any)
		if inst["lastError"] == nil {
			t.Fatalf("no lastError for %s", host)
		}
		texts = append(texts, inst["lastError"].(string))
		for _, rm := range a.View().Remotes {
			texts = append(texts, rm.LastError)
		}
	}
	all := logs.String() + strings.Join(texts, "\n")
	for _, secret := range []string{"secret-path", "SECRET", "user:"} {
		if strings.Contains(all, secret) {
			t.Errorf("%q leaks:\n%s", secret, all)
		}
	}
	if !strings.Contains(all, "<redacted>") || strings.Count(logs.String(), "remote_status_collection_failed") != 3 {
		t.Errorf("log:\n%s", all)
	}
}
