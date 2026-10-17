package xrayfake

import (
	"context"
	"math/rand/v2"

	"github.com/xtls/xray-core/app/proxyman"
	handlercmd "github.com/xtls/xray-core/app/proxyman/command"
	xnet "github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/serial"
	"github.com/xtls/xray-core/core"
	"github.com/xtls/xray-core/proxy/freedom"
	"github.com/xtls/xray-core/proxy/socks"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// Xray v26.10.10 error texts of the handler managers (08 §3).
const (
	errOutboundExists = "app/proxyman/outbound: existing tag found: "
	errInboundExists  = "app/proxyman/inbound: existing tag found: "
	errNoClue         = "common: not enough information for making a decision"
)

// xrayError is how Xray returns its errors over gRPC: codes.Unknown with the
// "<package>: <message>" text.
func xrayError(msg string) error { return status.Error(codes.Unknown, msg) }

func staticOutbound(tag string) *core.OutboundHandlerConfig {
	return &core.OutboundHandlerConfig{
		Tag:            tag,
		SenderSettings: serial.ToTypedMessage(&proxyman.SenderConfig{}),
		ProxySettings:  serial.ToTypedMessage(&freedom.Config{}),
	}
}

func staticInbound(tag string, port uint32) *core.InboundHandlerConfig {
	return &core.InboundHandlerConfig{
		Tag: tag,
		ReceiverSettings: serial.ToTypedMessage(&proxyman.ReceiverConfig{
			PortList: &xnet.PortList{Range: []*xnet.PortRange{xnet.SinglePortRange(xnet.Port(port))}},
			Listen:   xnet.NewIPOrDomain(xnet.AnyIP),
		}),
		ProxySettings: serial.ToTypedMessage(&socks.ServerConfig{}),
	}
}

// shuffled returns clones of the map values in random order: Xray iterates a
// Go map, so List* order is not deterministic (08 §3).
func shuffled[M proto.Message](m map[string]M) []M {
	out := make([]M, 0, len(m))
	for _, v := range m {
		out = append(out, proto.CloneOf(v))
	}
	rand.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
	return out
}

type handlerService struct {
	handlercmd.UnimplementedHandlerServiceServer
	s *Server
}

func (h *handlerService) ListOutbounds(ctx context.Context, _ *handlercmd.ListOutboundsRequest) (*handlercmd.ListOutboundsResponse, error) {
	if err := h.s.enter(ctx, "ListOutbounds", "ListOutbounds"); err != nil {
		return nil, err
	}
	h.s.mu.Lock()
	defer h.s.mu.Unlock()
	return &handlercmd.ListOutboundsResponse{Outbounds: shuffled(h.s.outbounds)}, nil
}

func (h *handlerService) AddOutbound(ctx context.Context, req *handlercmd.AddOutboundRequest) (*handlercmd.AddOutboundResponse, error) {
	cfg := req.GetOutbound()
	if err := h.s.enter(ctx, "AddOutbound", "AddOutbound:"+cfg.GetTag()); err != nil {
		return nil, err
	}
	if err := checkTypedMessages(cfg.ProtoReflect()); err != nil {
		return nil, xrayError(err.Error())
	}
	h.s.mu.Lock()
	defer h.s.mu.Unlock()
	if _, ok := h.s.outbounds[cfg.GetTag()]; ok {
		return nil, xrayError(errOutboundExists + cfg.GetTag())
	}
	h.s.outbounds[cfg.GetTag()] = proto.CloneOf(cfg)
	return &handlercmd.AddOutboundResponse{}, nil
}

func (h *handlerService) RemoveOutbound(ctx context.Context, req *handlercmd.RemoveOutboundRequest) (*handlercmd.RemoveOutboundResponse, error) {
	tag := req.GetTag()
	if err := h.s.enter(ctx, "RemoveOutbound", "RemoveOutbound:"+tag); err != nil {
		return nil, err
	}
	if tag == "" {
		return nil, xrayError(errNoClue)
	}
	h.s.mu.Lock()
	defer h.s.mu.Unlock()
	delete(h.s.outbounds, tag) // unknown tag: success, as in Xray
	return &handlercmd.RemoveOutboundResponse{}, nil
}

func (h *handlerService) ListInbounds(ctx context.Context, req *handlercmd.ListInboundsRequest) (*handlercmd.ListInboundsResponse, error) {
	if err := h.s.enter(ctx, "ListInbounds", "ListInbounds"); err != nil {
		return nil, err
	}
	h.s.mu.Lock()
	defer h.s.mu.Unlock()
	inbounds := shuffled(h.s.inbounds)
	if req.GetIsOnlyTags() {
		for i, in := range inbounds {
			inbounds[i] = &core.InboundHandlerConfig{Tag: in.GetTag()}
		}
	}
	return &handlercmd.ListInboundsResponse{Inbounds: inbounds}, nil
}

func (h *handlerService) AddInbound(ctx context.Context, req *handlercmd.AddInboundRequest) (*handlercmd.AddInboundResponse, error) {
	cfg := req.GetInbound()
	if err := h.s.enter(ctx, "AddInbound", "AddInbound:"+cfg.GetTag()); err != nil {
		return nil, err
	}
	if err := checkTypedMessages(cfg.ProtoReflect()); err != nil {
		return nil, xrayError(err.Error())
	}
	h.s.mu.Lock()
	defer h.s.mu.Unlock()
	if _, ok := h.s.inbounds[cfg.GetTag()]; ok {
		return nil, xrayError(errInboundExists + cfg.GetTag())
	}
	h.s.inbounds[cfg.GetTag()] = proto.CloneOf(cfg)
	return &handlercmd.AddInboundResponse{}, nil
}

func (h *handlerService) RemoveInbound(ctx context.Context, req *handlercmd.RemoveInboundRequest) (*handlercmd.RemoveInboundResponse, error) {
	tag := req.GetTag()
	if err := h.s.enter(ctx, "RemoveInbound", "RemoveInbound:"+tag); err != nil {
		return nil, err
	}
	h.s.mu.Lock()
	defer h.s.mu.Unlock()
	if _, ok := h.s.inbounds[tag]; !ok || tag == "" {
		return nil, xrayError(errNoClue) // unknown tag fails, unlike outbounds
	}
	delete(h.s.inbounds, tag)
	return &handlercmd.RemoveInboundResponse{}, nil
}
