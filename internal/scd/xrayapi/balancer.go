package xrayapi

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/xtls/xray-core/app/observatory"
	obscmd "github.com/xtls/xray-core/app/observatory/command"
	routercmd "github.com/xtls/xray-core/app/router/command"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// BalancerAPI is the read-only RoutingService balancer part (D22, 10 §6.1).
// scd never calls OverrideBalancerTarget (10 §6.8).
type BalancerAPI interface {
	// GetBalancerInfo returns the override and the principle target of the
	// balancer with tag. An unknown tag fails with *BalancerNotFoundError, a
	// disabled RoutingService with *ServiceDisabledError.
	GetBalancerInfo(ctx context.Context, balancerTag string) (BalancerInfo, error)
}

// ObservatoryAPI is the read-only ObservatoryService part (D22, 10 §6.1).
type ObservatoryAPI interface {
	// GetOutboundStatus returns the observatory statuses sorted by Tag. A
	// disabled ObservatoryService fails with *ServiceDisabledError.
	GetOutboundStatus(ctx context.Context) ([]OutboundObservation, error)
}

// BalancerInfo is the normalized GetBalancerInfo answer (10 §2.2).
type BalancerInfo struct {
	// OverrideTarget is the override target, "" when there is none.
	OverrideTarget string
	// PrincipleTargets are the principle target tags without empty strings
	// (leastPing's [""] becomes []); nil when PrincipleKnown is false.
	PrincipleTargets []string
	// PrincipleKnown is false when principle_target was absent (Xray only
	// logged a GetPrincipleTarget error).
	PrincipleKnown bool
}

// OutboundObservation is one normalized observatory status (10 §3.1).
type OutboundObservation struct {
	// Tag is the outbound tag.
	Tag string
	// Alive is the result of the last probe.
	Alive bool
	// DelayMs is the probe delay in ms; nil when the outbound is not alive or
	// reports the "dead" delay 99999999.
	DelayMs *int64
	// LastError is last_error_reason as is (human-readable).
	LastError string
	// LastSeenAt is the last successful probe time; nil when never.
	LastSeenAt *time.Time
	// LastTryAt is the last probe time; nil when never.
	LastTryAt *time.Time
	// HealthPing is set by burstObservatory only.
	HealthPing *HealthPing
}

// HealthPing is the burstObservatory measurement (nanoseconds on the wire).
type HealthPing struct {
	// All and Fail count the probes and the failed ones.
	All, Fail int64
	// Average, Deviation, Min and Max are the RTT statistics.
	Average, Deviation, Min, Max time.Duration
}

// ServiceDisabledError reports an Xray API service missing from
// api.services (gRPC Unimplemented "unknown service …").
type ServiceDisabledError struct {
	// Service is the short service name, e.g. "RoutingService".
	Service string
}

// Error implements error.
func (e *ServiceDisabledError) Error() string {
	return "Xray API service " + e.Service + " is not enabled (add it to api.services)."
}

// BalancerNotFoundError reports a balancer tag unknown to Xray
// ("app/router: cannot find tag").
type BalancerNotFoundError struct {
	// Tag is the requested balancer tag.
	Tag string
}

// Error implements error.
func (e *BalancerNotFoundError) Error() string {
	return `Xray balancer "` + e.Tag + `" not found.`
}

// deadDelay is the delay the regular observatory reports for a dead outbound.
const deadDelay = 99999999

func (c *client) GetBalancerInfo(ctx context.Context, balancerTag string) (BalancerInfo, error) {
	resp, err := readCall(ctx, c, func(ctx context.Context) (*routercmd.GetBalancerInfoResponse, error) {
		return c.routing.GetBalancerInfo(ctx, &routercmd.GetBalancerInfoRequest{Tag: balancerTag})
	}, func() error { return &BalancerNotFoundError{Tag: balancerTag} })
	if err != nil {
		return BalancerInfo{}, err
	}
	b := resp.GetBalancer()
	info := BalancerInfo{OverrideTarget: b.GetOverride().GetTarget()}
	if pt := b.GetPrincipleTarget(); pt != nil {
		info.PrincipleKnown = true
		info.PrincipleTargets = slices.DeleteFunc(slices.Clone(pt.GetTag()), func(s string) bool { return s == "" })
		if info.PrincipleTargets == nil {
			info.PrincipleTargets = []string{}
		}
	}
	return info, nil
}

func (c *client) GetOutboundStatus(ctx context.Context) ([]OutboundObservation, error) {
	resp, err := readCall(ctx, c, func(ctx context.Context) (*obscmd.GetOutboundStatusResponse, error) {
		return c.observatory.GetOutboundStatus(ctx, &obscmd.GetOutboundStatusRequest{})
	}, nil)
	if err != nil {
		return nil, err
	}
	out := make([]OutboundObservation, 0, len(resp.GetStatus().GetStatus()))
	for _, st := range resp.GetStatus().GetStatus() {
		out = append(out, decodeObservation(st))
	}
	slices.SortStableFunc(out, func(a, b OutboundObservation) int { return strings.Compare(a.Tag, b.Tag) })
	return out, nil
}

func decodeObservation(st *observatory.OutboundStatus) OutboundObservation {
	o := OutboundObservation{
		Tag:        st.GetOutboundTag(),
		Alive:      st.GetAlive(),
		LastError:  st.GetLastErrorReason(),
		LastSeenAt: unixTime(st.GetLastSeenTime()),
		LastTryAt:  unixTime(st.GetLastTryTime()),
	}
	if o.Alive && st.GetDelay() != deadDelay {
		d := st.GetDelay()
		o.DelayMs = &d
	}
	if hp := st.GetHealthPing(); hp != nil {
		o.HealthPing = &HealthPing{
			All:       hp.GetAll(),
			Fail:      hp.GetFail(),
			Average:   time.Duration(hp.GetAverage()),
			Deviation: time.Duration(hp.GetDeviation()),
			Min:       time.Duration(hp.GetMin()),
			Max:       time.Duration(hp.GetMax()),
		}
	}
	return o
}

// unixTime converts Unix seconds to a time; 0 means "never" (nil).
func unixTime(sec int64) *time.Time {
	if sec == 0 {
		return nil
	}
	t := time.Unix(sec, 0)
	return &t
}

// readCall is call for the read-only D22 RPCs (10 §6.1): on top of the
// generic mapping it returns *ServiceDisabledError for an unregistered service
// (codes.Unimplemented "unknown service <pkg>.<Service>") and, when notFound
// is set, its error for codes.Unknown "cannot find tag".
func readCall[Resp any](ctx context.Context, c *client, rpc func(context.Context) (Resp, error), notFound func() error) (Resp, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	resp, err := rpc(ctx)
	if err == nil {
		return resp, nil
	}
	var zero Resp
	st := status.Convert(err)
	if st.Code() == codes.Unimplemented {
		if svc, ok := strings.CutPrefix(st.Message(), "unknown service "); ok {
			return zero, &ServiceDisabledError{Service: svc[strings.LastIndexByte(svc, '.')+1:]}
		}
	}
	if notFound != nil && st.Code() == codes.Unknown && strings.Contains(st.Message(), "cannot find tag") {
		return zero, notFound()
	}
	return zero, mapError(err, c.address, c.timeout)
}
