package xrayapi_test

import (
	"context"
	"errors"
	"net"
	"slices"
	"testing"
	"time"

	"github.com/xtls/xray-core/app/observatory"
	routercmd "github.com/xtls/xray-core/app/router/command"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/a1ekseev/scd/internal/scd/xrayapi"
	"github.com/a1ekseev/scd/internal/scd/xrayapi/xrayfake"
)

// candidates are the four prefixed outbounds of the live stand (10 §5.1).
var candidates = []string{"x-observe-dead", "x-observe-fast", "x-observe-mid", "x-observe-slow"}

func startBalancers(t *testing.T, opts ...xrayfake.Option) (*xrayfake.Server, xrayapi.Client) {
	t.Helper()
	all := append([]xrayfake.Option{
		xrayfake.WithStaticOutbounds(append([]string{"direct", "tunnel-repaired"}, candidates...)...),
		xrayfake.WithBalancer("x-balancer", []string{"x-observe-"}, "leastPing"),
		xrayfake.WithBalancer("bal-load", []string{"x-observe-"}, "leastLoad"),
		xrayfake.WithBalancer("bal-random", []string{"x-observe-"}, "random"),
	}, opts...)
	return xrayfake.Start(t, all...)
}

func obs(tag string, alive bool, delay int64) *observatory.OutboundStatus {
	return &observatory.OutboundStatus{OutboundTag: tag, Alive: alive, Delay: delay}
}

func TestBalancerInfoNormalizesEmptyTag(t *testing.T) {
	s, c := startBalancers(t)
	// Every candidate is dead: leastPing answers [""], leastLoad [] (10 §5.2).
	s.SetObservation(obs("x-observe-dead", false, 99999999), obs("x-observe-fast", false, 99999999))
	for _, tag := range []string{"x-balancer", "bal-load"} {
		t.Run(tag, func(t *testing.T) {
			info, err := c.GetBalancerInfo(t.Context(), tag)
			if err != nil {
				t.Fatalf("GetBalancerInfo: %v", err)
			}
			if !info.PrincipleKnown || info.PrincipleTargets == nil || len(info.PrincipleTargets) != 0 {
				t.Fatalf("info = %#v, want known empty (non-nil) targets", info)
			}
			if info.OverrideTarget != "" {
				t.Fatalf("OverrideTarget = %q, want empty", info.OverrideTarget)
			}
		})
	}
	t.Run("alive", func(t *testing.T) {
		s.SetObservation(obs("x-observe-fast", true, 21), obs("x-observe-mid", true, 120))
		info, err := c.GetBalancerInfo(t.Context(), "x-balancer")
		if err != nil {
			t.Fatalf("GetBalancerInfo: %v", err)
		}
		want := xrayapi.BalancerInfo{PrincipleTargets: []string{"x-observe-fast"}, PrincipleKnown: true}
		if !balancerInfoEqual(info, want) {
			t.Fatalf("info = %#v, want %#v", info, want)
		}
	})
	t.Run("pool", func(t *testing.T) {
		info, err := c.GetBalancerInfo(t.Context(), "bal-random")
		if err != nil {
			t.Fatalf("GetBalancerInfo: %v", err)
		}
		if !info.PrincipleKnown || !slices.Equal(info.PrincipleTargets, candidates) {
			t.Fatalf("info = %#v, want all candidates", info)
		}
	})
}

func balancerInfoEqual(a, b xrayapi.BalancerInfo) bool {
	return a.OverrideTarget == b.OverrideTarget && a.PrincipleKnown == b.PrincipleKnown &&
		slices.Equal(a.PrincipleTargets, b.PrincipleTargets)
}

// stubRouting answers GetBalancerInfo with a fixed message, for shapes the
// fake (like Xray in practice) never produces.
type stubRouting struct {
	routercmd.UnimplementedRoutingServiceServer
	resp *routercmd.GetBalancerInfoResponse
}

