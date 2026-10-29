package state

import (
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/a1ekseev/scd/internal/scd/apply"
	"github.com/a1ekseev/scd/internal/scd/topology"
)

func topo(ids ...string) topology.Topology {
	var t topology.Topology
	for i, id := range ids {
		t.Tunnels = append(t.Tunnels, topology.Tunnel{
			BaseTunnelID: id, OutboundTagInitial: "x-" + id, OutboundTagCurrent: "x-" + id,
			InboundTag: "in-" + id, RouteTag: "route-" + id, Port: 20000 + i,
		})
	}
	return t
}

func TestKey(t *testing.T) {
	if got := Key("sub-1", "127.0.0.1:8080"); got != "sub-1::127.0.0.1:8080" {
		t.Fatalf("Key = %q", got)
	}
}

func TestEnsureInitialState(t *testing.T) {
	m := New()
	m.Ensure("k")
	m.Ensure("k") // idempotent
	ok := m.View("k", func(tg *Target) {
		if tg.Generation != 0 || tg.Sources == nil || len(tg.Sources) != 0 || len(tg.Tunnels) != 0 {
			t.Errorf("target = %+v, want empty", tg)
		}
		if tg.Balancer.State != BalancerIdle || tg.Balancer.RemotePingState != RemotePingIdle {
			t.Errorf("balancer = %+v, want idle/idle", tg.Balancer)
		}
	})
	if !ok {
		t.Fatal("View after Ensure = false")
	}
	if got := m.Keys(); len(got) != 1 || got[0] != "k" {
		t.Fatalf("Keys = %v", got)
	}
}

func TestReplaceSourceTopologyResetsMonitorsKeepsBalancer(t *testing.T) {
	m := New()
	m.Ensure("k")
	m.ReplaceSourceTopology("k", "vless", topo("b", "a"), map[apply.Kind]string{apply.KindOutbound: "h1"})
	now := time.Now()
	m.Update("k", func(tg *Target) {
		tg.Tunnels[0].Monitor = TunnelMonitor{State: TunnelDegraded, LastFailureAt: &now, LastError: "x", ConsecutiveFailures: 3}
		tg.Tunnels[1].Monitor.State = TunnelHealthy
		tg.Balancer.State = BalancerHealthy
		tg.Balancer.RemotePingState = RemotePingOK
		tg.Balancer.ConsecutiveFailures = 2
	})

	m.ReplaceSourceTopology("k", "vless", topo("c", "b"), map[apply.Kind]string{apply.KindOutbound: "h2", apply.KindRouting: "r2"})

	m.View("k", func(tg *Target) {
		if tg.Generation != 2 {
			t.Errorf("Generation = %d, want 2", tg.Generation)
		}
		src := tg.Sources["vless"]
		if len(src.Hashes) != 2 || src.Hashes[apply.KindOutbound] != "h2" || src.Hashes[apply.KindRouting] != "r2" {
			t.Errorf("Hashes = %v", src.Hashes)
		}
		if src.Topology == nil || len(src.Topology.Tunnels) != 2 || src.Topology.Tunnels[0].SourceID != "vless" {
			t.Fatalf("Topology = %+v", src.Topology)
		}
		if len(tg.Tunnels) != 2 || tg.Tunnels[0].Mapping.BaseTunnelID != "c" || tg.Tunnels[1].Mapping.BaseTunnelID != "b" {
			t.Fatalf("Tunnels order = %+v, want topology order c, b", tg.Tunnels)
		}
		for _, tn := range tg.Tunnels {
			if !reflect.DeepEqual(tn.Monitor, TunnelMonitor{State: TunnelIdle}) {
				t.Errorf("tunnel %s monitor = %+v, want idle", tn.Mapping.BaseTunnelID, tn.Monitor)
			}
		}
		want := BalancerMonitor{State: BalancerHealthy, RemotePingState: RemotePingOK, ConsecutiveFailures: 2}
		if tg.Balancer != want {
			t.Errorf("Balancer = %+v, want untouched %+v", tg.Balancer, want)
		}
	})
}

func TestReplaceSourceTopologyCopiesHashes(t *testing.T) {
	m := New()
	h := map[apply.Kind]string{apply.KindOutbound: "h1"}
	m.ReplaceSourceTopology("k", "vless", topo("a"), h)
	h[apply.KindOutbound] = "changed"
	m.View("k", func(tg *Target) {
		if tg.Sources["vless"].Hashes[apply.KindOutbound] != "h1" {
			t.Errorf("Hashes aliased the caller's map: %v", tg.Sources["vless"].Hashes)
		}
	})
}

