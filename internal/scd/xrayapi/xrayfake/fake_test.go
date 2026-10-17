package xrayfake_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/xtls/xray-core/app/proxyman"
	handlercmd "github.com/xtls/xray-core/app/proxyman/command"
	"github.com/xtls/xray-core/app/router"
	routercmd "github.com/xtls/xray-core/app/router/command"
	xnet "github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/serial"
	"github.com/xtls/xray-core/core"
	"github.com/xtls/xray-core/proxy/freedom"
	"github.com/xtls/xray-core/proxy/socks"
	"github.com/xtls/xray-core/transport/internet"
	"github.com/xtls/xray-core/transport/internet/tcp"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/a1ekseev/scd/internal/scd/xrayapi"
	"github.com/a1ekseev/scd/internal/scd/xrayapi/xrayfake"
)

func outboundCfg(tag string) *core.OutboundHandlerConfig {
	return &core.OutboundHandlerConfig{
		Tag:            tag,
		SenderSettings: serial.ToTypedMessage(&proxyman.SenderConfig{}),
		ProxySettings:  serial.ToTypedMessage(&freedom.Config{}),
	}
}

func inboundCfg(tag string, port uint32) *core.InboundHandlerConfig {
	return &core.InboundHandlerConfig{
		Tag: tag,
		ReceiverSettings: serial.ToTypedMessage(&proxyman.ReceiverConfig{
			PortList: &xnet.PortList{Range: []*xnet.PortRange{xnet.SinglePortRange(xnet.Port(port))}},
			Listen:   xnet.NewIPOrDomain(xnet.LocalHostIP),
		}),
		ProxySettings: serial.ToTypedMessage(&socks.ServerConfig{}),
	}
}

func ruleCfg(ruleTag, outboundTag, inboundTag string) *router.RoutingRule {
	return &router.RoutingRule{
		TargetTag:  &router.RoutingRule_Tag{Tag: outboundTag},
		InboundTag: []string{inboundTag},
		RuleTag:    ruleTag,
	}
}

func streamWith(settings *serial.TypedMessage) *internet.StreamConfig {
	return &internet.StreamConfig{
		ProtocolName:      "tcp",
		TransportSettings: []*internet.TransportConfig{{ProtocolName: "tcp", Settings: settings}},
	}
}

// rawConn opens a raw gRPC connection to the fake, to observe status codes
// that xrayapi.Client maps away.
func rawConn(t *testing.T, s *xrayfake.Server) *grpc.ClientConn {
	t.Helper()
	conn, err := grpc.NewClient("passthrough:///"+s.Address(), s.DialOptions()...)
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func wantErrText(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("error = nil, want %q", want)
	}
	var apiErr *xrayapi.Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("error %T %v is not *xrayapi.Error", err, err)
	}
	if apiErr.Message != want {
		t.Fatalf("error = %q, want %q", apiErr.Message, want)
	}
}

func TestFakeDuplicateOutbound(t *testing.T) {
	s, c := xrayfake.Start(t, xrayfake.WithStaticOutbounds("direct"))
	ctx := t.Context()
	if err := c.AddOutbound(ctx, outboundCfg("x")); err != nil {
		t.Fatalf("AddOutbound: %v", err)
	}
	wantErrText(t, c.AddOutbound(ctx, outboundCfg("x")), "app/proxyman/outbound: existing tag found: x")
	wantErrText(t, c.AddOutbound(ctx, outboundCfg("direct")), "app/proxyman/outbound: existing tag found: direct")

	_, err := handlercmd.NewHandlerServiceClient(rawConn(t, s)).AddOutbound(ctx, &handlercmd.AddOutboundRequest{Outbound: outboundCfg("x")})
	if got := status.Code(err); got != codes.Unknown {
		t.Fatalf("code = %v, want Unknown", got)
	}
	if got, want := s.OutboundTags(), []string{"direct", "x"}; !slices.Equal(got, want) {
		t.Fatalf("OutboundTags = %v, want %v", got, want)
	}
}

