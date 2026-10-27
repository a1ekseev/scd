//go:build xraylive

package livetest

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/a1ekseev/scd/internal/scd/xrayapi"
)

const (
	balancerTag   = "x-balancer"
	liveCandidate = "x-observe-live"
	observeWait   = 15 * time.Second
)

// TestLiveBalancerAndObservatory (D22, 10 §6.9 p.8): the read-only RPCs
// against live Xray 26.10.21 with the template's leastPing balancer and
// observatory: GetBalancerInfo knows the principle target, GetOutboundStatus
// answers, an unknown balancer is a *BalancerNotFoundError; a freedom
// candidate x-observe-live becomes the leastPing principle target once the
// observatory has probed it (probeURL is the local httptest probe).
func TestLiveBalancerAndObservatory(t *testing.T) {
	c, _, _ := startXray(t)
	ctx := t.Context()

	info, err := c.GetBalancerInfo(ctx, balancerTag)
	if err != nil {
		t.Fatalf("GetBalancerInfo(%s): %v", balancerTag, err)
	}
	if !info.PrincipleKnown || info.OverrideTarget != "" {
		t.Errorf("GetBalancerInfo(%s) without candidates = %+v, want PrincipleKnown and no override", balancerTag, info)
	}
	if _, err := c.GetOutboundStatus(ctx); err != nil {
		t.Fatalf("GetOutboundStatus: %v", err)
	}

	_, err = c.GetBalancerInfo(ctx, "nope")
	var nf *xrayapi.BalancerNotFoundError
	if !errors.As(err, &nf) || nf.Tag != "nope" {
		t.Errorf("GetBalancerInfo(nope) error = %v (%T), want *BalancerNotFoundError{nope}", err, err)
	}

	if err := c.AddOutbound(ctx, freedomOutbound(liveCandidate)); err != nil {
		t.Fatalf("AddOutbound %s: %v", liveCandidate, err)
	}
	deadline := time.Now().Add(observeWait)
	for {
		obs, oerr := c.GetOutboundStatus(ctx)
		info, err = c.GetBalancerInfo(ctx, balancerTag)
		if oerr == nil && err == nil && alive(obs, liveCandidate) &&
			info.PrincipleKnown && slices.Equal(info.PrincipleTargets, []string{liveCandidate}) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("after %s: GetOutboundStatus = %+v (%v), GetBalancerInfo = %+v (%v); want %s alive and principle",
				observeWait, obs, oerr, info, err, liveCandidate)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// alive reports whether obs has tag alive with a delay.
func alive(obs []xrayapi.OutboundObservation, tag string) bool {
	for _, o := range obs {
		if o.Tag == tag {
			return o.Alive && o.DelayMs != nil
		}
	}
	return false
}
