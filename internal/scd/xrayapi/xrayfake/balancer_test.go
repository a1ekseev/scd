package xrayfake_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/xtls/xray-core/app/observatory"
	obscmd "github.com/xtls/xray-core/app/observatory/command"
	routercmd "github.com/xtls/xray-core/app/router/command"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/a1ekseev/scd/internal/scd/xrayapi/xrayfake"
)

func obs(tag string, alive bool, delay int64) *observatory.OutboundStatus {
	return &observatory.OutboundStatus{OutboundTag: tag, Alive: alive, Delay: delay}
}

func TestFakeStrategies(t *testing.T) {
	// Four candidates, one dead (10 §5.2), plus outbounds outside the
	// selector and a stale observation of a removed tag.
	cands := []string{"x-observe-dead", "x-observe-fast", "x-observe-mid", "x-observe-slow"}
	s, c := xrayfake.Start(t,
		xrayfake.WithStaticOutbounds(append([]string{"direct", "tunnel-repaired", "other-x"}, cands...)...),
		xrayfake.WithBalancer("x-balancer", []string{"x-observe-"}, "leastPing"),
		xrayfake.WithBalancer("bal-load", []string{"x-observe-"}, "leastLoad"),
		xrayfake.WithBalancer("bal-random", []string{"x-observe-"}, "random"),
		xrayfake.WithBalancer("bal-rr", []string{"x-observe-"}, "roundRobin"),
		xrayfake.WithBalancer("bal-multi", []string{"other-", "x-observe-m"}, "roundRobin"),
	)
	s.SetObservation(
		obs("x-observe-slow", true, 300),
		obs("x-observe-mid", true, 120),
		obs("x-observe-dead", false, 99999999),
		obs("x-observe-fast", true, 21),
		obs("x-observe-gone", true, 1), // removed outbound, still reported
		obs("tunnel-repaired", true, 2),
	)
	raw := routercmd.NewRoutingServiceClient(rawConn(t, s))
	principle := func(t *testing.T, tag string) []string {
		t.Helper()
		resp, err := raw.GetBalancerInfo(t.Context(), &routercmd.GetBalancerInfoRequest{Tag: tag})
		if err != nil {
			t.Fatalf("GetBalancerInfo(%s): %v", tag, err)
		}
		if resp.GetBalancer().GetOverride() == nil {
			t.Fatalf("override must always be set: %v", resp)
		}
		if resp.GetBalancer().GetPrincipleTarget() == nil {
			t.Fatalf("principle_target nil: %v", resp)
		}
		return resp.GetBalancer().GetPrincipleTarget().GetTag()
	}

	tests := []struct {
		balancer string
		want     []string
	}{
		{"x-balancer", []string{"x-observe-fast"}},
		{"bal-load", []string{"x-observe-fast"}},
		{"bal-random", cands},
		{"bal-rr", cands},
		{"bal-multi", []string{"other-x", "x-observe-mid"}},
	}
	for _, tt := range tests {
		t.Run(tt.balancer, func(t *testing.T) {
			if got := principle(t, tt.balancer); !slices.Equal(got, tt.want) {
				t.Fatalf("principle = %q, want %q", got, tt.want)
			}
		})
	}

	t.Run("leastLoad tie-break by tag", func(t *testing.T) {
		s.SetObservation(obs("x-observe-slow", true, 50), obs("x-observe-mid", true, 50), obs("x-observe-fast", true, 90))
		if got := principle(t, "bal-load"); !slices.Equal(got, []string{"x-observe-mid"}) {
			t.Fatalf("leastLoad = %q", got)
		}
		// leastPing keeps the first of equal delays in observatory order.
		if got := principle(t, "x-balancer"); !slices.Equal(got, []string{"x-observe-slow"}) {
			t.Fatalf("leastPing = %q", got)
		}
	})

	t.Run("candidates follow outbounds", func(t *testing.T) {
		s.SetObservation(obs("x-observe-fast", true, 21), obs("x-observe-mid", true, 120))
		if err := c.RemoveOutbound(t.Context(), "x-observe-fast"); err != nil {
			t.Fatalf("RemoveOutbound: %v", err)
		}
		if got := principle(t, "x-balancer"); !slices.Equal(got, []string{"x-observe-mid"}) {
			t.Fatalf("leastPing after remove = %q", got)
		}
		if got := principle(t, "bal-random"); slices.Contains(got, "x-observe-fast") {
			t.Fatalf("random after remove = %q", got)
		}
	})

	t.Run("no alive candidate", func(t *testing.T) {
		s.SetObservation(obs("x-observe-dead", false, 99999999))
		if got := principle(t, "x-balancer"); !slices.Equal(got, []string{""}) {
			t.Fatalf("leastPing = %q, want [\"\"]", got)
		}
		if got := principle(t, "bal-load"); len(got) != 0 {
			t.Fatalf("leastLoad = %q, want []", got)
		}
		if got := principle(t, "bal-rr"); !slices.Contains(got, "x-observe-dead") {
			t.Fatalf("roundRobin = %q, want dead included", got)
		}
	})
}