func TestFakeRemoveUnknownOutboundOK(t *testing.T) {
	s, c := xrayfake.Start(t, xrayfake.WithStaticOutbounds("direct"))
	if err := c.RemoveOutbound(t.Context(), "does-not-exist"); err != nil {
		t.Fatalf("RemoveOutbound unknown: %v", err)
	}
	if err := c.RemoveOutbound(t.Context(), "direct"); err != nil {
		t.Fatalf("RemoveOutbound direct: %v", err)
	}
	if got := s.OutboundTags(); len(got) != 0 {
		t.Fatalf("OutboundTags = %v, want empty", got)
	}
	wantErrText(t, c.RemoveOutbound(t.Context(), ""), "common: not enough information for making a decision")
}

func TestFakeDuplicateInbound(t *testing.T) {
	_, c := xrayfake.Start(t, xrayfake.WithStaticInbounds("socks-proxy"))
	if err := c.AddInbound(t.Context(), inboundCfg("in-t1", 20001)); err != nil {
		t.Fatalf("AddInbound: %v", err)
	}
	wantErrText(t, c.AddInbound(t.Context(), inboundCfg("in-t1", 20002)), "app/proxyman/inbound: existing tag found: in-t1")
	wantErrText(t, c.AddInbound(t.Context(), inboundCfg("socks-proxy", 20003)), "app/proxyman/inbound: existing tag found: socks-proxy")
}

func TestFakeRemoveUnknownInboundFails(t *testing.T) {
	s, c := xrayfake.Start(t, xrayfake.WithStaticInbounds("socks-proxy"))
	wantErrText(t, c.RemoveInbound(t.Context(), "does-not-exist"), "common: not enough information for making a decision")
	wantErrText(t, c.RemoveInbound(t.Context(), ""), "common: not enough information for making a decision")
	if err := c.RemoveInbound(t.Context(), "socks-proxy"); err != nil {
		t.Fatalf("RemoveInbound existing: %v", err)
	}
	if got := s.InboundTags(); len(got) != 0 {
		t.Fatalf("InboundTags = %v, want empty", got)
	}
}

func TestFakeAddRuleAppendsAndRejectsDuplicate(t *testing.T) {
	s, c := xrayfake.Start(t, xrayfake.WithStaticRules(
		xrayapi.Rule{Tag: "direct", RuleTag: "direct"},
		xrayapi.Rule{Tag: "", RuleTag: "x-balancer"},
	))
	ctx := t.Context()
	if err := c.AddRule(ctx, ruleCfg("route-t1", "out-t1", "in-t1")); err != nil {
		t.Fatalf("AddRule: %v", err)
	}
	if err := c.AddRule(ctx, ruleCfg("route-t2", "out-t2", "in-t2")); err != nil {
		t.Fatalf("AddRule: %v", err)
	}
	want := []xrayapi.Rule{
		{Tag: "direct", RuleTag: "direct"},
		{Tag: "", RuleTag: "x-balancer"},
		{Tag: "out-t1", RuleTag: "route-t1"},
		{Tag: "out-t2", RuleTag: "route-t2"},
	}
	if got := s.Rules(); !slices.Equal(got, want) {
		t.Fatalf("Rules = %v, want %v", got, want)
	}
	wantErrText(t, c.AddRule(ctx, ruleCfg("route-t1", "other", "in-x")), "app/router: duplicate ruleTag route-t1")
	wantErrText(t, c.AddRule(ctx, ruleCfg("direct", "other", "in-x")), "app/router: duplicate ruleTag direct")
	if got := s.Rules(); !slices.Equal(got, want) {
		t.Fatalf("Rules after duplicate = %v, want unchanged %v", got, want)
	}
	// Empty ruleTags may repeat.
	for range 2 {
		if err := c.AddRule(ctx, ruleCfg("", "out-e", "in-e")); err != nil {
			t.Fatalf("AddRule empty ruleTag: %v", err)
		}
	}
	if got := len(s.Rules()); got != 6 {
		t.Fatalf("len(Rules) = %d, want 6", got)
	}
}

