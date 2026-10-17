package state

import (
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
		if tg.Generation != 0 || tg.Topology != nil || len(tg.Tunnels) != 0 || len(tg.Hashes) != 0 {
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

func TestReplaceTopologyResetsMonitorsKeepsBalancer(t *testing.T) {
	m := New()
	m.Ensure("k")
	m.ReplaceTopology("k", topo("b", "a"), map[apply.Kind]string{apply.KindOutbound: "h1"})
	now := time.Now()
	m.Update("k", func(tg *Target) {
		tg.Tunnels[0].Monitor = TunnelMonitor{State: TunnelDegraded, LastFailureAt: &now, LastError: "x", ConsecutiveFailures: 3}
		tg.Tunnels[1].Monitor.State = TunnelHealthy
		tg.Balancer.State = BalancerHealthy
		tg.Balancer.RemotePingState = RemotePingOK
		tg.Balancer.ConsecutiveFailures = 2
	})

	m.ReplaceTopology("k", topo("c", "b"), map[apply.Kind]string{apply.KindOutbound: "h2", apply.KindRouting: "r2"})

	m.View("k", func(tg *Target) {
		if tg.Generation != 2 {
			t.Errorf("Generation = %d, want 2", tg.Generation)
		}
		if len(tg.Hashes) != 2 || tg.Hashes[apply.KindOutbound] != "h2" || tg.Hashes[apply.KindRouting] != "r2" {
			t.Errorf("Hashes = %v", tg.Hashes)
		}
		if tg.Topology == nil || len(tg.Topology.Tunnels) != 2 {
			t.Fatalf("Topology = %+v", tg.Topology)
		}
		if len(tg.Tunnels) != 2 || tg.Tunnels[0].Mapping.BaseTunnelID != "c" || tg.Tunnels[1].Mapping.BaseTunnelID != "b" {
			t.Fatalf("Tunnels order = %+v, want topology order c, b", tg.Tunnels)
		}
		for _, tn := range tg.Tunnels {
			if tn.Monitor != (TunnelMonitor{State: TunnelIdle}) {
				t.Errorf("tunnel %s monitor = %+v, want idle", tn.Mapping.BaseTunnelID, tn.Monitor)
			}
		}
		want := BalancerMonitor{State: BalancerHealthy, RemotePingState: RemotePingOK, ConsecutiveFailures: 2}
		if tg.Balancer != want {
			t.Errorf("Balancer = %+v, want untouched %+v", tg.Balancer, want)
		}
	})
}

func TestReplaceTopologyCopiesHashes(t *testing.T) {
	m := New()
	h := map[apply.Kind]string{apply.KindOutbound: "h1"}
	m.ReplaceTopology("k", topo("a"), h)
	h[apply.KindOutbound] = "changed"
	m.View("k", func(tg *Target) {
		if tg.Hashes[apply.KindOutbound] != "h1" {
			t.Errorf("Hashes aliased the caller's map: %v", tg.Hashes)
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
