package xrayapi_test

import (
	"errors"
	"net"
	"slices"
	"testing"
	"time"

	"github.com/xtls/xray-core/app/proxyman"
	"github.com/xtls/xray-core/app/router"
	xnet "github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/protocol"
	"github.com/xtls/xray-core/common/serial"
	"github.com/xtls/xray-core/core"
	"github.com/xtls/xray-core/proxy/socks"
	"github.com/xtls/xray-core/proxy/vless"
	vlessout "github.com/xtls/xray-core/proxy/vless/outbound"
	"github.com/xtls/xray-core/transport/internet"
	"github.com/xtls/xray-core/transport/internet/tls"
	"github.com/xtls/xray-core/transport/internet/websocket"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/a1ekseev/scd/internal/scd/xrayapi"
	"github.com/a1ekseev/scd/internal/scd/xrayapi/xrayfake"
)

func vlessOutbound(tag string) *core.OutboundHandlerConfig {
	sec := &tls.Config{ServerName: "cdn.example.com", Fingerprint: "firefox", NextProtocol: []string{"http/1.1"}}
	stream := &internet.StreamConfig{
		ProtocolName:     "websocket",
		SecurityType:     serial.GetMessageType(sec),
		SecuritySettings: []*serial.TypedMessage{serial.ToTypedMessage(sec)},
		TransportSettings: []*internet.TransportConfig{{
			ProtocolName: "websocket",
			Settings:     serial.ToTypedMessage(&websocket.Config{Host: "ws.example.com", Path: "/ws"}),
		}},
	}
	return &core.OutboundHandlerConfig{
		Tag:            tag,
		SenderSettings: serial.ToTypedMessage(&proxyman.SenderConfig{StreamSettings: stream}),
		ProxySettings: serial.ToTypedMessage(&vlessout.Config{Vnext: &protocol.ServerEndpoint{
			Address: xnet.NewIPOrDomain(xnet.DomainAddress("203.0.113.10")),
			Port:    443,
			User: &protocol.User{Email: tag, Account: serial.ToTypedMessage(&vless.Account{
				Id: "7d1b6590-1069-4372-92be-8d0a0ae6eaf5", Encryption: "none",
			})},
		}}),
	}
}

func socksInbound(tag string, port uint32) *core.InboundHandlerConfig {
	return &core.InboundHandlerConfig{
		Tag: tag,
		ReceiverSettings: serial.ToTypedMessage(&proxyman.ReceiverConfig{
			PortList: &xnet.PortList{Range: []*xnet.PortRange{xnet.SinglePortRange(xnet.Port(port))}},
			Listen:   xnet.NewIPOrDomain(xnet.LocalHostIP),
		}),
		ProxySettings: serial.ToTypedMessage(&socks.ServerConfig{UdpEnabled: true}),
	}
}

func apiErrorText(t *testing.T, err error) string {
	t.Helper()
	if err == nil {
		t.Fatal("error = nil, want *xrayapi.Error")
	}
	var apiErr *xrayapi.Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("error %T %v is not *xrayapi.Error", err, err)
	}
	if apiErr.Error() != apiErr.Message {
		t.Fatalf("Error() = %q, Message = %q", apiErr.Error(), apiErr.Message)
	}
	return apiErr.Message
}

func TestClientRoundTrip(t *testing.T) {
	s, c := xrayfake.Start(t,
		xrayfake.WithStaticOutbounds("direct"),
		xrayfake.WithStaticInbounds("socks-proxy"),
		xrayfake.WithStaticRules(xrayapi.Rule{Tag: "direct", RuleTag: "direct"}),
	)
	ctx := t.Context()

	t.Run("outbound", func(t *testing.T) {
		sent := vlessOutbound("out-t1")
		if err := c.AddOutbound(ctx, sent); err != nil {
			t.Fatalf("AddOutbound: %v", err)
		}
		list, err := c.ListOutbounds(ctx)
		if err != nil {
			t.Fatalf("ListOutbounds: %v", err)
		}
		i := slices.IndexFunc(list, func(h *core.OutboundHandlerConfig) bool { return h.GetTag() == "out-t1" })
		if len(list) != 2 || i < 0 {
			t.Fatalf("ListOutbounds = %v", list)
		}
		if !proto.Equal(list[i], sent) {
			t.Fatalf("listed outbound differs from sent:\n got %v\nwant %v", list[i], sent)
		}
		if err := c.RemoveOutbound(ctx, "out-t1"); err != nil {
			t.Fatalf("RemoveOutbound: %v", err)
		}
		if got := s.OutboundTags(); !slices.Equal(got, []string{"direct"}) {
			t.Fatalf("OutboundTags = %v", got)
		}
	})

	t.Run("inbound", func(t *testing.T) {
		sent := socksInbound("in-t1", 20001)
		if err := c.AddInbound(ctx, sent); err != nil {
			t.Fatalf("AddInbound: %v", err)
		}
		list, err := c.ListInbounds(ctx)
		if err != nil {
			t.Fatalf("ListInbounds: %v", err)
		}
		i := slices.IndexFunc(list, func(h *core.InboundHandlerConfig) bool { return h.GetTag() == "in-t1" })
		if len(list) != 2 || i < 0 || !proto.Equal(list[i], sent) {
			t.Fatalf("ListInbounds = %v", list)
		}
		if err := c.RemoveInbound(ctx, "in-t1"); err != nil {
			t.Fatalf("RemoveInbound: %v", err)
		}
		if got := s.InboundTags(); !slices.Equal(got, []string{"socks-proxy"}) {
			t.Fatalf("InboundTags = %v", got)
		}
	})

	t.Run("routing", func(t *testing.T) {
		rule := &router.RoutingRule{
			TargetTag:  &router.RoutingRule_Tag{Tag: "out-t1"},
			InboundTag: []string{"in-t1"},
			RuleTag:    "route-t1",
		}
		if err := c.AddRule(ctx, rule); err != nil {
			t.Fatalf("AddRule: %v", err)
		}
		got, err := c.ListRules(ctx)
		if err != nil {
			t.Fatalf("ListRules: %v", err)
		}
		want := []xrayapi.Rule{{Tag: "direct", RuleTag: "direct"}, {Tag: "out-t1", RuleTag: "route-t1"}}
		if !slices.Equal(got, want) {
			t.Fatalf("ListRules = %v, want %v", got, want)
		}
		if err := c.RemoveRule(ctx, "route-t1"); err != nil {
			t.Fatalf("RemoveRule: %v", err)
		}
		if got := s.Rules(); !slices.Equal(got, want[:1]) {
			t.Fatalf("Rules = %v", got)
		}
	})

	wantCalls := []string{
		"AddOutbound:out-t1", "ListOutbounds", "RemoveOutbound:out-t1",
		"AddInbound:in-t1", "ListInbounds", "RemoveInbound:in-t1",
		"AddRule:route-t1->out-t1", "ListRule", "RemoveRule:route-t1",
	}
	if got := s.Calls(); !slices.Equal(got, wantCalls) {
		t.Fatalf("Calls = %v, want %v", got, wantCalls)
	}
}