func TestFakeAddRuleRejectsRuleWithoutConditions(t *testing.T) {
	s, c := xrayfake.Start(t)
	err := c.AddRule(t.Context(), &router.RoutingRule{TargetTag: &router.RoutingRule_Tag{Tag: "o"}, RuleTag: "r"})
	wantErrText(t, err, "app/router: this rule has no effective fields")
	if got := s.Rules(); len(got) != 0 {
		t.Fatalf("Rules = %v, want empty", got)
	}
}

func TestFakeRemoveEmptyRuleTag(t *testing.T) {
	s, c := xrayfake.Start(t, xrayfake.WithStaticRules(xrayapi.Rule{Tag: "direct", RuleTag: "direct"}))
	ctx := t.Context()
	wantErrText(t, c.RemoveRule(ctx, ""), "app/router: empty tag name!")
	if err := c.RemoveRule(ctx, "nope"); err != nil {
		t.Fatalf("RemoveRule unknown: %v", err)
	}
	for _, r := range []*router.RoutingRule{ruleCfg("", "a", "i"), ruleCfg("dup", "b", "i")} {
		if err := c.AddRule(ctx, r); err != nil {
			t.Fatalf("AddRule: %v", err)
		}
	}
	if err := c.RemoveRule(ctx, "direct"); err != nil {
		t.Fatalf("RemoveRule: %v", err)
	}
	want := []xrayapi.Rule{{Tag: "a", RuleTag: ""}, {Tag: "b", RuleTag: "dup"}}
	if got := s.Rules(); !slices.Equal(got, want) {
		t.Fatalf("Rules = %v, want %v", got, want)
	}
}

func TestFakeRemoveRuleRemovesAllWithTag(t *testing.T) {
	s, c := xrayfake.Start(t, xrayfake.WithStaticRules(
		xrayapi.Rule{Tag: "a", RuleTag: "same"},
		xrayapi.Rule{Tag: "b", RuleTag: "keep"},
		xrayapi.Rule{Tag: "c", RuleTag: "same"},
	))
	if err := c.RemoveRule(t.Context(), "same"); err != nil {
		t.Fatalf("RemoveRule: %v", err)
	}
	if got, want := s.Rules(), []xrayapi.Rule{{Tag: "b", RuleTag: "keep"}}; !slices.Equal(got, want) {
		t.Fatalf("Rules = %v, want %v", got, want)
	}
}

func TestFakeListRuleShape(t *testing.T) {
	s, c := xrayfake.Start(t, xrayfake.WithStaticRules(
		xrayapi.Rule{Tag: "direct", RuleTag: "direct"},
		xrayapi.Rule{Tag: "", RuleTag: "x-balancer"},
	))
	if err := c.AddRule(t.Context(), ruleCfg("route-t1", "out-t1", "in-t1")); err != nil {
		t.Fatalf("AddRule: %v", err)
	}
	got, err := c.ListRules(t.Context())
	if err != nil {
		t.Fatalf("ListRules: %v", err)
	}
	want := []xrayapi.Rule{
		{Tag: "direct", RuleTag: "direct"},
		{Tag: "", RuleTag: "x-balancer"},
		{Tag: "out-t1", RuleTag: "route-t1"},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("ListRules = %v, want %v", got, want)
	}
	resp, err := routercmd.NewRoutingServiceClient(rawConn(t, s)).ListRule(t.Context(), &routercmd.ListRuleRequest{})
	if err != nil {
		t.Fatalf("raw ListRule: %v", err)
	}
	wantItem := &routercmd.ListRuleItem{Tag: "out-t1", RuleTag: "route-t1"}
	if item := resp.GetRules()[2]; !proto.Equal(item, wantItem) {
		t.Fatalf("raw item = %v, want only {Tag, RuleTag} %v", item, wantItem)
	}
}

