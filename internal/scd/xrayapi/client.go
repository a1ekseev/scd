// Package xrayapi is the only scd package that talks gRPC to Xray: a Client
// over the xray-core command services (HandlerService and RoutingService
// today; read-only balancer/observatory RPCs follow in task 5.5) built on the
// native xray-core v26.10.10 types (D2, D4). One *grpc.ClientConn is kept per
// target, every RPC runs under its own context.WithTimeout(timeout), and every
// error is mapped to *Error with the texts of 02 §2.7.
package xrayapi

import (
	"context"
	"time"

	handlercmd "github.com/xtls/xray-core/app/proxyman/command"
	"github.com/xtls/xray-core/app/router"
	routercmd "github.com/xtls/xray-core/app/router/command"
	"github.com/xtls/xray-core/common/serial"
	"github.com/xtls/xray-core/core"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// Rule is one ListRule item: the outbound tag (empty for a balancer rule)
// and the ruleTag. Xray returns nothing else (02 §1.5).
type Rule struct {
	Tag     string
	RuleTag string
}

// OutboundAPI is the HandlerService outbound part.
type OutboundAPI interface {
	// ListOutbounds returns all outbounds except the commander one, in
	// Xray's nondeterministic order.
	ListOutbounds(ctx context.Context) ([]*core.OutboundHandlerConfig, error)
	// AddOutbound adds h; a duplicate tag fails.
	AddOutbound(ctx context.Context, h *core.OutboundHandlerConfig) error
	// RemoveOutbound removes the outbound; an unknown tag succeeds.
	RemoveOutbound(ctx context.Context, tag string) error
}

// InboundAPI is the HandlerService inbound part.
type InboundAPI interface {
	// ListInbounds returns the full inbound configs (isOnlyTags=false).
	ListInbounds(ctx context.Context) ([]*core.InboundHandlerConfig, error)
	// AddInbound adds h; a duplicate tag fails.
	AddInbound(ctx context.Context, h *core.InboundHandlerConfig) error
	// RemoveInbound removes the inbound; an unknown tag fails.
	RemoveInbound(ctx context.Context, tag string) error
}

// RoutingAPI is the RoutingService rule part.
type RoutingAPI interface {
	// ListRules returns the rules in router order.
	ListRules(ctx context.Context) ([]Rule, error)
	// AddRule appends r (AddRule with router.Config{Rule: [r]} and
	// shouldAppend=true).
	AddRule(ctx context.Context, r *router.RoutingRule) error
	// RemoveRule removes every rule with ruleTag; "" fails, unknown succeeds.
	RemoveRule(ctx context.Context, ruleTag string) error
}

// BalancerAPI holds the read-only RoutingService balancer RPC
// (GetBalancerInfo, D22); its methods are added in task 5.5.
type BalancerAPI any

// ObservatoryAPI holds the read-only ObservatoryService RPC
// (GetOutboundStatus, D22); its methods are added in task 5.5.
type ObservatoryAPI any

// Client is the complete Xray API surface used by scd.
type Client interface {
	OutboundAPI
	InboundAPI
	RoutingAPI
	BalancerAPI
	ObservatoryAPI
	// Close releases the underlying connection.
	Close() error
}

// Dialer creates a Client for a target address with a per-RPC timeout.
type Dialer func(address string, timeout time.Duration) (Client, error)

// Dial creates a Client for address (host:port, h2c without TLS). It does not
// connect eagerly and does not use WaitForReady, so a closed port fails fast
// with the "Unable to connect" error instead of waiting for the deadline.
func Dial(address string, timeout time.Duration) (Client, error) {
	return DialWith(address, timeout)
}

// DialWith is Dial with extra grpc.DialOptions appended after the insecure
// transport credentials (e.g. a bufconn context dialer). The address is used
// both as the passthrough target and in error texts.
func DialWith(address string, timeout time.Duration, opts ...grpc.DialOption) (Client, error) {
	all := append([]grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}, opts...)
	conn, err := grpc.NewClient("passthrough:///"+address, all...)
	if err != nil {
		return nil, &Error{Message: err.Error()}
	}
	return &client{
		conn:    conn,
		address: address,
		timeout: timeout,
		handler: handlercmd.NewHandlerServiceClient(conn),
		routing: routercmd.NewRoutingServiceClient(conn),
	}, nil
}

