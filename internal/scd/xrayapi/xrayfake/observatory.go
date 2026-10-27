package xrayfake

import (
	"context"
	"math/rand/v2"

	"github.com/xtls/xray-core/app/observatory"
	obscmd "github.com/xtls/xray-core/app/observatory/command"
	"google.golang.org/protobuf/proto"
)

// SetObservation replaces the observatory statuses (copies are kept). They
// are served as given — dead outbounds with delay 99999999, burst results
// with health_ping in ns, stale entries of removed tags — and drive the
// leastPing/leastLoad principle targets in that order.
func (s *Server) SetObservation(obs ...*observatory.OutboundStatus) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.observations = make([]*observatory.OutboundStatus, 0, len(obs))
	for _, o := range obs {
		s.observations = append(s.observations, proto.CloneOf(o))
	}
}

type observatoryService struct {
	obscmd.UnimplementedObservatoryServiceServer
	s *Server
}

// GetOutboundStatus answers copies of the statuses in shuffled order: Xray's
// order depends on probe completion (or a Go map for burst), 10 §3.1.
func (o *observatoryService) GetOutboundStatus(ctx context.Context, _ *obscmd.GetOutboundStatusRequest) (*obscmd.GetOutboundStatusResponse, error) {
	if err := o.s.enter(ctx, "GetOutboundStatus", "GetOutboundStatus"); err != nil {
		return nil, err
	}
	o.s.mu.Lock()
	defer o.s.mu.Unlock()
	out := make([]*observatory.OutboundStatus, 0, len(o.s.observations))
	for _, st := range o.s.observations {
		out = append(out, proto.CloneOf(st))
	}
	rand.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
	return &obscmd.GetOutboundStatusResponse{Status: &observatory.ObservationResult{Status: out}}, nil
}