func TestFakeRejectsUnknownTypedMessage(t *testing.T) {
	s, _ := xrayfake.Start(t)
	conn := rawConn(t, s)
	h := handlercmd.NewHandlerServiceClient(conn)
	r := routercmd.NewRoutingServiceClient(conn)
	bogus := &serial.TypedMessage{Type: "xray.bogus.Config"}
	ctx := t.Context()

	topLevel := outboundCfg("bad-top")
	topLevel.ProxySettings = bogus
	nested := outboundCfg("bad-nested")
	nested.SenderSettings = serial.ToTypedMessage(&proxyman.SenderConfig{
		StreamSettings: streamWith(serial.ToTypedMessage(&tcp.Config{})),
	})
	nestedStream := outboundCfg("bad-stream")
	nestedStream.SenderSettings = serial.ToTypedMessage(&proxyman.SenderConfig{
		StreamSettings: streamWith(bogus),
	})
	inbound := inboundCfg("bad-in", 20001)
	inbound.ProxySettings = bogus

	cases := []struct {
		name string
		call func() error
	}{
		{"outbound proxy settings", func() error {
			_, err := h.AddOutbound(ctx, &handlercmd.AddOutboundRequest{Outbound: topLevel})
			return err
		}},
		{"outbound nested stream settings", func() error {
			_, err := h.AddOutbound(ctx, &handlercmd.AddOutboundRequest{Outbound: nestedStream})
			return err
		}},
		{"inbound proxy settings", func() error {
			_, err := h.AddInbound(ctx, &handlercmd.AddInboundRequest{Inbound: inbound})
			return err
		}},
		{"rule config", func() error {
			_, err := r.AddRule(ctx, &routercmd.AddRuleRequest{Config: bogus, ShouldAppend: true})
			return err
		}},
		{"rule config of wrong type", func() error {
			_, err := r.AddRule(ctx, &routercmd.AddRuleRequest{Config: serial.ToTypedMessage(&freedom.Config{}), ShouldAppend: true})
			return err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := status.Code(tc.call()); got != codes.Unknown {
				t.Fatalf("code = %v, want Unknown", got)
			}
		})
	}
	// A valid nested message passes.
	if _, err := h.AddOutbound(ctx, &handlercmd.AddOutboundRequest{Outbound: nested}); err != nil {
		t.Fatalf("valid nested AddOutbound: %v", err)
	}
	if got, want := s.OutboundTags(), []string{"bad-nested"}; !slices.Equal(got, want) {
		t.Fatalf("OutboundTags = %v, want %v", got, want)
	}
	if got := s.InboundTags(); len(got) != 0 {
		t.Fatalf("InboundTags = %v, want empty", got)
	}
	if got := s.Rules(); len(got) != 0 {
		t.Fatalf("Rules = %v, want empty", got)
	}
}

func TestFakeListOrderShuffled(t *testing.T) {
	tags := []string{"a", "b", "c", "d"}
	_, c := xrayfake.Start(t, xrayfake.WithStaticOutbounds(tags...), xrayfake.WithStaticInbounds(tags...))
	ctx := t.Context()
	outOrders := map[string]bool{}
	inOrders := map[string]bool{}
	for range 100 {
		outs, err := c.ListOutbounds(ctx)
		if err != nil {
			t.Fatalf("ListOutbounds: %v", err)
		}
		ins, err := c.ListInbounds(ctx)
		if err != nil {
			t.Fatalf("ListInbounds: %v", err)
		}
		var o, i []string
		for _, h := range outs {
			o = append(o, h.GetTag())
		}
		for _, h := range ins {
			i = append(i, h.GetTag())
		}
		if got := slices.Sorted(slices.Values(o)); !slices.Equal(got, tags) {
			t.Fatalf("outbound tags = %v, want %v", got, tags)
		}
		if got := slices.Sorted(slices.Values(i)); !slices.Equal(got, tags) {
			t.Fatalf("inbound tags = %v, want %v", got, tags)
		}
		outOrders[strings.Join(o, ",")] = true
		inOrders[strings.Join(i, ",")] = true
	}
	if len(outOrders) < 2 || len(inOrders) < 2 {
		t.Fatalf("orders: outbounds %d, inbounds %d distinct; want >= 2 each", len(outOrders), len(inOrders))
	}
}