func (s *stubRouting) GetBalancerInfo(context.Context, *routercmd.GetBalancerInfoRequest) (*routercmd.GetBalancerInfoResponse, error) {
	return s.resp, nil
}

func stubClient(t *testing.T, resp *routercmd.GetBalancerInfoResponse) xrayapi.Client {
	t.Helper()
	ln := bufconn.Listen(1 << 16)
	srv := grpc.NewServer()
	routercmd.RegisterRoutingServiceServer(srv, &stubRouting{resp: resp})
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(srv.Stop)
	c, err := xrayapi.DialWith("stub", time.Second, grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
		return ln.DialContext(ctx)
	}))
	if err != nil {
		t.Fatalf("DialWith: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestBalancerInfoNilPrinciple(t *testing.T) {
	// principle_target == nil when Xray only logged a GetPrincipleTarget
	// error (10 §2.2); override is still set.
	c := stubClient(t, &routercmd.GetBalancerInfoResponse{Balancer: &routercmd.BalancerMsg{
		Override: &routercmd.OverrideInfo{Target: "x-observe-mid"},
	}})
	info, err := c.GetBalancerInfo(t.Context(), "x-balancer")
	if err != nil {
		t.Fatalf("GetBalancerInfo: %v", err)
	}
	want := xrayapi.BalancerInfo{OverrideTarget: "x-observe-mid"}
	if !balancerInfoEqual(info, want) || info.PrincipleTargets != nil {
		t.Fatalf("info = %#v, want %#v", info, want)
	}

	t.Run("empty strings dropped", func(t *testing.T) {
		c := stubClient(t, &routercmd.GetBalancerInfoResponse{Balancer: &routercmd.BalancerMsg{
			PrincipleTarget: &routercmd.PrincipleTargetInfo{Tag: []string{"", "a", ""}},
		}})
		info, err := c.GetBalancerInfo(t.Context(), "b")
		if err != nil {
			t.Fatalf("GetBalancerInfo: %v", err)
		}
		if !info.PrincipleKnown || !slices.Equal(info.PrincipleTargets, []string{"a"}) {
			t.Fatalf("info = %#v", info)
		}
	})
	t.Run("nil balancer", func(t *testing.T) {
		c := stubClient(t, &routercmd.GetBalancerInfoResponse{})
		info, err := c.GetBalancerInfo(t.Context(), "b")
		if err != nil {
			t.Fatalf("GetBalancerInfo: %v", err)
		}
		if info.PrincipleKnown || info.OverrideTarget != "" || info.PrincipleTargets != nil {
			t.Fatalf("info = %#v, want zero", info)
		}
	})
}

func TestBalancerInfoOverride(t *testing.T) {
	s, c := startBalancers(t)
	s.SetObservation(obs("x-observe-fast", true, 21), obs("x-observe-slow", true, 300))
	s.SetOverride("x-balancer", "x-observe-slow")
	info, err := c.GetBalancerInfo(t.Context(), "x-balancer")
	if err != nil {
		t.Fatalf("GetBalancerInfo: %v", err)
	}
	// GetPrincipleTarget ignores the override (10 §2.3, §5.2).
	want := xrayapi.BalancerInfo{
		OverrideTarget: "x-observe-slow", PrincipleTargets: []string{"x-observe-fast"}, PrincipleKnown: true,
	}
	if !balancerInfoEqual(info, want) {
		t.Fatalf("info = %#v, want %#v", info, want)
	}
	s.SetOverride("x-balancer", "")
	info, err = c.GetBalancerInfo(t.Context(), "x-balancer")
	if err != nil {
		t.Fatalf("GetBalancerInfo: %v", err)
	}
	if info.OverrideTarget != "" {
		t.Fatalf("OverrideTarget after reset = %q", info.OverrideTarget)
	}
	if got, want := s.Calls(), []string{"GetBalancerInfo:x-balancer", "GetBalancerInfo:x-balancer"}; !slices.Equal(got, want) {
		t.Fatalf("Calls = %v, want %v", got, want)
	}
}

func TestBalancerNotFound(t *testing.T) {
	_, c := startBalancers(t)
	_, err := c.GetBalancerInfo(t.Context(), "no-such-balancer")
	var nf *xrayapi.BalancerNotFoundError
	if !errors.As(err, &nf) {
		t.Fatalf("error %T %v is not *BalancerNotFoundError", err, err)
	}
	if nf.Tag != "no-such-balancer" {
		t.Fatalf("Tag = %q", nf.Tag)
	}
	if got, want := err.Error(), `Xray balancer "no-such-balancer" not found.`; got != want {
		t.Fatalf("error = %q, want %q", got, want)
	}
}

func TestRoutingServiceDisabled(t *testing.T) {
	_, c := startBalancers(t, xrayfake.WithoutRoutingService())
	_, err := c.GetBalancerInfo(t.Context(), "x-balancer")
	var sd *xrayapi.ServiceDisabledError
	if !errors.As(err, &sd) {
		t.Fatalf("error %T %v is not *ServiceDisabledError", err, err)
	}
	if sd.Service != "RoutingService" {
		t.Fatalf("Service = %q", sd.Service)
	}
	if got, want := err.Error(), "Xray API service RoutingService is not enabled (add it to api.services)."; got != want {
		t.Fatalf("error = %q, want %q", got, want)
	}
	if _, err := c.GetOutboundStatus(t.Context()); err != nil {
		t.Fatalf("observatory must still work: %v", err)
	}
}

func TestObservatoryDisabled(t *testing.T) {
	_, c := startBalancers(t, xrayfake.WithoutObservatoryService())
	_, err := c.GetOutboundStatus(t.Context())
	var sd *xrayapi.ServiceDisabledError
	if !errors.As(err, &sd) {
		t.Fatalf("error %T %v is not *ServiceDisabledError", err, err)
	}
	if sd.Service != "ObservatoryService" {
		t.Fatalf("Service = %q", sd.Service)
	}
	if got, want := err.Error(), "Xray API service ObservatoryService is not enabled (add it to api.services)."; got != want {
		t.Fatalf("error = %q, want %q", got, want)
	}
	if _, err := c.GetBalancerInfo(t.Context(), "x-balancer"); err != nil {
		t.Fatalf("routing must still work: %v", err)
	}
}

func TestReadRPCGenericErrors(t *testing.T) {
	s, _ := startBalancers(t)
	c, err := s.Dialer()(s.Address(), 50*time.Millisecond)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	s.FailNext("GetBalancerInfo", status.Error(codes.Unknown, "app/router: unable to select outbounds"))
	_, err = c.GetBalancerInfo(t.Context(), "x-balancer")
	if got := apiErrorText(t, err); got != "app/router: unable to select outbounds" {
		t.Fatalf("error = %q", got)
	}

	// Unimplemented without "unknown service" is not "service disabled".
	s.FailNext("GetOutboundStatus", status.Error(codes.Unimplemented, "method X not implemented"))
	_, err = c.GetOutboundStatus(t.Context())
	if got := apiErrorText(t, err); got != "method X not implemented" {
		t.Fatalf("error = %q", got)
	}

	s.Delay("GetOutboundStatus", 200*time.Millisecond)
	_, err = c.GetOutboundStatus(t.Context())
	if got, want := apiErrorText(t, err), "Request to Xray API at "+s.Address()+" timed out after 50ms."; got != want {
		t.Fatalf("error = %q, want %q", got, want)
	}
}

func TestObservationDecoding(t *testing.T) {
	s, c := startBalancers(t)
	s.SetObservation(
		&observatory.OutboundStatus{
			OutboundTag: "x-observe-mid", Alive: true, Delay: 120,
			LastSeenTime: 1792229310, LastTryTime: 1792229311,
		},
		&observatory.OutboundStatus{
			OutboundTag: "x-observe-dead", Alive: false, Delay: 99999999,
			LastErrorReason: "the outbound x-observe-dead is dead: GET request failed",
			LastTryTime:     1792229311,
		},
		// burstObservatory: dead → delay 0, times 0, health_ping in ns.
		&observatory.OutboundStatus{
			OutboundTag: "x-observe-burst", Alive: false, Delay: 0,
			HealthPing: &observatory.HealthPingMeasurementResult{All: 3, Fail: 3},
		},
		&observatory.OutboundStatus{
			OutboundTag: "x-observe-fast", Alive: true, Delay: 21,
			HealthPing: &observatory.HealthPingMeasurementResult{
				All: 5, Fail: 1, Deviation: 6_100_000, Average: 21_748_562, Min: 20_000_000, Max: 35_000_000,
			},
		},
	)
	got, err := c.GetOutboundStatus(t.Context())
	if err != nil {
		t.Fatalf("GetOutboundStatus: %v", err)
	}
	ptr := func(v int64) *int64 { return &v }
	at := func(sec int64) *time.Time { v := time.Unix(sec, 0); return &v }
	want := []xrayapi.OutboundObservation{
		{
			Tag:        "x-observe-burst",
			HealthPing: &xrayapi.HealthPing{All: 3, Fail: 3},
		},
		{
			Tag: "x-observe-dead", LastError: "the outbound x-observe-dead is dead: GET request failed",
			LastTryAt: at(1792229311),
		},
		{
			Tag: "x-observe-fast", Alive: true, DelayMs: ptr(21),
			HealthPing: &xrayapi.HealthPing{
				All: 5, Fail: 1, Deviation: 6100 * time.Microsecond, Average: 21748562 * time.Nanosecond,
				Min: 20 * time.Millisecond, Max: 35 * time.Millisecond,
			},
		},
		{
			Tag: "x-observe-mid", Alive: true, DelayMs: ptr(120),
			LastSeenAt: at(1792229310), LastTryAt: at(1792229311),
		},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d observations, want %d: %#v", len(got), len(want), got)
	}
	for i := range want {
		if !observationEqual(got[i], want[i]) {
			t.Errorf("[%d] = %s, want %s", i, fmtObs(got[i]), fmtObs(want[i]))
		}
	}
	if got := s.Calls(); !slices.Equal(got, []string{"GetOutboundStatus"}) {
		t.Fatalf("Calls = %v", got)
	}

	t.Run("empty", func(t *testing.T) {
		s.SetObservation()
		got, err := c.GetOutboundStatus(t.Context())
		if err != nil || got == nil || len(got) != 0 {
			t.Fatalf("GetOutboundStatus = %#v, %v; want empty non-nil", got, err)
		}
	})
}

func observationEqual(a, b xrayapi.OutboundObservation) bool {
	eqPtr := func(x, y *int64) bool { return (x == nil) == (y == nil) && (x == nil || *x == *y) }
	eqTime := func(x, y *time.Time) bool { return (x == nil) == (y == nil) && (x == nil || x.Equal(*y)) }
	eqHP := func(x, y *xrayapi.HealthPing) bool { return (x == nil) == (y == nil) && (x == nil || *x == *y) }
	return a.Tag == b.Tag && a.Alive == b.Alive && a.LastError == b.LastError &&
		eqPtr(a.DelayMs, b.DelayMs) && eqTime(a.LastSeenAt, b.LastSeenAt) &&
		eqTime(a.LastTryAt, b.LastTryAt) && eqHP(a.HealthPing, b.HealthPing)
}

func fmtObs(o xrayapi.OutboundObservation) string {
	s := o.Tag
	if o.DelayMs != nil {
		s += " delay=" + time.Duration(*o.DelayMs*int64(time.Millisecond)).String()
	}
	if o.LastSeenAt != nil {
		s += " seen=" + o.LastSeenAt.UTC().String()
	}
	if o.LastTryAt != nil {
		s += " try=" + o.LastTryAt.UTC().String()
	}
	if o.HealthPing != nil {
		s += " hp=" + o.HealthPing.Average.String() + "/" + o.HealthPing.Deviation.String()
	}
	return s
}