func TestFakeBalancerUnknownTag(t *testing.T) {
	s, _ := xrayfake.Start(t, xrayfake.WithBalancer("b", []string{"x-"}, "random"))
	raw := routercmd.NewRoutingServiceClient(rawConn(t, s))
	_, err := raw.GetBalancerInfo(t.Context(), &routercmd.GetBalancerInfoRequest{Tag: "nope"})
	if st := status.Convert(err); st.Code() != codes.Unknown || st.Message() != "app/router: cannot find tag" {
		t.Fatalf("GetBalancerInfo(nope) = %v", err)
	}
	_, err = raw.OverrideBalancerTarget(t.Context(), &routercmd.OverrideBalancerTargetRequest{BalancerTag: "nope", Target: "x"})
	if st := status.Convert(err); st.Code() != codes.Unknown || st.Message() != "app/router: cannot find tag" {
		t.Fatalf("OverrideBalancerTarget(nope) = %v", err)
	}
}

func TestFakeOverrideNotValidated(t *testing.T) {
	s, _ := xrayfake.Start(t, xrayfake.WithBalancer("b", []string{"x-"}, "random"))
	raw := routercmd.NewRoutingServiceClient(rawConn(t, s))
	if _, err := raw.OverrideBalancerTarget(t.Context(), &routercmd.OverrideBalancerTargetRequest{
		BalancerTag: "b", Target: "nonexistent-tag",
	}); err != nil {
		t.Fatalf("OverrideBalancerTarget: %v", err)
	}
	resp, err := raw.GetBalancerInfo(t.Context(), &routercmd.GetBalancerInfoRequest{Tag: "b"})
	if err != nil {
		t.Fatalf("GetBalancerInfo: %v", err)
	}
	if got := resp.GetBalancer().GetOverride().GetTarget(); got != "nonexistent-tag" {
		t.Fatalf("override = %q", got)
	}
	want := []string{"OverrideBalancerTarget:b->nonexistent-tag", "GetBalancerInfo:b"}
	if got := s.Calls(); !slices.Equal(got, want) {
		t.Fatalf("Calls = %v, want %v", got, want)
	}
}

func TestFakeWithBalancerRejectsUnknownStrategy(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("WithBalancer with an unknown strategy did not panic")
		}
	}()
	xrayfake.Start(t, xrayfake.WithBalancer("b", []string{"x-"}, "fastest"))
}

func TestFakeObservatoryShuffledAndCopied(t *testing.T) {
	s, _ := xrayfake.Start(t)
	tags := []string{"a", "b", "c", "d"}
	var in []*observatory.OutboundStatus
	for _, tag := range tags {
		in = append(in, obs(tag, true, 1))
	}
	s.SetObservation(in...)
	in[0].OutboundTag = "mutated" // SetObservation keeps its own copies
	raw := obscmd.NewObservatoryServiceClient(rawConn(t, s))
	orders := map[string]bool{}
	for range 100 {
		resp, err := raw.GetOutboundStatus(t.Context(), &obscmd.GetOutboundStatusRequest{})
		if err != nil {
			t.Fatalf("GetOutboundStatus: %v", err)
		}
		var got []string
		for _, st := range resp.GetStatus().GetStatus() {
			got = append(got, st.GetOutboundTag())
		}
		if sorted := slices.Sorted(slices.Values(got)); !slices.Equal(sorted, tags) {
			t.Fatalf("tags = %v, want %v", sorted, tags)
		}
		orders[strings.Join(got, ",")] = true
	}
	if len(orders) < 2 {
		t.Fatalf("observatory order never shuffled")
	}
}

func TestFakeWithoutObservatoryService(t *testing.T) {
	s, _ := xrayfake.Start(t, xrayfake.WithoutObservatoryService())
	_, err := obscmd.NewObservatoryServiceClient(rawConn(t, s)).GetOutboundStatus(t.Context(), &obscmd.GetOutboundStatusRequest{})
	st := status.Convert(err)
	if st.Code() != codes.Unimplemented ||
		st.Message() != "unknown service xray.core.app.observatory.command.ObservatoryService" {
		t.Fatalf("GetOutboundStatus = %v", err)
	}
}