func TestFakeListReturnsCopies(t *testing.T) {
	s, c := xrayfake.Start(t)
	sent := outboundCfg("x")
	if err := c.AddOutbound(t.Context(), sent); err != nil {
		t.Fatalf("AddOutbound: %v", err)
	}
	if got := s.Outbound("x"); !proto.Equal(got, sent) {
		t.Fatalf("Outbound(x) = %v, want %v", got, sent)
	}
	s.Outbound("x").Tag = "mutated"
	if got := s.OutboundTags(); !slices.Equal(got, []string{"x"}) {
		t.Fatalf("state mutated through accessor: %v", got)
	}
	if s.Outbound("missing") != nil {
		t.Fatal("Outbound(missing) != nil")
	}
}

func TestFakeCallsRecorded(t *testing.T) {
	s, c := xrayfake.Start(t)
	ctx := t.Context()
	_ = c.AddOutbound(ctx, outboundCfg("o1"))
	_ = c.RemoveOutbound(ctx, "o1")
	_ = c.AddInbound(ctx, inboundCfg("i1", 20001))
	_ = c.RemoveInbound(ctx, "i1")
	_ = c.AddRule(ctx, ruleCfg("r1", "o1", "i1"))
	_ = c.RemoveRule(ctx, "r1")
	_, _ = c.ListOutbounds(ctx)
	_, _ = c.ListInbounds(ctx)
	_, _ = c.ListRules(ctx)
	want := []string{
		"AddOutbound:o1", "RemoveOutbound:o1", "AddInbound:i1", "RemoveInbound:i1",
		"AddRule:r1->o1", "RemoveRule:r1", "ListOutbounds", "ListInbounds", "ListRule",
	}
	if got := s.Calls(); !slices.Equal(got, want) {
		t.Fatalf("Calls = %v, want %v", got, want)
	}
}

func TestFakeFailNextIsOneShot(t *testing.T) {
	s, c := xrayfake.Start(t)
	s.FailNext("AddOutbound", status.Error(codes.Internal, "injected"))
	wantErrText(t, c.AddOutbound(t.Context(), outboundCfg("o1")), "injected")
	if got := s.OutboundTags(); len(got) != 0 {
		t.Fatalf("injected failure changed state: %v", got)
	}
	if err := c.AddOutbound(t.Context(), outboundCfg("o1")); err != nil {
		t.Fatalf("second AddOutbound: %v", err)
	}
	if got, want := s.Calls(), []string{"AddOutbound:o1", "AddOutbound:o1"}; !slices.Equal(got, want) {
		t.Fatalf("Calls = %v, want %v", got, want)
	}
}

func TestFakeDelay(t *testing.T) {
	s, _ := xrayfake.Start(t)
	s.Delay("ListOutbounds", 100*time.Millisecond)
	c, err := s.Dialer()(s.Address(), time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	start := time.Now()
	if _, err := c.ListOutbounds(context.Background()); err != nil {
		t.Fatalf("ListOutbounds: %v", err)
	}
	if d := time.Since(start); d < 100*time.Millisecond {
		t.Fatalf("ListOutbounds took %v, want >= 100ms", d)
	}
}

func TestFakeWithoutRoutingService(t *testing.T) {
	s, c := xrayfake.Start(t, xrayfake.WithoutRoutingService())
	_, err := c.ListRules(t.Context())
	wantErrText(t, err, "unknown service xray.app.router.command.RoutingService")
	_, err = routercmd.NewRoutingServiceClient(rawConn(t, s)).ListRule(t.Context(), &routercmd.ListRuleRequest{})
	if got := status.Code(err); got != codes.Unimplemented {
		t.Fatalf("code = %v, want Unimplemented", got)
	}
	if _, err := c.ListOutbounds(t.Context()); err != nil {
		t.Fatalf("HandlerService still served: %v", err)
	}
}
