package apply_test

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/a1ekseev/scd/internal/scd/apply"
	"github.com/a1ekseev/scd/internal/scd/config"
	"github.com/a1ekseev/scd/internal/scd/manifest"
	"github.com/a1ekseev/scd/internal/scd/normalize"
	"github.com/a1ekseev/scd/internal/scd/xrayapi"
	"github.com/a1ekseev/scd/internal/scd/xrayapi/xrayfake"
)

func TestNamespaceOwns(t *testing.T) {
	for _, c := range []struct {
		obs  string
		kind apply.Kind
		tag  string
		want bool
	}{
		{"x-observe-", apply.KindOutbound, "x-observe-vless-at-1", true},
		{"x-observe-", apply.KindOutbound, "vless-at-1", true},
		{"x-observe-", apply.KindOutbound, "x-observe-wg-at-1", false},
		{"x-observe-", apply.KindOutbound, "x-observe-vlessx-1", false},
		{"x-observe-", apply.KindOutbound, "direct", false},
		{"", apply.KindOutbound, "vless-at-1", true},
		{"", apply.KindOutbound, "x-observe-vless-at-1", false},
		{"", apply.KindInbound, "in-vless-at-1", true},
		{"", apply.KindInbound, "vless-at-1", false},
		{"", apply.KindInbound, "in-wg-at-1", false},
		{"", apply.KindRouting, "route-vless-at-1", true},
		{"", apply.KindRouting, "in-vless-at-1", false},
		{"", apply.KindRouting, "", false},
		{"", "balancer", "vless-at-1", false},
	} {
		n := apply.Namespace{ObsPrefix: c.obs, SourceID: "vless"}
		if got := n.Owns(c.kind, c.tag); got != c.want {
			t.Errorf("%q %s %q: got %v", c.obs, c.kind, c.tag, got)
		}
	}
}

// orphanTarget has fixed resources and the observatory prefix.
var orphanTarget = config.SubscriptionTargetConfig{
	Address: target, ObservatorySubjectSelectorPrefix: "x-observe-",
	FixedOutbounds: []string{"direct"}, FixedInbounds: []string{"api"}, FixedRouting: []string{"keep"},
}

func orphanFake(t *testing.T) (*xrayfake.Server, xrayapi.Client) {
	t.Helper()
	return xrayfake.Start(t,
		xrayfake.WithStaticOutbounds("direct", "x-observe-a-1", "a-2", "x-observe-old-1", "vless-de-1", "x-observe-b-1"),
		xrayfake.WithStaticInbounds("api", "in-a-1", "in-old-1", "in-b-1"),
		xrayfake.WithStaticRules(
			xrayapi.Rule{Tag: "direct", RuleTag: "keep"},
			xrayapi.Rule{Tag: "direct"}, // a static rule without ruleTag
			xrayapi.Rule{Tag: "x-observe-a-1", RuleTag: "route-a-1"},
			xrayapi.Rule{Tag: "x-observe-old-1", RuleTag: "route-old-1"},
			xrayapi.Rule{Tag: "x-observe-b-1", RuleTag: "route-b-1"},
		),
	)
}

func TestRemoveOrphans(t *testing.T) {
	srv, c := orphanFake(t)
	ns := []apply.Namespace{{ObsPrefix: "x-observe-", SourceID: "a"}, {ObsPrefix: "x-observe-", SourceID: "b"}}
	removed, err := apply.RemoveOrphans(context.Background(), c, orphanTarget, apply.Order, ns)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, o := range removed {
		got = append(got, string(o.Kind)+":"+o.Tag)
	}
	slices.Sort(got)
	if want := []string{"inbound:in-old-1", "outbound:vless-de-1", "outbound:x-observe-old-1", "routing:route-old-1"}; !slices.Equal(got, want) {
		t.Fatalf("removed %v", got)
	}
	if got, want := srv.OutboundTags(), []string{"a-2", "direct", "x-observe-a-1", "x-observe-b-1"}; !slices.Equal(got, want) {
		t.Errorf("outbounds %v", got)
	}
	if got, want := srv.InboundTags(), []string{"api", "in-a-1", "in-b-1"}; !slices.Equal(got, want) {
		t.Errorf("inbounds %v", got)
	}
	if n := len(srv.Rules()); n != 4 {
		t.Errorf("rules %v", srv.Rules())
	}
	// Kinds in the order rule → inbound → outbound, each listed first;
	// RemoveRule("") is never sent.
	var kinds []string
	for _, call := range srv.Calls() {
		if call == "RemoveRule:" {
			t.Errorf("RemoveRule with an empty tag")
		}
		name, _, _ := strings.Cut(call, ":")
		if len(kinds) == 0 || kinds[len(kinds)-1] != name {
			kinds = append(kinds, name)
		}
	}
	if want := []string{"ListRule", "RemoveRule", "ListInbounds", "RemoveInbound", "ListOutbounds", "RemoveOutbound"}; !slices.Equal(kinds, want) {
		t.Errorf("calls %v", srv.Calls())
	}
}

