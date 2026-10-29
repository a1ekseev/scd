package refresh

import (
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/a1ekseev/scd/internal/sfc/cache"
)

// levels returns "LEVEL outputName|subscriptionId" of every record of event.
func (l *logRecorder) levels(t *testing.T, event string) []string {
	t.Helper()
	var out []string
	for _, e := range l.events(t, event) {
		who, _ := e["outputName"].(string)
		if who == "" {
			who, _ = e["subscriptionId"].(string)
		}
		out = append(out, e["level"].(string)+" "+who)
	}
	return out
}

// An empty output is one warn line while it stays empty; the first good
// refresh logs output_recovered. The output id never appears.
func TestOutputFailureEdge(t *testing.T) {
	cfg := loadConfig(t, baseConfig)
	src := &source{content: lineDE}
	r, rec := newRefresher(t, cfg, src)
	now := fixedNow
	r.Now = func() time.Time { return now }
	r.Run(t.Context())
	src.set(lineNL, nil)
	for range 2 {
		now = now.Add(time.Minute)
		r.Run(t.Context())
	}
	src.set(lineDE, nil)
	now = now.Add(time.Minute)
	rep := r.Run(t.Context())
	if got := rec.levels(t, "output_refresh_failed"); !slices.Equal(got, []string{"WARN germany", "DEBUG germany"}) {
		t.Errorf("output_refresh_failed %v", got)
	}
	ev := rec.events(t, "output_refresh_failed")
	if ev[0]["keptPrevious"] != true || ev[0]["error"] != errEmptyOutput || ev[0]["subscriptionId"] != "main" {
		t.Errorf("output_refresh_failed %v", ev[0])
	}
	recovered := rec.events(t, "output_recovered")
	if len(recovered) != 1 || recovered[0]["outputName"] != "germany" || recovered[0]["failedForMs"] != 120000.0 || rep.Failed != 0 {
		t.Errorf("output_recovered %v", recovered)
	}
	refreshed := rec.events(t, "subscription_refreshed")
	if got := refreshed[1]["failedOutputs"]; len(got.([]any)) != 1 || got.([]any)[0] != "germany" {
		t.Errorf("failedOutputs %v", got)
	}
	if _, ok := refreshed[0]["durationMs"]; !ok {
		t.Errorf("durationMs missing: %v", refreshed[0])
	}
	if strings.Contains(rec.buf.String(), "out-germany") || strings.Contains(rec.buf.String(), "out-europe") {
		t.Errorf("an output id is in the log:\n%s", rec.buf.String())
	}
}

// A load failure without cache is one error line per series, then debug;
// the next good load logs subscription_refresh_recovered only (no
// output_recovered: the outputs failed with the load).
func TestRefreshFailureEdge(t *testing.T) {
	cfg := loadConfig(t, baseConfig)
	src := &source{err: errors.New("network down")}
	r, rec := newRefresher(t, cfg, src)
	now := fixedNow
	r.Now = func() time.Time { return now }
	r.Run(t.Context())
	now = now.Add(time.Minute)
	r.Run(t.Context())
	src.set(lineDE+"\n"+lineNL, nil)
	now = now.Add(time.Minute)
	r.Run(t.Context())
	if got := rec.levels(t, "subscription_refresh_failed"); !slices.Equal(got, []string{"ERROR main", "DEBUG main"}) {
		t.Errorf("subscription_refresh_failed %v", got)
	}
	ev := rec.events(t, "subscription_refresh_recovered")
	if len(ev) != 1 || ev[0]["failedForMs"] != 120000.0 || ev[0]["source"] != "https://sub.example.com/<redacted>" {
		t.Errorf("subscription_refresh_recovered %v", ev)
	}
	if got := rec.levels(t, "output_recovered"); len(got) != 0 {
		t.Errorf("output_recovered after a load failure: %v", got)
	}
	if n := len(rec.events(t, "output_refresh_failed")); n != 0 {
		t.Errorf("output_refresh_failed for a load failure: %d", n)
	}
}

// With a cache, the same load failure is one warn line, then debug; the
// cache disappearing is a new state and logged at error.
func TestCacheUsedEdge(t *testing.T) {
	cfg := loadConfig(t, baseConfig)
	src := &source{content: lineDE}
	r, rec := newRefresher(t, cfg, src)
	r.Run(t.Context())
	src.set("", errors.New("network down"))
	r.Run(t.Context())
	r.Run(t.Context())
	if err := os.Remove(cache.Path(cfg.Path, "main")); err != nil {
		t.Fatal(err)
	}
	r.Run(t.Context())
	if got := rec.levels(t, "subscription_source_cache_used"); !slices.Equal(got, []string{"WARN main", "DEBUG main"}) {
		t.Errorf("cache_used %v", got)
	}
	if got := rec.levels(t, "subscription_refresh_failed"); !slices.Equal(got, []string{"ERROR main"}) {
		t.Errorf("refresh_failed %v", got)
	}
	if got := rec.levels(t, "subscription_source_cache_written"); !slices.Equal(got, []string{"DEBUG main"}) {
		t.Errorf("cache_written %v", got)
	}
}
