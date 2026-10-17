package xrayfake

import (
	"cmp"
	"context"
	"slices"
	"strings"

	"github.com/xtls/xray-core/app/observatory"
	routercmd "github.com/xtls/xray-core/app/router/command"
)

// errCannotFindTag is the router error for an unknown balancer tag (10 §2.2).
const errCannotFindTag = errRouterPrefix + "cannot find tag"

// leastPingMax is LeastPingStrategy's initial bound: a delay must be below it.
const leastPingMax = 99999999

// Balancing strategies, lowercased as BalancingRule.Build compares them.
const (
	strategyLeastPing  = "leastping"
	strategyLeastLoad  = "leastload"
	strategyRandom     = "random"
	strategyRoundRobin = "roundrobin"
)

// fakeBalancer is one routing balancer: its selectors (tag prefixes), the
// lowercased strategy and the override (never validated, as in Xray).
type fakeBalancer struct {
	selectors []string
	strategy  string
	override  string
}

// WithBalancer adds a balancer over the outbounds whose tags start with one
// of selectors. strategy is leastPing, leastLoad (expected 0), random or
// roundRobin, case-insensitive; "" means random as in Xray. Any other value
// panics: Xray refuses such a config ("unrecognized balancer type").
func WithBalancer(tag string, selectors []string, strategy string) Option {
	s := strings.ToLower(strategy)
	switch s {
	case strategyLeastPing, strategyLeastLoad, strategyRandom, strategyRoundRobin:
	case "":
		s = strategyRandom
	default:
		panic("xrayfake: WithBalancer " + tag + ": unrecognized balancer type " + strategy)
	}
	return func(srv *Server) {
		srv.balancers[tag] = &fakeBalancer{selectors: slices.Clone(selectors), strategy: s}
	}
}

// SetOverride sets the override target of balancer like
// OverrideBalancerTarget ("" resets it). It panics on an unknown balancer.
func (s *Server) SetOverride(balancer, target string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.balancers[balancer]
	if !ok {
		panic("xrayfake: SetOverride: unknown balancer " + balancer)
	}
	b.override = target
}

// candidates mirrors outbound.Manager.Select: every outbound whose tag starts
// with one of the selectors, sort.Strings order. Callers hold s.mu.
func (s *Server) candidates(selectors []string) []string {
	tags := []string{}
	for tag := range s.outbounds {
		if slices.ContainsFunc(selectors, func(p string) bool { return strings.HasPrefix(tag, p) }) {
			tags = append(tags, tag)
		}
	}
	slices.Sort(tags)
	return tags
}

// principleTarget mirrors the strategies' GetPrincipleTarget (10 §2.4) over
// the fake observatory. Callers hold s.mu.
func (s *Server) principleTarget(b *fakeBalancer) []string {
	cands := s.candidates(b.selectors)
	alive := func(st *observatory.OutboundStatus) bool {
		return st.GetAlive() && slices.Contains(cands, st.GetOutboundTag())
	}
	switch b.strategy {
	case strategyLeastPing:
		// Strict "<" in observatory order: the first of equal delays wins.
		best, bestDelay := "", int64(leastPingMax)
		for _, st := range s.observations {
			if alive(st) && st.GetDelay() < bestDelay {
				best, bestDelay = st.GetOutboundTag(), st.GetDelay()
			}
		}
		return []string{best}
	case strategyLeastLoad:
		// expected 0 without baselines: the single best node by delay
		// (RTTDeviationCost = RTTAverage = Delay), tie-break by tag.
		var nodes []*observatory.OutboundStatus
		for _, st := range s.observations {
			if alive(st) {
				nodes = append(nodes, st)
			}
		}
		if len(nodes) == 0 {
			return []string{}
		}
		best := slices.MinFunc(nodes, func(a, b *observatory.OutboundStatus) int {
			return cmp.Or(cmp.Compare(a.GetDelay(), b.GetDelay()), strings.Compare(a.GetOutboundTag(), b.GetOutboundTag()))
		})
		return []string{best.GetOutboundTag()}
	default: // random, roundRobin: the whole candidate set
		return cands
	}
}

func (r *routingService) GetBalancerInfo(ctx context.Context, req *routercmd.GetBalancerInfoRequest) (*routercmd.GetBalancerInfoResponse, error) {
	tag := req.GetTag()
	if err := r.s.enter(ctx, "GetBalancerInfo", "GetBalancerInfo:"+tag); err != nil {
		return nil, err
	}
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	b, ok := r.s.balancers[tag]
	if !ok {
		return nil, xrayError(errCannotFindTag)
	}
	return &routercmd.GetBalancerInfoResponse{Balancer: &routercmd.BalancerMsg{
		Override:        &routercmd.OverrideInfo{Target: b.override},
		PrincipleTarget: &routercmd.PrincipleTargetInfo{Tag: r.s.principleTarget(b)},
	}}, nil
}

// OverrideBalancerTarget stores the target without validation, as Xray does.
func (r *routingService) OverrideBalancerTarget(ctx context.Context, req *routercmd.OverrideBalancerTargetRequest) (*routercmd.OverrideBalancerTargetResponse, error) {
	tag := req.GetBalancerTag()
	if err := r.s.enter(ctx, "OverrideBalancerTarget", "OverrideBalancerTarget:"+tag+"->"+req.GetTarget()); err != nil {
		return nil, err
	}
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	b, ok := r.s.balancers[tag]
	if !ok {
		return nil, xrayError(errCannotFindTag)
	}
	b.override = req.GetTarget()
	return &routercmd.OverrideBalancerTargetResponse{}, nil
}
