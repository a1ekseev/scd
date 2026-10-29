package syncer

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/a1ekseev/scd/internal/scd/state"
	"github.com/a1ekseev/scd/internal/scd/topology"
	"github.com/a1ekseev/scd/internal/scd/xrayapi"
	"github.com/a1ekseev/scd/internal/scd/xrayapi/xrayfake"
)

// levels returns the levels of the event in log order.
func (l *lockedBuffer) levels(t *testing.T, event string) []string {
	t.Helper()
	var out []string
	for _, e := range l.events(t, event) {
		out = append(out, e["level"].(string))
	}
	return out
}

// sync_finished closes every sync with the run counters.
func TestSyncFinished(t *testing.T) {
	l := cfg(allKinds, sub("source-a", "/a", target(addrA)), sub("source-b", "/b", target(addrB)))
	f := newFixture(t, l, map[string]string{"/a": vienna + "\n" + trojan}, addrA, addrB)
	f.in.fail["/b"] = errors.New("Input source timed out")
	f.run(t)
	f.run(t)
	ev := f.logs.events(t, "sync_finished")
	if len(ev) != 2 {
		t.Fatalf("sync_finished = %v", ev)
	}
	first, second := ev[0], ev[1]
	if first["level"] != "INFO" || first["sourcesApplied"] != 1.0 || first["sourcesFailed"] != 1.0 || first["sourcesUnchanged"] != 0.0 ||
		first["resourcesAdded"] != 1.0 || first["skippedLines"] != 1.0 || first["msg"] != "Sync finished." ||
		!reflect.DeepEqual(first["appliedSourceIds"], []any{"source-a/vless"}) {
		t.Errorf("first = %v", first)
	}
	for _, k := range []string{"canceled", "added", "failed"} {
		if _, ok := first[k]; ok {
			t.Errorf("%s in sync_finished: %v", k, first)
		}
	}
	if _, ok := second["appliedSourceIds"]; ok || second["sourcesApplied"] != 0.0 || second["sourcesUnchanged"] != 1.0 || second["resourcesAdded"] != 0.0 {
		t.Errorf("second = %v", second)
	}
	for _, e := range []string{"sync_started", "subscription_fetched", "manifest_built", "skipped_no_changes", "resource_apply_started"} {
		for _, lv := range f.logs.levels(t, e) {
			if lv != "DEBUG" {
				t.Errorf("%s at %s", e, lv)
			}
		}
	}
	// apply_finished is debug: sync_finished names the applied sources.
	if got := f.logs.levels(t, "apply_finished"); !slices.Equal(got, []string{"DEBUG", "DEBUG", "DEBUG"}) {
		t.Errorf("apply_finished levels %v", got)
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := f.s.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if ev := f.logs.events(t, "sync_finished"); ev[2]["canceled"] != PhaseLoad {
		t.Errorf("canceled run = %v", ev[2])
	}
}

// A repeated fetch failure is one error line, then debug; the first good
// load logs subscription_fetch_recovered with failedForMs.
func TestFetchFailureEdge(t *testing.T) {
	f := newFixture(t, cfg(outboundOnly, sub("source-1", "/s1", target(addrA))), map[string]string{"/s1": vienna}, addrA)
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	f.s.Now = func() time.Time { return now }
	f.in.fail["/s1"] = errors.New("Input source timed out")
	f.run(t)
	now = now.Add(time.Minute)
	f.run(t)
	f.in.fail["/s1"] = errors.New("Input source returned 503")
	f.run(t)
	delete(f.in.fail, "/s1")
	now = now.Add(time.Minute)
	f.run(t)
	f.run(t)
	if got := f.logs.levels(t, "subscription_fetch_failed"); !slices.Equal(got, []string{"ERROR", "DEBUG", "ERROR"}) {
		t.Errorf("subscription_fetch_failed levels %v", got)
	}
	rec := f.logs.events(t, "subscription_fetch_recovered")
	if len(rec) != 1 || rec[0]["level"] != "INFO" || rec[0]["failedForMs"] != 120000.0 || rec[0]["sourceId"] != "vless" ||
		rec[0]["subscriptionId"] != "source-1" || rec[0]["targetAddress"] != addrA {
		t.Errorf("recovered %v", rec)
	}
	if n := len(f.logs.events(t, "apply_failed")); n != 0 {
		t.Errorf("apply_failed for a fetch failure: %d", n)
	}
}

// apply_failed is one error line per source while its text stays the same;
// a success resets it.
func TestApplyFailedEdge(t *testing.T) {
	f := newFixture(t, cfg(outboundOnly, sub("source-1", "/s1", target(addrA))), map[string]string{"/s1": vienna}, addrA)
	fail := func() { f.x.servers[addrA].FailNext("AddOutbound", status.Error(codes.Unknown, "rejected")) }
	fail()
	f.run(t)
	fail()
	f.run(t) // the hashes were not saved: applied again, same error
	f.run(t) // succeeds
	f.in.set("/s1", berlin)
	fail()
	f.run(t)
	if got := f.logs.levels(t, "apply_failed"); !slices.Equal(got, []string{"ERROR", "DEBUG", "ERROR"}) {
		t.Errorf("apply_failed levels %v", got)
	}
}

// A dial failure of the apply fails every kind but logs one error line.
func TestApplyDialFailureOneLine(t *testing.T) {
	f := newFixture(t, cfg(allKinds, sub("source-1", "/s1", target(addrA))), map[string]string{"/s1": vienna}, addrA)
	dials := 0
	dial := f.s.Dial
	f.s.Dial = func(addr string, timeout time.Duration) (xrayapi.Client, error) {
		if dials++; dials == 2 { // the orphan check dials first
			return nil, errors.New("Unable to connect to Xray API")
		}
		return dial(addr, timeout)
	}
	if r := f.run(t); r.Failed != 3 {
		t.Fatalf("failed %d", r.Failed)
	}
	if got := f.logs.levels(t, "apply_failed"); !slices.Equal(got, []string{"ERROR", "DEBUG", "DEBUG"}) {
		t.Errorf("apply_failed levels %v", got)
	}
}

// manifest_entry_warning is warn while the warnings of the source change,
// then debug; port_range_exhausted is one line with the count.
func TestEntryWarningEdge(t *testing.T) {
	const wsVision = "vless://7d1b6590-1069-4372-92be-8d0a0ae6eaf1@example.com:443?type=ws&security=tls&path=%2Fws&flow=xtls-rprx-vision#🇦🇹 Вена WS"
	l := cfg(allKinds, socksSub("source-1", "/s1", target(addrA), 20000, 20000))
	f := newFixture(t, l, map[string]string{"/s1": wsVision + "\n" + berlin + "\n" + vienna}, addrA)
	f.run(t)
	f.run(t)
	f.in.set("/s1", wsVision)
	f.run(t)
	var got []string
	for _, e := range f.logs.events(t, "manifest_entry_warning") {
		s := e["level"].(string) + " " + e["code"].(string)
		if e["code"] == topology.CodePortRangeExhausted {
			if e["count"] != 2.0 || e["line"] != 1.0 {
				t.Errorf("port range %v", e)
			}
		}
		got = append(got, s)
	}
	want := []string{
		"WARN flow_cleared", "WARN " + topology.CodePortRangeExhausted,
		"DEBUG flow_cleared", "DEBUG " + topology.CodePortRangeExhausted,
		"DEBUG flow_cleared",
	}
	if !slices.Equal(got, want) {
		t.Errorf("warnings %q, want %q", got, want)
	}
}

// The same orphan removal failure on two syncs is one error line, then
// debug; the first good removal logs orphan_remove_recovered.
func TestOrphanRemovalFailureEdge(t *testing.T) {
	f := newSourcesFixture(t, twoSourceSub(), map[string]string{"/a": vienna, "/b": berlin},
		xrayfake.WithStaticOutbounds("direct", "old-1"))
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	f.s.Now = func() time.Time { return now }
	srv := f.x.servers[addrA]
	for range 2 {
		srv.FailNext("RemoveOutbound", status.Error(codes.Unknown, "boom"))
		f.run(t)
		now = now.Add(time.Minute)
	}
	f.run(t)
	f.run(t)
	if got := f.logs.levels(t, "orphan_remove_failed"); !slices.Equal(got, []string{"ERROR", "DEBUG"}) {
		t.Errorf("orphan_remove_failed levels %v", got)
	}
	rec := f.logs.events(t, "orphan_remove_recovered")
	if len(rec) != 1 || rec[0]["level"] != "INFO" || rec[0]["failedForMs"] != 120000.0 || rec[0]["subscriptionId"] != twoSourceSub().ID {
		t.Errorf("orphan_remove_recovered %v", rec)
	}
}

// An unavailable Xray API in the orphan removal is one xray_unavailable
// line with orphan_remove_failed at debug; the next good sync logs
// xray_recovered and orphan_remove_recovered at debug. Another error is
// logged as before (TestOrphanRemovalFailureEdge).
func TestOrphanRemovalXrayUnavailable(t *testing.T) {
	f := newSourcesFixture(t, twoSourceSub(), map[string]string{"/a": vienna, "/b": berlin})
	srv := f.x.servers[addrA]
	srv.FailNext("ListOutbounds", status.Error(codes.Unavailable, "connection error: read tcp 10.0.0.2:5555->10.0.0.5:10085: read: connection reset by peer"))
	f.run(t)
	ev := f.logs.events(t, "xray_unavailable")
	if len(ev) != 1 || ev[0]["level"] != "ERROR" || ev[0]["error"] != "connection error: read tcp 10.0.0.5:10085: read: connection reset by peer" {
		t.Fatalf("xray_unavailable %v", ev)
	}
	if got := f.logs.levels(t, "orphan_remove_failed"); !slices.Equal(got, []string{"DEBUG"}) {
		t.Fatalf("orphan_remove_failed %v", got)
	}
	f.run(t)
	if got := f.logs.levels(t, "xray_recovered"); !slices.Equal(got, []string{"INFO"}) {
		t.Fatalf("xray_recovered %v", got)
	}
	if got := f.logs.levels(t, "orphan_remove_recovered"); !slices.Equal(got, []string{"DEBUG"}) {
		t.Fatalf("orphan_remove_recovered %v", got)
	}
}

// A failure logged only at debug (Xray unavailable) recovers at debug even
// when another caller (the monitor) logged xray_recovered first.
func TestOrphanRecoveredDebugAfterDebugFailure(t *testing.T) {
	f := newSourcesFixture(t, twoSourceSub(), map[string]string{"/a": vienna, "/b": berlin})
	f.x.servers[addrA].FailNext("ListOutbounds", status.Error(codes.Unavailable, "connection error"))
	f.run(t)
	f.s.Memory.Update(state.Key("sub", addrA), func(tg *state.Target) { tg.ClearXrayDown(time.Now()) })
	f.run(t)
	if got := f.logs.levels(t, "orphan_remove_recovered"); !slices.Equal(got, []string{"DEBUG"}) {
		t.Fatalf("orphan_remove_recovered %v", got)
	}
}

// A restarted Xray (an empty fake) is one xray_state_drift warn for the
// target, debug for its other source, and one xray_state_restored once a
// sync applied every marked source. A marked source that does not load or
// apply stays marked without the line, and the next restart is warn again.
func TestStateRestored(t *testing.T) {
	for _, fail := range []string{"", "fetch", "apply"} {
		f := newSourcesFixture(t, twoSourceSub(), map[string]string{"/a": vienna, "/b": berlin})
		key := state.Key("sub", addrA)
		f.run(t)
		restart := func() *xrayfake.Server {
			s, _ := xrayfake.Start(t)
			f.x.servers[addrA] = s
			return s
		}
		s := restart()
		switch fail {
		case "fetch":
			// The monitor found b's inbound missing; b no longer loads.
			f.s.Memory.Update(key, func(tg *state.Target) { tg.SetStateLost("b", time.Unix(0, 0)) })
			f.in.fail["/b"] = errors.New("Input source timed out")
		case "apply":
			s.FailNext("AddInbound", status.Error(codes.Unknown, "rejected"))
		}
		f.run(t)
		if fail != "fetch" {
			if got := f.logs.levels(t, "xray_state_drift"); !slices.Equal(got, []string{"WARN", "DEBUG"}) {
				t.Errorf("fail=%q: xray_state_drift %v", fail, got)
			}
		}
		restored := f.logs.events(t, "xray_state_restored")
		lost := false
		f.s.Memory.View(key, func(tg *state.Target) { lost = len(tg.StateLost) > 0 })
		if (len(restored) == 1) != (fail == "") || lost != (fail != "") {
			t.Errorf("fail=%q: restored %v, lost %v", fail, restored, lost)
		}
		if fail != "fetch" {
			continue
		}
		// b lost its resources and does not load: one warn, then debug.
		f.run(t)
		if got := f.logs.levels(t, "xray_state_restore_blocked"); !slices.Equal(got, []string{"WARN", "DEBUG"}) {
			t.Errorf("xray_state_restore_blocked %v", got)
		}
		if ev := f.logs.events(t, "xray_state_restore_blocked"); ev[0]["sourceId"] != "b" || ev[0]["error"] != "Input source timed out" {
			t.Errorf("xray_state_restore_blocked %v", ev[0])
		}
		restart()
		f.run(t)
		if got := f.logs.levels(t, "xray_state_drift"); !slices.Equal(got, []string{"DEBUG", "WARN"}) {
			t.Errorf("second restart: xray_state_drift %v", got)
		}
		// b loads again: its episode ends with xray_state_restored, and a
		// later loss logs a new warn.
		delete(f.in.fail, "/b")
		f.run(t)
		if len(f.logs.events(t, "xray_state_restored")) != 1 {
			t.Errorf("xray_state_restored %v", f.logs.events(t, "xray_state_restored"))
		}
		f.s.Memory.Update(key, func(tg *state.Target) { tg.SetStateLost("b", time.Unix(0, 0)) })
		f.in.fail["/b"] = errors.New("Input source timed out")
		f.run(t)
		if got := f.logs.levels(t, "xray_state_restore_blocked"); len(got) == 0 || got[len(got)-1] != "WARN" {
			t.Errorf("new episode: xray_state_restore_blocked %v", got)
		}
	}
}