func TestUpdateMissingTargetFalse(t *testing.T) {
	m := New()
	called := false
	if m.Update("missing", func(*Target) { called = true }) {
		t.Error("Update on a missing target = true")
	}
	if m.View("missing", func(*Target) { called = true }) {
		t.Error("View on a missing target = true")
	}
	if called {
		t.Error("fn called for a missing target")
	}
	if len(m.Keys()) != 0 {
		t.Error("Update/View created a target")
	}
}

// TestLockTargetMutualExclusion uses real time: goroutines blocked on a
// sync.Mutex are not durably blocked for testing/synctest, so its fake
// clock would never advance.
func TestLockTargetMutualExclusion(t *testing.T) {
	m := New()
	var active, maxActive atomic.Int32
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			unlock := m.LockTarget("k")
			defer unlock()
			n := active.Add(1)
			for {
				cur := maxActive.Load()
				if n <= cur || maxActive.CompareAndSwap(cur, n) {
					break
				}
			}
			time.Sleep(10 * time.Millisecond)
			active.Add(-1)
		})
	}
	wg.Wait()
	if got := maxActive.Load(); got != 1 {
		t.Errorf("max concurrent holders = %d, want 1", got)
	}

	// Another key is not blocked while "k" is held.
	unlock := m.LockTarget("k")
	done := make(chan struct{})
	go func() {
		m.LockTarget("other")()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("lock of another key was blocked by k")
	}
	unlock()
}

func TestTryStartSyncOverlap(t *testing.T) {
	m := New()
	if !m.TryStartSync() {
		t.Fatal("first TryStartSync = false")
	}
	if m.TryStartSync() {
		t.Fatal("overlapping TryStartSync = true")
	}
	m.EndSync()
	if !m.TryStartSync() {
		t.Fatal("TryStartSync after EndSync = false")
	}
}

// SyncSeen means "a sync reached this target" (03 §1, review 6.6): Ensure
// (e.g. by the balancer selection tick) does not set it; MarkSynced and
// ReplaceSourceTopology do.
func TestMarkSynced(t *testing.T) {
	m := New()
	seen := func(key string) bool {
		var s bool
		m.View(key, func(tg *Target) { s = tg.SyncSeen })
		return s
	}
	m.Ensure("a")
	if seen("a") {
		t.Error("Ensure set SyncSeen")
	}
	m.Update("a", func(tg *Target) { tg.BalancerSelection.State = SelectionOK })
	m.MarkSynced("a")
	if !seen("a") {
		t.Error("MarkSynced did not set SyncSeen")
	}
	m.View("a", func(tg *Target) {
		if tg.BalancerSelection.State != SelectionOK {
			t.Error("MarkSynced reset the target")
		}
	})
	m.MarkSynced("b") // creates
	if !seen("b") {
		t.Error("MarkSynced on a new key")
	}
	m.ReplaceSourceTopology("c", "vless", topo("x"), nil)
	if !seen("c") {
		t.Error("ReplaceSourceTopology did not set SyncSeen")
	}
}

// A source replacement keeps the other sources' tunnels and monitor state;
// tunnels are ordered by source id, then topology order.
func TestReplaceSourceTopologyKeepsOtherSources(t *testing.T) {
	m := New()
	m.ReplaceSourceTopology("k", "b", topo("b2", "b1"), map[apply.Kind]string{apply.KindOutbound: "hb"})
	m.ReplaceSourceTopology("k", "a", topo("a1"), map[apply.Kind]string{apply.KindOutbound: "ha"})
	m.Update("k", func(tg *Target) { tg.Tunnels[1].Monitor.State = TunnelDegraded })
	m.ReplaceSourceTopology("k", "c", topo("c1"), nil)
	m.ReplaceSourceTopology("k", "a", topo("a2", "a1"), map[apply.Kind]string{apply.KindOutbound: "ha2"})
	order := func(tg *Target) (out []string) {
		for _, tn := range tg.Tunnels {
			out = append(out, tn.Mapping.SourceID+"/"+tn.Mapping.BaseTunnelID)
		}
		return out
	}
	m.View("k", func(tg *Target) {
		if got := order(tg); len(got) != 5 || got[0] != "a/a2" || got[1] != "a/a1" || got[2] != "b/b2" || got[3] != "b/b1" || got[4] != "c/c1" {
			t.Fatalf("order %v", got)
		}
		if tg.Tunnels[2].Monitor.State != TunnelDegraded || tg.Generation != 4 {
			t.Fatalf("source b monitor %+v generation %d", tg.Tunnels[2].Monitor, tg.Generation)
		}
		if tg.Sources["b"].Hashes[apply.KindOutbound] != "hb" || tg.Sources["a"].Hashes[apply.KindOutbound] != "ha2" {
			t.Fatalf("hashes %+v", tg.Sources)
		}
	})
	m.DropSource("k", "b")
	m.DropSource("k", "zz")
	m.View("k", func(tg *Target) {
		if got := order(tg); len(got) != 3 || got[2] != "c/c1" || tg.Sources["b"] != nil || tg.Generation != 5 {
			t.Fatalf("after drop %v %d", got, tg.Generation)
		}
		if ids := tg.SourceIDs(); len(ids) != 2 || ids[0] != "a" || ids[1] != "c" {
			t.Fatalf("SourceIDs %v", ids)
		}
	})
}