func TestRemoveOrphansOnlyEnabledKinds(t *testing.T) {
	srv, c := orphanFake(t)
	ns := []apply.Namespace{{ObsPrefix: "x-observe-", SourceID: "a"}}
	if _, err := apply.RemoveOrphans(context.Background(), c, orphanTarget, []apply.Kind{apply.KindOutbound}, ns); err != nil {
		t.Fatal(err)
	}
	for _, call := range srv.Calls() {
		if !strings.Contains(call, "Outbound") {
			t.Errorf("RPC of a disabled kind: %s", call)
		}
	}
}

func TestRemoveOrphansStopsOnError(t *testing.T) {
	for _, method := range []string{"ListRule", "RemoveRule", "ListInbounds", "RemoveInbound", "ListOutbounds", "RemoveOutbound"} {
		t.Run(method, func(t *testing.T) {
			srv, c := orphanFake(t)
			srv.FailNext(method, status.Error(codes.Unknown, "boom"))
			_, err := apply.RemoveOrphans(context.Background(), c, orphanTarget, apply.Order, nil)
			if err == nil || !strings.Contains(err.Error(), "boom") {
				t.Fatalf("err %v", err)
			}
			calls := srv.Calls()
			if last := calls[len(calls)-1]; !strings.HasPrefix(last, method) {
				t.Errorf("calls went on after the failure: %v", calls)
			}
		})
	}
}

// Applying one source never sends an RPC for another source's resources,
// and each kind removes the source's old resources before it adds the new
// ones.
func TestApplyTouchesOnlyItsSource(t *testing.T) {
	srv, c := orphanFake(t)
	m := manifest.Build(vienna+"\n"+berlin, "a", normalize.Policy{TagPrefix: "a"}, time.Unix(0, 0))
	tc := orphanTarget
	withSocks(&tc)
	a := config.SourceConfig{ID: "a", PortRange: &config.PortRange{Start: 20000, End: 20010}}
	for _, kind := range apply.Order {
		p, err := apply.PlanForSource(kind, m, a, tc)
		if err != nil {
			t.Fatal(err)
		}
		if r := apply.Apply(context.Background(), c, p, tc, meta()); r.Failed != 0 {
			t.Fatalf("%s: %+v", kind, r.Items)
		}
	}
	for _, call := range srv.Calls() {
		_, tag, _ := strings.Cut(call, ":")
		inA := strings.HasPrefix(tag, "x-observe-a-") || strings.HasPrefix(tag, "a-") ||
			strings.HasPrefix(tag, "in-a-") || strings.HasPrefix(tag, "route-a-")
		if strings.HasPrefix(call, "Remove") && !inA {
			t.Errorf("removed a resource outside source a: %s", call)
		}
		if strings.Contains(tag, "b-1") || strings.Contains(tag, "old-1") || tag == "direct" || tag == "api" || tag == "keep" {
			t.Errorf("RPC on a foreign resource: %s", call)
		}
	}
	calls := srv.Calls()
	slices.Sort(calls[1:3]) // the fake lists outbounds in random order
	at, de := "a-at-3ed0e2a1-tcp-tls-cf29de98", "a-de-51bd674f-tcp-tls-675db9e5"
	want := []string{
		"ListOutbounds", "RemoveOutbound:a-2", "RemoveOutbound:x-observe-a-1",
		"AddOutbound:x-observe-" + at, "AddOutbound:x-observe-" + de,
		"ListInbounds", "RemoveInbound:in-a-1", "AddInbound:in-" + at, "AddInbound:in-" + de,
		"ListRule", "RemoveRule:route-a-1",
		"AddRule:route-" + at + "->x-observe-" + at, "AddRule:route-" + de + "->x-observe-" + de,
	}
	if !slices.Equal(calls, want) {
		t.Errorf("calls = %q\nwant    %q", calls, want)
	}
}
