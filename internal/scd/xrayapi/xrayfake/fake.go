// Package xrayfake is an in-process fake Xray API server on bufconn with the
// observable semantics of Xray v26.10.21 (08 §3, 02 §6 level 2): the
// HandlerService outbound/inbound RPCs, the RoutingService rule and balancer
// RPCs and the ObservatoryService used by scd (D22, 10 §6.9), the exact Xray
// error texts (codes.Unknown), shuffled List*
// responses, GetInstance() on every TypedMessage of an Add, per-method
// failure injection and delays, and a log of calls. It is meant for tests of
// every package above xrayapi.
package xrayfake

import (
	"context"
	"maps"
	"net"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/xtls/xray-core/app/observatory"
	obscmd "github.com/xtls/xray-core/app/observatory/command"
	handlercmd "github.com/xtls/xray-core/app/proxyman/command"
	routercmd "github.com/xtls/xray-core/app/router/command"
	"github.com/xtls/xray-core/common/serial"
	"github.com/xtls/xray-core/core"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/a1ekseev/scd/internal/scd/xrayapi"
)

// address is what the fake reports as its target address (used by clients
// in error texts).
const address = "bufconn"

// defaultTimeout is the per-RPC timeout of the client returned by Start, the
// default target timeoutMs (5000).
const defaultTimeout = 5 * time.Second

// Server is the fake Xray. All state is guarded by mu.
type Server struct {
	mu        sync.Mutex
	outbounds map[string]*core.OutboundHandlerConfig
	inbounds  map[string]*core.InboundHandlerConfig
	rules     []xrayapi.Rule
	balancers map[string]*fakeBalancer
	calls     []string
	failNext  map[string]error
	delays    map[string]time.Duration

	observations []*observatory.OutboundStatus

	noRouting     bool
	noObservatory bool

	listener *bufconn.Listener
	grpc     *grpc.Server
}

// Option configures a Server before it starts.
type Option func(*Server)

// WithStaticOutbounds preloads freedom outbounds with the given tags, like
// the static "direct"/"blocked" of the test Xray config.
func WithStaticOutbounds(tags ...string) Option {
	return func(s *Server) {
		for _, tag := range tags {
			s.outbounds[tag] = staticOutbound(tag)
		}
	}
}

// WithStaticInbounds preloads SOCKS inbounds with the given tags.
func WithStaticInbounds(tags ...string) Option {
	return func(s *Server) {
		for i, tag := range tags {
			s.inbounds[tag] = staticInbound(tag, uint32(10800+i))
		}
	}
}

// WithStaticRules preloads router rules in the given order.
func WithStaticRules(rules ...xrayapi.Rule) Option {
	return func(s *Server) {
		s.rules = append(s.rules, rules...)
	}
}

// WithoutRoutingService leaves RoutingService unregistered: its RPCs fail
// with codes.Unimplemented "unknown service …", as on an Xray whose api.services
// lack it.
func WithoutRoutingService() Option {
	return func(s *Server) { s.noRouting = true }
}

// WithoutObservatoryService leaves ObservatoryService unregistered: its RPC
// fails with codes.Unimplemented "unknown service …", as on an Xray whose
// api.services lack it. It is registered by default.
func WithoutObservatoryService() Option {
	return func(s *Server) { s.noObservatory = true }
}

// Start runs a fake Xray on bufconn and returns it with a connected Client
// (timeout 5000ms). t.Cleanup closes the client and stops the server.
func Start(t testing.TB, opts ...Option) (*Server, xrayapi.Client) {
	t.Helper()
	s := &Server{
		outbounds: map[string]*core.OutboundHandlerConfig{},
		inbounds:  map[string]*core.InboundHandlerConfig{},
		balancers: map[string]*fakeBalancer{},
		failNext:  map[string]error{},
		delays:    map[string]time.Duration{},
		listener:  bufconn.Listen(1 << 20),
		grpc:      grpc.NewServer(),
	}
	for _, opt := range opts {
		opt(s)
	}
	handlercmd.RegisterHandlerServiceServer(s.grpc, &handlerService{s: s})
	if !s.noRouting {
		routercmd.RegisterRoutingServiceServer(s.grpc, &routingService{s: s})
	}
	if !s.noObservatory {
		obscmd.RegisterObservatoryServiceServer(s.grpc, &observatoryService{s: s})
	}
	go func() { _ = s.grpc.Serve(s.listener) }()
	t.Cleanup(s.grpc.Stop)

	c, err := s.Dialer()(address, defaultTimeout)
	if err != nil {
		t.Fatalf("xrayfake: dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return s, c
}

// Address returns the address string clients use in error texts.
func (s *Server) Address() string { return address }

// DialOptions returns the grpc.DialOptions that connect to this server over
// bufconn (for raw gRPC clients in tests; the target is "passthrough:///"+Address()).
func (s *Server) DialOptions() []grpc.DialOption {
	return []grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return s.listener.DialContext(ctx)
		}),
	}
}

