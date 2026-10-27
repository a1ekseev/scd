package apply_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/xtls/xray-core/core"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/a1ekseev/scd/internal/scd/apply"
	"github.com/a1ekseev/scd/internal/scd/resources"
	"github.com/a1ekseev/scd/internal/scd/topology"
	"github.com/a1ekseev/scd/internal/scd/xrayapi"
	"github.com/a1ekseev/scd/internal/scd/xrayapi/xrayfake"
)

// tunnel is a minimal tunnel of entry tag id on port.
func tunnel(id string, port int) topology.Tunnel {
	return topology.Tunnel{
		BaseTunnelID:       id,
		BaseOutboundTag:    id,
		OutboundTagInitial: "x-observe-" + id,
		OutboundTagCurrent: "x-observe-" + id,
		InboundTag:         "in-" + id,
		RouteTag:           "route-" + id,
		Listen:             "127.0.0.1",
		Port:               port,
	}
}

func mustInbound(t *testing.T, tn topology.Tunnel) *core.InboundHandlerConfig {
	t.Helper()
	h, err := resources.Inbound(tn)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// sortedHead returns the first n items sorted (List order is shuffled) and
// the rest as is.
func sortedHead(items []string, n int) []string {
	out := slices.Clone(items)
	slices.Sort(out[:n])
	return out
}

func TestInboundsReplacesNonFixed(t *testing.T) {
	srv, c := xrayfake.Start(t, xrayfake.WithStaticInbounds("api", "old", "in-a"))
	tunnels := []topology.Tunnel{tunnel("a", 20000), tunnel("b", 20001)}

	r, err := apply.Inbounds(context.Background(), c, []string{"api"}, tunnels, meta())
	if err != nil {
		t.Fatal(err)
	}
	if got, want := sortedHead(statuses(r), 2), []string{"in-a=removed", "old=removed", "in-a=replaced", "in-b=added"}; !slices.Equal(got, want) {
		t.Errorf("items = %v, want %v", got, want)
	}
	assertCounts(t, r, 1, 1, 2, 0)
	if got := sortedHead(r.DeletedIDs, 2); !slices.Equal(got, []string{"in-a", "old"}) {
		t.Errorf("deletedIds = %v", r.DeletedIDs)
	}
	if !slices.Equal(r.AppliedIDs, []string{"in-a", "in-b"}) {
		t.Errorf("appliedIds = %v", r.AppliedIDs)
	}
	want := []string{"ListInbounds", "RemoveInbound:in-a", "RemoveInbound:old", "AddInbound:in-a", "AddInbound:in-b"}
	if got := srv.Calls(); len(got) != len(want) || !slices.Equal(sortedHead(got[1:], 2), want[1:]) || got[0] != want[0] {
		t.Errorf("calls = %v, want %v (removals in any order)", got, want)
	}
	if got, want := srv.InboundTags(), []string{"api", "in-a", "in-b"}; !slices.Equal(got, want) {
		t.Errorf("tags = %v, want %v", got, want)
	}
	if !proto.Equal(srv.Inbound("in-a"), mustInbound(t, tunnels[0])) {
		t.Error("inbound in-a is not the tunnel's SOCKS inbound")
	}
	if r.Kind != apply.KindInbound || r.TargetAddress != target || r.AppliedAt != "2026-01-01T00:00:00.000Z" {
		t.Errorf("header = %+v", r)
	}
	if r.Skipped == nil || len(r.Skipped) != 0 {
		t.Errorf("skipped = %#v, want []", r.Skipped)
	}
}

func TestInboundsEmptyTopologyRemovesAllNonFixed(t *testing.T) {
	srv, c := xrayfake.Start(t, xrayfake.WithStaticInbounds("api", "in-a"))

	r, err := apply.Inbounds(context.Background(), c, []string{"api"}, nil, meta())
	if err != nil {
		t.Fatal(err)
	}
	if got := statuses(r); !slices.Equal(got, []string{"in-a=removed"}) {
		t.Errorf("items = %v", got)
	}
	if got := srv.InboundTags(); !slices.Equal(got, []string{"api"}) {
		t.Errorf("tags = %v", got)
	}
}

func TestInboundsFixedConflictNoRPC(t *testing.T) {
	srv, c := xrayfake.Start(t)

	tunnels := []topology.Tunnel{tunnel("a", 20000), tunnel("b", 20001)}
	r, err := apply.Inbounds(context.Background(), c, []string{"in-b"}, tunnels, meta())
	if err != nil {
		t.Fatal(err)
	}
	if calls := srv.Calls(); len(calls) != 0 {
		t.Errorf("calls = %v, want none", calls)
	}
	assertCounts(t, r, 0, 0, 0, 1)
	want := []apply.Item{{
		ID:      "in-b",
		Status:  "failed",
		Message: `Managed inbound tag "in-b" conflicts with fixedInbounds on target "127.0.0.1:8080".`,
	}}
	if !slices.Equal(r.Items, want) {
		t.Errorf("items = %+v, want %+v", r.Items, want)
	}
}

// failOnAddInbound makes the AddInbound of tag fail through FailNext.
type failOnAddInbound struct {
	xrayapi.InboundAPI
	srv *xrayfake.Server
	tag string
}

func (f *failOnAddInbound) AddInbound(ctx context.Context, h *core.InboundHandlerConfig) error {
	if h.GetTag() == f.tag {
		f.srv.FailNext("AddInbound", status.Error(codes.Unknown, "boom"))
	}
	return f.InboundAPI.AddInbound(ctx, h)
}

func TestInboundsAddFailureNoRollback(t *testing.T) {
	srv, c := xrayfake.Start(t, xrayfake.WithStaticInbounds("old"))
	tunnels := []topology.Tunnel{tunnel("a", 20000), tunnel("b", 20001)}

	api := &failOnAddInbound{InboundAPI: c, srv: srv, tag: "in-b"}
	r, err := apply.Inbounds(context.Background(), api, nil, tunnels, meta())
	if err != nil {
		t.Fatal(err)
	}
	assertCounts(t, r, 1, 0, 1, 1)
	// The failed item carries the first tunnel's inbound tag, not the one
	// being added (apply-inbounds.ts:99-106).
	want := []string{"old=removed", "in-a=added", "in-a=failed"}
	if got := statuses(r); !slices.Equal(got, want) {
		t.Errorf("items = %v, want %v", got, want)
	}
	if last := r.Items[len(r.Items)-1]; last.Message != "boom" {
		t.Errorf("message = %q", last.Message)
	}
	if got, want := srv.Calls(), []string{"ListInbounds", "RemoveInbound:old", "AddInbound:in-a", "AddInbound:in-b"}; !slices.Equal(got, want) {
		t.Errorf("calls = %v, want %v (no rollback)", got, want)
	}
	if got := srv.InboundTags(); !slices.Equal(got, []string{"in-a"}) {
		t.Errorf("tags = %v", got)
	}
}

func TestInboundsFailedIDFallsBackToKind(t *testing.T) {
	srv, c := xrayfake.Start(t, xrayfake.WithStaticInbounds("old"))
	srv.FailNext("RemoveInbound", status.Error(codes.Unknown, "boom"))

	r, err := apply.Inbounds(context.Background(), c, nil, nil, meta())
	if err != nil {
		t.Fatal(err)
	}
	if got := statuses(r); !slices.Equal(got, []string{"inbound=failed"}) {
		t.Errorf("items = %v", got)
	}
}

func TestInboundsListErrorPropagates(t *testing.T) {
	srv, c := xrayfake.Start(t)
	srv.FailNext("ListInbounds", status.Error(codes.Unknown, "boom"))

	_, err := apply.Inbounds(context.Background(), c, nil, []topology.Tunnel{tunnel("a", 20000)}, meta())
	var xe *xrayapi.Error
	if !errors.As(err, &xe) || err.Error() != "boom" {
		t.Fatalf("err = %v", err)
	}
	if got := srv.Calls(); !slices.Equal(got, []string{"ListInbounds"}) {
		t.Errorf("calls = %v", got)
	}
}