type client struct {
	conn    *grpc.ClientConn
	address string
	timeout time.Duration
	handler handlercmd.HandlerServiceClient
	routing routercmd.RoutingServiceClient
}

// call runs one RPC under its own timeout and maps its error.
func call[Resp any](ctx context.Context, c *client, rpc func(context.Context) (Resp, error)) (Resp, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	resp, err := rpc(ctx)
	if err != nil {
		var zero Resp
		return zero, mapError(err, c.address, c.timeout)
	}
	return resp, nil
}

func (c *client) ListOutbounds(ctx context.Context) ([]*core.OutboundHandlerConfig, error) {
	resp, err := call(ctx, c, func(ctx context.Context) (*handlercmd.ListOutboundsResponse, error) {
		return c.handler.ListOutbounds(ctx, &handlercmd.ListOutboundsRequest{})
	})
	return resp.GetOutbounds(), err
}

func (c *client) AddOutbound(ctx context.Context, h *core.OutboundHandlerConfig) error {
	_, err := call(ctx, c, func(ctx context.Context) (*handlercmd.AddOutboundResponse, error) {
		return c.handler.AddOutbound(ctx, &handlercmd.AddOutboundRequest{Outbound: h})
	})
	return err
}

func (c *client) RemoveOutbound(ctx context.Context, tag string) error {
	_, err := call(ctx, c, func(ctx context.Context) (*handlercmd.RemoveOutboundResponse, error) {
		return c.handler.RemoveOutbound(ctx, &handlercmd.RemoveOutboundRequest{Tag: tag})
	})
	return err
}

func (c *client) ListInbounds(ctx context.Context) ([]*core.InboundHandlerConfig, error) {
	resp, err := call(ctx, c, func(ctx context.Context) (*handlercmd.ListInboundsResponse, error) {
		return c.handler.ListInbounds(ctx, &handlercmd.ListInboundsRequest{})
	})
	return resp.GetInbounds(), err
}

func (c *client) AddInbound(ctx context.Context, h *core.InboundHandlerConfig) error {
	_, err := call(ctx, c, func(ctx context.Context) (*handlercmd.AddInboundResponse, error) {
		return c.handler.AddInbound(ctx, &handlercmd.AddInboundRequest{Inbound: h})
	})
	return err
}

func (c *client) RemoveInbound(ctx context.Context, tag string) error {
	_, err := call(ctx, c, func(ctx context.Context) (*handlercmd.RemoveInboundResponse, error) {
		return c.handler.RemoveInbound(ctx, &handlercmd.RemoveInboundRequest{Tag: tag})
	})
	return err
}

func (c *client) ListRules(ctx context.Context) ([]Rule, error) {
	resp, err := call(ctx, c, func(ctx context.Context) (*routercmd.ListRuleResponse, error) {
		return c.routing.ListRule(ctx, &routercmd.ListRuleRequest{})
	})
	if err != nil {
		return nil, err
	}
	rules := make([]Rule, 0, len(resp.GetRules()))
	for _, item := range resp.GetRules() {
		rules = append(rules, Rule{Tag: item.GetTag(), RuleTag: item.GetRuleTag()})
	}
	return rules, nil
}

func (c *client) AddRule(ctx context.Context, r *router.RoutingRule) error {
	req := &routercmd.AddRuleRequest{
		Config:       serial.ToTypedMessage(&router.Config{Rule: []*router.RoutingRule{r}}),
		ShouldAppend: true,
	}
	_, err := call(ctx, c, func(ctx context.Context) (*routercmd.AddRuleResponse, error) {
		return c.routing.AddRule(ctx, req)
	})
	return err
}

func (c *client) RemoveRule(ctx context.Context, ruleTag string) error {
	_, err := call(ctx, c, func(ctx context.Context) (*routercmd.RemoveRuleResponse, error) {
		return c.routing.RemoveRule(ctx, &routercmd.RemoveRuleRequest{RuleTag: ruleTag})
	})
	return err
}

func (c *client) Close() error { return c.conn.Close() }