// Dialer returns an xrayapi.Dialer that connects to this server whatever the
// address; the address and timeout still drive the client's error texts.
// The caller closes the clients it creates.
func (s *Server) Dialer() xrayapi.Dialer {
	return func(addr string, timeout time.Duration) (xrayapi.Client, error) {
		return xrayapi.DialWith(addr, timeout, s.DialOptions()...)
	}
}

// OutboundTags returns the sorted outbound tags.
func (s *Server) OutboundTags() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Sorted(maps.Keys(s.outbounds))
}

// InboundTags returns the sorted inbound tags.
func (s *Server) InboundTags() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Sorted(maps.Keys(s.inbounds))
}

// Rules returns a copy of the rules in router order (the order is part of
// the state: AddRule appends).
func (s *Server) Rules() []xrayapi.Rule {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.rules)
}

// Outbound returns a copy of the outbound with tag, or nil.
func (s *Server) Outbound(tag string) *core.OutboundHandlerConfig {
	s.mu.Lock()
	defer s.mu.Unlock()
	if h, ok := s.outbounds[tag]; ok {
		return proto.CloneOf(h)
	}
	return nil
}

// Inbound returns a copy of the inbound with tag, or nil.
func (s *Server) Inbound(tag string) *core.InboundHandlerConfig {
	s.mu.Lock()
	defer s.mu.Unlock()
	if h, ok := s.inbounds[tag]; ok {
		return proto.CloneOf(h)
	}
	return nil
}

// Calls returns the RPCs received so far, including failed ones, as
// "ListOutbounds", "AddOutbound:<tag>", "RemoveOutbound:<tag>",
// "ListInbounds", "AddInbound:<tag>", "RemoveInbound:<tag>", "ListRule",
// "AddRule:<ruleTag>-><outboundTag>" (one per rule; "AddRule" when the
// config cannot be decoded), "RemoveRule:<ruleTag>",
// "GetBalancerInfo:<balancerTag>",
// "OverrideBalancerTarget:<balancerTag>-><target>" and "GetOutboundStatus".
func (s *Server) Calls() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.calls)
}

// FailNext makes the next call of method (an RPC name such as "AddOutbound",
// "ListRule" or "GetBalancerInfo") fail with err, typically a status.Error, without touching
// the state. It applies once.
func (s *Server) FailNext(method string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failNext[method] = err
}

// Delay makes every later call of method wait d before it is served. If the
// caller's deadline expires first, the call fails with that context error
// and the state is not changed.
func (s *Server) Delay(method string, d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.delays[method] = d
}

// enter records a call, applies its delay and its injected failure. A nil
// result means the RPC proceeds.
func (s *Server) enter(ctx context.Context, method, call string) error {
	s.mu.Lock()
	s.calls = append(s.calls, call)
	d := s.delays[method]
	s.mu.Unlock()

	if d > 0 {
		timer := time.NewTimer(d)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return status.FromContextError(ctx.Err()).Err()
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if err, ok := s.failNext[method]; ok {
		delete(s.failNext, method)
		return err
	}
	return nil
}

// checkTypedMessages calls GetInstance() on every TypedMessage reachable
// from m (and inside every decoded instance), as Xray does when it builds a
// handler or a router config: an unknown or malformed type fails.
func checkTypedMessages(m protoreflect.Message) error {
	if tm, ok := m.Interface().(*serial.TypedMessage); ok {
		inst, err := tm.GetInstance()
		if err != nil {
			return err
		}
		return checkTypedMessages(inst.ProtoReflect())
	}
	var err error
	m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		switch {
		case fd.IsMap():
			if fd.MapValue().Message() != nil {
				v.Map().Range(func(_ protoreflect.MapKey, mv protoreflect.Value) bool {
					err = checkTypedMessages(mv.Message())
					return err == nil
				})
			}
		case fd.IsList():
			if fd.Message() != nil {
				for i := 0; i < v.List().Len() && err == nil; i++ {
					err = checkTypedMessages(v.List().Get(i).Message())
				}
			}
		case fd.Message() != nil:
			err = checkTypedMessages(v.Message())
		}
		return err == nil
	})
	return err
}