func TestErrorConnectionRefused(t *testing.T) {
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	c, err := xrayapi.Dial(addr, 5*time.Second)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()
	start := time.Now()
	_, err = c.ListOutbounds(t.Context())
	want := "Unable to connect to Xray API at " + addr + `. Start the Xray service first, for example with "docker compose up -d".`
	if got := apiErrorText(t, err); got != want {
		t.Fatalf("error = %q, want %q", got, want)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("refused connection took %v, want fast failure (no WaitForReady)", d)
	}
}

func TestErrorTimeout(t *testing.T) {
	s, _ := xrayfake.Start(t)
	s.Delay("ListOutbounds", 200*time.Millisecond)
	c, err := s.Dialer()(s.Address(), 50*time.Millisecond)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	_, err = c.ListOutbounds(t.Context())
	want := "Request to Xray API at " + s.Address() + " timed out after 50ms."
	if got := apiErrorText(t, err); got != want {
		t.Fatalf("error = %q, want %q", got, want)
	}
}

func TestErrorPassesXrayMessage(t *testing.T) {
	_, c := xrayfake.Start(t, xrayfake.WithStaticOutbounds("x"))
	err := c.AddOutbound(t.Context(), vlessOutbound("x"))
	if got, want := apiErrorText(t, err), "app/proxyman/outbound: existing tag found: x"; got != want {
		t.Fatalf("error = %q, want %q", got, want)
	}
}

func TestErrorEmptyMessage(t *testing.T) {
	s, c := xrayfake.Start(t)
	s.FailNext("RemoveRule", status.Error(codes.Internal, ""))
	if got, want := apiErrorText(t, c.RemoveRule(t.Context(), "r")), "gRPC call failed with status 13"; got != want {
		t.Fatalf("error = %q, want %q", got, want)
	}
}

func TestErrorUnavailableOtherTextPassesThrough(t *testing.T) {
	s, c := xrayfake.Start(t)
	s.FailNext("ListRule", status.Error(codes.Unavailable, "transport is closing"))
	_, err := c.ListRules(t.Context())
	if got, want := apiErrorText(t, err), "transport is closing"; got != want {
		t.Fatalf("error = %q, want %q", got, want)
	}
}

func TestErrorOperationNotPermitted(t *testing.T) {
	s, c := xrayfake.Start(t)
	s.FailNext("ListInbounds", status.Error(codes.Unavailable, "dial tcp 10.0.0.1:8080: connect: operation not permitted"))
	_, err := c.ListInbounds(t.Context())
	want := "Unable to connect to Xray API at " + s.Address() + `. Start the Xray service first, for example with "docker compose up -d".`
	if got := apiErrorText(t, err); got != want {
		t.Fatalf("error = %q, want %q", got, want)
	}
}

func TestAddRuleRequestShape(t *testing.T) {
	// AddRule wraps the rule as router.Config{Rule: [r]} with ShouldAppend
	// (02 §1.2 #9); the fake rejects anything else, and appends.
	s, c := xrayfake.Start(t, xrayfake.WithStaticRules(xrayapi.Rule{Tag: "direct", RuleTag: "direct"}))
	rule := &router.RoutingRule{
		TargetTag:  &router.RoutingRule_Tag{Tag: "o"},
		InboundTag: []string{"i"},
		RuleTag:    "r",
	}
	if err := c.AddRule(t.Context(), rule); err != nil {
		t.Fatalf("AddRule: %v", err)
	}
	if got, want := s.Rules(), []xrayapi.Rule{{Tag: "direct", RuleTag: "direct"}, {Tag: "o", RuleTag: "r"}}; !slices.Equal(got, want) {
		t.Fatalf("Rules = %v, want %v", got, want)
	}
}
