package apply_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/a1ekseev/scd/internal/scd/apply"
	"github.com/a1ekseev/scd/internal/scd/topology"
	"github.com/a1ekseev/scd/internal/scd/xrayapi"
	"github.com/a1ekseev/scd/internal/scd/xrayapi/xrayfake"
)

func TestRoutingRemovesThenAddsOneRulePerCall(t *testing.T) {
	srv, c := xrayfake.Start(t, xrayfake.WithStaticRules(
		xrayapi.Rule{Tag: "direct", RuleTag: "keep"},
		xrayapi.Rule{Tag: "old", RuleTag: "route-old"},
		xrayapi.Rule{Tag: "x-observe-a", RuleTag: "route-a"},
	))
	tunnels := []topology.Tunnel{tunnel("a", 20000), tunnel("b", 20001)}

	r, err := apply.Routing(context.Background(), c, []string{"keep"}, tunnels, meta())
	if err != nil {
		t.Fatal(err)
	}
	wantCalls := []string{
		"ListRule",
		"RemoveRule:route-old", "RemoveRule:route-a",
		"AddRule:route-a->x-observe-a", "AddRule:route-b->x-observe-b",
	}
	if got := srv.Calls(); !slices.Equal(got, wantCalls) {
		t.Errorf("calls = %v, want %v", got, wantCalls)
	}
	wantItems := []string{"route-old=removed", "route-a=removed", "route-a=replaced", "route-b=added"}
	if got := statuses(r); !slices.Equal(got, wantItems) {
		t.Errorf("items = %v, want %v", got, wantItems)
	}
	assertCounts(t, r, 1, 1, 2, 0)
	if !slices.Equal(r.DeletedIDs, []string{"route-old", "route-a"}) || !slices.Equal(r.AppliedIDs, []string{"route-a", "route-b"}) {
		t.Errorf("deletedIds = %v, appliedIds = %v", r.DeletedIDs, r.AppliedIDs)
	}
	wantRules := []xrayapi.Rule{
		{Tag: "direct", RuleTag: "keep"},
		{Tag: "x-observe-a", RuleTag: "route-a"},
		{Tag: "x-observe-b", RuleTag: "route-b"},
	}
	if got := srv.Rules(); !slices.Equal(got, wantRules) {
		t.Errorf("rules = %v, want %v", got, wantRules)
	}
	if r.Kind != apply.KindRouting || r.Skipped == nil || len(r.Skipped) != 0 {
		t.Errorf("kind = %q, skipped = %#v", r.Kind, r.Skipped)
	}
}

func TestRoutingUsesCurrentOutboundTag(t *testing.T) {
	srv, c := xrayfake.Start(t)
	tn := tunnel("a", 20000)
	tn.OutboundTagCurrent = "a" // repaired tunnel

	if _, err := apply.Routing(context.Background(), c, nil, []topology.Tunnel{tn}, meta()); err != nil {
		t.Fatal(err)
	}
	if got := srv.Rules(); !slices.Equal(got, []xrayapi.Rule{{Tag: "a", RuleTag: "route-a"}}) {
		t.Errorf("rules = %v", got)
	}
}

func TestRoutingFixedConflictNoRPC(t *testing.T) {
	srv, c := xrayfake.Start(t)

	r, err := apply.Routing(context.Background(), c, []string{"route-a"}, []topology.Tunnel{tunnel("a", 20000)}, meta())
	if err != nil {
		t.Fatal(err)
	}
	if calls := srv.Calls(); len(calls) != 0 {
		t.Errorf("calls = %v, want none", calls)
	}
	want := []apply.Item{{
		ID:      "route-a",
		Status:  "failed",
		Message: `Managed routing rule "route-a" conflicts with fixedRouting on target "127.0.0.1:8080".`,
	}}
	if !slices.Equal(r.Items, want) || r.Failed != 1 {
		t.Errorf("items = %+v, want %+v", r.Items, want)
	}
}

// O5: a static rule without ruleTag that is not fixed is removed by its
// empty ruleTag, as in TS; Xray refuses it and the routing apply fails.
func TestRoutingRemovesEmptyRuleTagLikeTS(t *testing.T) {
	srv, c := xrayfake.Start(t, xrayfake.WithStaticRules(xrayapi.Rule{Tag: "direct", RuleTag: ""}))
	tunnels := []topology.Tunnel{tunnel("a", 20000), tunnel("b", 20001)}

	r, err := apply.Routing(context.Background(), c, nil, tunnels, meta())
	if err != nil {
		t.Fatal(err)
	}
	if got, want := srv.Calls(), []string{"ListRule", "RemoveRule:"}; !slices.Equal(got, want) {
		t.Errorf("calls = %v, want %v", got, want)
	}
	assertCounts(t, r, 0, 0, 0, 1)
	want := []apply.Item{{ID: "route-a", Status: "failed", Message: "app/router: empty tag name!"}}
	if !slices.Equal(r.Items, want) {
		t.Errorf("items = %+v, want %+v", r.Items, want)
	}
	if got := srv.Rules(); !slices.Equal(got, []xrayapi.Rule{{Tag: "direct"}}) {
		t.Errorf("rules = %v", got)
	}
}

func TestRoutingAddFailureNoRollback(t *testing.T) {
	srv, c := xrayfake.Start(t, xrayfake.WithStaticRules(xrayapi.Rule{Tag: "old", RuleTag: "route-old"}))
	srv.FailNext("AddRule", status.Error(codes.Unknown, "boom"))

	r, err := apply.Routing(context.Background(), c, nil, []topology.Tunnel{tunnel("a", 20000)}, meta())
	if err != nil {
		t.Fatal(err)
	}
	if got, want := statuses(r), []string{"route-old=removed", "route-a=failed"}; !slices.Equal(got, want) {
		t.Errorf("items = %v, want %v", got, want)
	}
	assertCounts(t, r, 0, 0, 1, 1)
	if got, want := srv.Calls(), []string{"ListRule", "RemoveRule:route-old", "AddRule:route-a->x-observe-a"}; !slices.Equal(got, want) {
		t.Errorf("calls = %v, want %v (no rollback)", got, want)
	}
	if got := srv.Rules(); len(got) != 0 {
		t.Errorf("rules = %v, want none", got)
	}
}

func TestRoutingFailedIDFallsBackToKind(t *testing.T) {
	srv, c := xrayfake.Start(t, xrayfake.WithStaticRules(xrayapi.Rule{Tag: "old", RuleTag: "route-old"}))
	srv.FailNext("RemoveRule", status.Error(codes.Unknown, "boom"))

	r, err := apply.Routing(context.Background(), c, nil, nil, meta())
	if err != nil {
		t.Fatal(err)
	}
	if got := statuses(r); !slices.Equal(got, []string{"routing=failed"}) {
		t.Errorf("items = %v", got)
	}
}

func TestRoutingListErrorPropagates(t *testing.T) {
	srv, c := xrayfake.Start(t)
	srv.FailNext("ListRule", status.Error(codes.Unknown, "boom"))

	_, err := apply.Routing(context.Background(), c, nil, []topology.Tunnel{tunnel("a", 20000)}, meta())
	var xe *xrayapi.Error
	if !errors.As(err, &xe) || err.Error() != "boom" {
		t.Fatalf("err = %v", err)
	}
}
