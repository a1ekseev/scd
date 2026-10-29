package apply_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/a1ekseev/scd/internal/scd/apply"
	"github.com/a1ekseev/scd/internal/scd/resources"
	"github.com/a1ekseev/scd/internal/scd/topology"
	"github.com/a1ekseev/scd/internal/scd/xrayapi"
	"github.com/a1ekseev/scd/internal/scd/xrayapi/xrayfake"
)

// slotTunnel is the tunnel of base tag tag on port with the observatory
// prefix "x-".
func slotTunnel(tag string, port int) topology.Tunnel {
	return topology.Tunnel{
		BaseTunnelID: tag, OutboundTagInitial: "x-" + tag, OutboundTagCurrent: "x-" + tag,
		InboundTag: "in-" + tag, RouteTag: "route-" + tag, Listen: "127.0.0.1", Port: port,
		OutboundInitial:       topology.PreparedOutbound{Tag: "x-" + tag, JSON: entry("x-"+tag, "198.51.100.1")},
		OutboundWithoutPrefix: topology.PreparedOutbound{Tag: tag, JSON: entry(tag, "198.51.100.1")},
	}
}

// install adds the tunnel's outbound, inbound and rule to the fake.
func install(t *testing.T, c xrayapi.Client, tn topology.Tunnel) {
	t.Helper()
	ctx := context.Background()
	in, err := resources.Inbound(tn)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.AddOutbound(ctx, mustProto(t, entry(tn.OutboundTagCurrent, "198.51.100.1"))); err != nil {
		t.Fatal(err)
	}
	if err := c.AddInbound(ctx, in); err != nil {
		t.Fatal(err)
	}
	if err := c.AddRule(ctx, resources.Rule(tn, tn.OutboundTagCurrent)); err != nil {
		t.Fatal(err)
	}
}

var replaceSequence = []string{
	"RemoveRule:route-s-old", "RemoveOutbound:s-old", "RemoveOutbound:x-s-old", "RemoveInbound:in-s-old",
	"AddOutbound:x-s-new", "AddInbound:in-s-new", "AddRule:route-s-new->x-s-new",
}

// Remove before add, on the same port; a repaired tunnel is removed by
// its current (unprefixed) tag first, then by the other one. A failure at any step adds nothing after
// it, a failed RemoveOutbound adds nothing new at all, and the source never
// has two outbounds at once.
func TestReplaceTunnel(t *testing.T) {
	old := slotTunnel("s-old", 20100)
	old.OutboundTagCurrent = "s-old" // repaired
	next := slotTunnel("s-new", 20100)
	for _, tc := range []struct {
		fail string
		want []string
	}{
		{"", replaceSequence},
		// The rule removal is best effort.
		{"RemoveRule", replaceSequence},
		{"RemoveOutbound", []string{"RemoveRule:route-s-old", "RemoveOutbound:s-old", "AddRule:route-s-old->s-old"}},
		{"RemoveInbound", replaceSequence[:4]},
		{"AddOutbound", replaceSequence[:5]},
		{"AddInbound", replaceSequence[:6]},
		{"AddRule", replaceSequence},
	} {
		t.Run(tc.fail, func(t *testing.T) {
			srv, c := xrayfake.Start(t)
			install(t, c, old)
			n := len(srv.Calls())
			if tc.fail != "" {
				srv.FailNext(tc.fail, status.Error(codes.Unknown, "boom"))
			}
			ruleErr, err := apply.ReplaceTunnel(context.Background(), c, old, next)
			if (ruleErr != nil) != (tc.fail == "RemoveRule") {
				t.Fatalf("ruleErr %v", ruleErr)
			}
			if got := srv.Calls()[n:]; !slices.Equal(got, tc.want) {
				t.Fatalf("calls %q, want %q", got, tc.want)
			}
			if wantErr := tc.fail != "" && tc.fail != "RemoveRule"; (err != nil) != wantErr || err != nil && !errors.Is(err, apply.ErrReplaceFailed) {
				t.Fatalf("err %v", err)
			}
			if p := peak([]string{"s-old"}, srv.Calls()[n:], ""); p > 1 {
				t.Fatalf("peak %d outbounds", p)
			}
			if tc.fail == "" {
				if in := srv.Inbound("in-s-new"); in == nil || !slices.Equal(srv.OutboundTags(), []string{"x-s-new"}) {
					t.Fatalf("outbounds %v, inbound %v", srv.OutboundTags(), in)
				}
			}
		})
	}
}

