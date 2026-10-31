package syncer

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/a1ekseev/scd/internal/scd/state"
)

// sourceError returns the error of source id of sub on addrA in state.
func sourceError(f *fixture, id string) (state.SourceError, bool) {
	var (
		e  state.SourceError
		ok bool
	)
	f.s.Memory.View(state.Key("sub", addrA), func(tg *state.Target) { e, ok = tg.SourceErrors[id] })
	return e, ok
}

// Every failure of a source in sync is in state with its code; the start
// of the episode survives the next failing sync; a successful sync clears
// it.
func TestSourceErrorsInState(t *testing.T) {
	clock := time.Unix(1_800_000_000, 0).UTC()
	f, srv := balancerSubFixture(t, outboundOnly)
	srv.AddBalancer("x-balancer", []string{"x-observe-"}, "leastPing")
	s := f.s.Config.Config.Subscriptions[0]
	f.s.Now = func() time.Time { return clock }
	step := func(code string) {
		t.Helper()
		f.run(t)
		e, ok := sourceError(f, "vless")
		if code == "" {
			if ok {
				t.Fatalf("error %+v after a successful sync", e)
			}
			return
		}
		if !ok || e.Code != code || e.Message == "" {
			t.Fatalf("error %+v, %v, want %s", e, ok, code)
		}
		clock = clock.Add(time.Minute)
	}

	f.in.fail["/s1"] = errors.New("boom")
	step(CodeFetchFailed)
	first, _ := sourceError(f, "vless")
	step(CodeFetchFailed)
	if e, _ := sourceError(f, "vless"); !e.At.Equal(first.At) {
		t.Errorf("at %v, want the start of the episode %v", e.At, first.At)
	}
	delete(f.in.fail, "/s1")
	step("")

	f.in.set("/s1", trojan)
	step(CodeManifestInvalid)
	f.in.set("/s1", vienna)
	step("")

	f.in.set("/s1", berlin)
	srv.FailNext("AddOutbound", status.Error(codes.Internal, "boom"))
	step(CodeApplyFailed)
	step("")

	srv.FailNext("ListOutbounds", status.Error(codes.Internal, "boom"))
	step(CodeOrphanRemoveFailed)
	step("")

	s.Sources[0].Balancer = "gone"
	f.s.Config = cfg(outboundOnly, s)
	step(CodeBalancerNotFound)
	if e, _ := sourceError(f, "vless"); e.Message != `Xray balancer "gone" not found.` {
		t.Errorf("message %q", e.Message)
	}
}

// A wireguard input with group or other access is wireguard_input_refused.
func TestSourceErrorInputRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wg")
	writeCandidates(t, path, wgFixtureLines(t), 1, 2)
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	f := newSourcesFixture(t, wgSub(path), nil)
	f.run(t)
	if e, ok := sourceError(f, "wg"); !ok || e.Code != "wireguard_input_refused" {
		t.Fatalf("error %+v, %v", e, ok)
	}
}