// A full apply keeps FailingSince and Flap of a tunnel of the same tag
// (logging only), not of a new tag, and none after a lost Xray state.
func TestReplaceSourceTopologyCarriesFailing(t *testing.T) {
	for _, lost := range []bool{false, true} {
		m := New()
		m.ReplaceSourceTopology("k", "vless", topo("a", "b"), nil)
		since := time.Unix(100, 0)
		m.Update("k", func(tg *Target) {
			for _, tn := range tg.Tunnels {
				tn.Monitor = TunnelMonitor{State: TunnelDegraded, FailingSince: &since, Flap: FlapState{Since: since, Count: 3}}
			}
			if lost {
				tg.SetStateLost("vless", since)
			}
		})
		m.ReplaceSourceTopology("k", "vless", topo("a", "c"), nil)
		m.View("k", func(tg *Target) {
			a, c := tg.Tunnels[0].Monitor, tg.Tunnels[1].Monitor
			carried := a.FailingSince != nil && a.Flap.Count == 3 && a.State == TunnelIdle
			if carried == lost || c.FailingSince != nil || c.Flap.Count != 0 {
				t.Errorf("lost=%v: a %+v, c %+v", lost, a, c)
			}
		})
	}
}

func TestXrayMarkers(t *testing.T) {
	var tg Target
	t0 := time.Unix(100, 0)
	if !tg.SetXrayDown("a", t0) || tg.SetXrayDown("b", t0.Add(time.Second)) || tg.XrayDown.Error != "b" {
		t.Fatalf("XrayDown %+v", tg.XrayDown)
	}
	if d, was := tg.ClearXrayDown(t0.Add(time.Minute)); !was || d != time.Minute || tg.XrayDown != nil {
		t.Fatalf("ClearXrayDown %v %v", d, was)
	}
	if _, was := tg.ClearXrayDown(t0); was {
		t.Fatal("cleared twice")
	}
	if !tg.SetStateLost("a", t0) || tg.SetStateLost("b", t0.Add(time.Second)) || tg.SetStateLost("a", t0.Add(time.Hour)) {
		t.Fatal("SetStateLost")
	}
	if _, ok := tg.ClearStateLost("a", t0, t0); ok || len(tg.StateLost) != 2 {
		t.Fatal("a mark at before is not covered")
	}
	if _, ok := tg.ClearStateLost("a", t0.Add(time.Second), t0); ok || len(tg.StateLost) != 1 {
		t.Fatal("restored while b is marked")
	}
	if d, ok := tg.ClearStateLost("b", t0.Add(time.Hour), t0.Add(time.Minute)); !ok || d != time.Minute || len(tg.StateLost) != 0 {
		t.Fatalf("ClearStateLost %v %v", d, ok)
	}
	if _, ok := tg.ClearStateLost("b", t0.Add(time.Hour), t0); ok {
		t.Fatal("cleared twice")
	}
	// A source stays marked (it never loads): a new loss after a partial
	// restore is first again.
	tg.SetStateLost("a", t0)
	if tg.SetStateLost("b", t0) {
		t.Fatal("second source of an episode is first")
	}
	if _, ok := tg.ClearStateLost("a", t0.Add(time.Second), t0); ok || tg.SetStateLost("b", t0) {
		t.Fatal("b already marked")
	}
	if !tg.SetStateLost("a", t0.Add(time.Hour)) {
		t.Fatal("new loss next to a stuck source is not first")
	}
}