// RemoveTunnel: rule, outbound, inbound; a failed RemoveOutbound restores
// the rule and stops.
func TestRemoveTunnel(t *testing.T) {
	old := slotTunnel("s-old", 20100)
	for _, tc := range []struct {
		fail string
		want []string
	}{
		{"", []string{"RemoveRule:route-s-old", "RemoveOutbound:x-s-old", "RemoveOutbound:s-old", "RemoveInbound:in-s-old"}},
		{"RemoveOutbound", []string{"RemoveRule:route-s-old", "RemoveOutbound:x-s-old", "AddRule:route-s-old->x-s-old"}},
		{"RemoveInbound", []string{"RemoveRule:route-s-old", "RemoveOutbound:x-s-old", "RemoveOutbound:s-old", "RemoveInbound:in-s-old"}},
	} {
		t.Run(tc.fail, func(t *testing.T) {
			srv, c := xrayfake.Start(t)
			install(t, c, old)
			n := len(srv.Calls())
			if tc.fail != "" {
				srv.FailNext(tc.fail, status.Error(codes.Unknown, "boom"))
			}
			_, err := apply.RemoveTunnel(context.Background(), c, old)
			if got := srv.Calls()[n:]; !slices.Equal(got, tc.want) || (err != nil) != (tc.fail != "") {
				t.Fatalf("calls %q, want %q, err %v", got, tc.want, err)
			}
		})
	}
}

func TestSlotReports(t *testing.T) {
	old, next := slotTunnel("s-old", 1), slotTunnel("s-new", 1)
	rs := apply.SlotReports(apply.Order, meta(), []topology.Tunnel{old}, []topology.Tunnel{next}, "x-s-new", errors.New("boom"))
	if len(rs) != 3 || rs[0].Removed != 1 || rs[0].Added != 1 || rs[0].Failed != 1 || rs[1].Failed != 0 ||
		!slices.Equal(rs[2].DeletedIDs, []string{"route-s-old"}) || !slices.Equal(rs[1].AppliedIDs, []string{"in-s-new"}) {
		t.Fatalf("reports %+v", rs)
	}
}

// AddTunnel adds outbound, inbound, rule; a failure removes what was
// added, in reverse, and leaves nothing of the tunnel.
func TestAddTunnel(t *testing.T) {
	next := slotTunnel("s-new", 20100)
	add := []string{"AddOutbound:x-s-new", "AddInbound:in-s-new", "AddRule:route-s-new->x-s-new"}
	for _, tc := range []struct {
		fail string
		want []string
	}{
		{"", add},
		{"AddOutbound", append(slices.Clone(add[:1]), "RemoveOutbound:x-s-new")},
		{"AddInbound", append(slices.Clone(add[:2]), "RemoveOutbound:x-s-new")},
		{"AddRule", append(slices.Clone(add), "RemoveRule:route-s-new", "RemoveInbound:in-s-new", "RemoveOutbound:x-s-new")},
	} {
		t.Run(tc.fail, func(t *testing.T) {
			srv, c := xrayfake.Start(t)
			if tc.fail != "" {
				srv.FailNext(tc.fail, status.Error(codes.Unknown, "boom"))
			}
			err := apply.AddTunnel(context.Background(), c, next)
			if got := srv.Calls(); !slices.Equal(got, tc.want) {
				t.Fatalf("calls %q, want %q", got, tc.want)
			}
			if (err != nil) != (tc.fail != "") || err != nil && !errors.Is(err, apply.ErrReplaceFailed) {
				t.Fatalf("err %v", err)
			}
			if tc.fail != "" && (len(srv.OutboundTags()) != 0 || len(srv.InboundTags()) != 0 || len(srv.Rules()) != 0) {
				t.Fatalf("left %v %v %v", srv.OutboundTags(), srv.InboundTags(), srv.Rules())
			}
		})
	}
}
