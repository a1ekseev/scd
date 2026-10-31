package syncer

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/a1ekseev/scd/internal/scd/config"
)

// dependsSub has sources "a" (/a, ports 20000..20010), which depends on
// "z" (/z, ports 20100..20110): id order and dependsOn order differ.
func dependsSub() config.SubscriptionConfig {
	sub := twoSourceSub()
	sub.Sources[1].ID, sub.Sources[1].Input = "z", "/z"
	sub.Sources[0].DependsOn = []config.DependencyConfig{{Source: "z", Health: config.HealthBalancer, RecoverGraceSeconds: 180}}
	return sub
}

// firstRPCOf returns the index of the first RPC on a resource of source id,
// -1 without one.
func firstRPCOf(calls []string, id string) int {
	return slices.IndexFunc(calls, func(c string) bool { return touchesSource(c, id) })
}

// Every RPC of the dependency comes before the first of its dependent.
func TestSyncInDependsOnOrder(t *testing.T) {
	f := newSourcesFixture(t, dependsSub(), map[string]string{"/a": vienna, "/z": berlin})
	r := f.run(t)
	if r.Failed != 0 || len(r.Targets) != 2 || r.Targets[0].SourceID != "z" || r.Targets[1].SourceID != "a" {
		t.Fatalf("report %+v", r.Targets)
	}
	calls := f.x.servers[addrA].Calls()
	firstA := firstRPCOf(calls, "a")
	lastZ := -1
	for i, c := range calls {
		if touchesSource(c, "z") {
			lastZ = i
		}
	}
	if firstA < 0 || lastZ < 0 || lastZ > firstA {
		t.Fatalf("z after a: %q", calls)
	}
}

// A dependency that was never applied blocks its dependent (no RPC, report
// failed, source_check_failed error once, then debug); once it is applied
// the dependent follows (source_check_recovered). A dependency that fails
// to load later, with its state kept, does not block it.
func TestDependencyNotApplied(t *testing.T) {
	f := newSourcesFixture(t, dependsSub(), map[string]string{"/a": vienna, "/z": berlin})
	srv := f.x.servers[addrA]
	f.in.fail["/z"] = errors.New("boom")
	for range 2 {
		r := f.run(t)
		if len(r.Targets) != 2 || r.Targets[0].SourceID != "z" || r.Targets[0].Failed == 0 ||
			r.Targets[1].SourceID != "a" || r.Targets[1].Failed == 0 ||
			!strings.HasPrefix(r.Targets[1].Resources[0].Items[0].Message, "dependency_not_applied: ") {
			t.Fatalf("report %+v", r.Targets)
		}
	}
	if i := firstRPCOf(srv.Calls(), "a"); i >= 0 {
		t.Fatalf("RPC on the blocked source: %q", srv.Calls())
	}
	ev := f.logs.events(t, "source_check_failed")
	if len(ev) != 2 || ev[0]["level"] != "ERROR" || ev[1]["level"] != "DEBUG" || ev[0]["sourceId"] != "a" ||
		ev[0]["reasonCode"] != CodeDependencyNotApplied || ev[0]["error"] != `dependency "z" is not applied to the target` ||
		ev[0]["subscriptionId"] != "sub" || ev[0]["targetAddress"] != addrA {
		t.Fatalf("source_check_failed %v", ev)
	}
	if e := f.logs.events(t, "apply_failed"); len(e) != 0 {
		t.Errorf("apply_failed %v", e)
	}

	delete(f.in.fail, "/z")
	if r := f.run(t); r.Failed != 0 {
		t.Fatalf("report %+v", r.Targets)
	}
	if firstRPCOf(srv.Calls(), "a") < 0 {
		t.Fatal("a not applied")
	}
	if e := f.logs.events(t, "source_check_recovered"); len(e) != 1 || e[0]["sourceId"] != "a" || e[0]["level"] != "INFO" {
		t.Fatalf("source_check_recovered %v", e)
	}

	n := len(srv.Calls())
	f.in.fail["/z"] = errors.New("boom")
	f.in.set("/a", vienna+"\n"+berlin)
	r := f.run(t)
	if r.Targets[0].SourceID != "z" || r.Targets[0].Failed == 0 || r.Targets[1].Failed != 0 {
		t.Fatalf("report %+v", r.Targets)
	}
	if firstRPCOf(srv.Calls()[n:], "a") < 0 {
		t.Fatal("a not applied while z keeps its state")
	}
}
