package apply

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/xtls/xray-core/core"

	"github.com/a1ekseev/scd/internal/scd/config"
	"github.com/a1ekseev/scd/internal/scd/xrayapi"
)

// ErrCapReached is returned instead of an AddOutbound that would put more
// than maxActive outbounds of a source into Xray.
var ErrCapReached = errors.New("the source already has maxActive outbounds in Xray")

// CheckCap lists the outbounds of c and returns ErrCapReached when those
// that owns reports (fixed ones aside) are limit or more; a List error is
// returned as is. Either way the caller must not add.
func CheckCap(ctx context.Context, c xrayapi.OutboundAPI, fixed []string, owns Owner, limit int) error {
	list, err := c.ListOutbounds(ctx)
	if err != nil {
		return err
	}
	n := 0
	for _, h := range list {
		if !slices.Contains(fixed, h.GetTag()) && owns(KindOutbound, h.GetTag()) {
			n++
		}
	}
	if n >= limit {
		return fmt.Errorf("%w (%d of %d)", ErrCapReached, n, limit)
	}
	return nil
}

// capped is a client whose AddOutbound goes through CheckCap for the
// source whose namespace holds the tag.
type capped struct {
	xrayapi.Client
	fixed   []string
	sources []Namespace
	limits  []int
}

// Capped wraps c for target t so that every AddOutbound of a tag in the
// namespace of a source with maxActive (both tags count, with and without
// the observatory prefix) first checks CheckCap and is not sent when it
// fails. Real Xray state is checked, not memory, so a stale state, a lost
// rule or an add that timed out after it happened can never exceed
// maxActive. The caller holds the target lock. Without such a source c is
// returned as is.
func Capped(c xrayapi.Client, t config.SubscriptionTargetConfig, sources []config.SourceConfig) xrayapi.Client {
	w := &capped{Client: c, fixed: t.FixedOutbounds}
	for _, s := range sources {
		if s.MaxActive > 0 {
			w.sources = append(w.sources, Namespace{ObsPrefix: t.ObservatorySubjectSelectorPrefix, SourceID: s.ID})
			w.limits = append(w.limits, s.MaxActive)
		}
	}
	if len(w.sources) == 0 {
		return c
	}
	return w
}

func (c *capped) AddOutbound(ctx context.Context, h *core.OutboundHandlerConfig) error {
	for i, n := range c.sources {
		if n.Owns(KindOutbound, h.GetTag()) {
			if err := CheckCap(ctx, c.Client, c.fixed, n.Owns, c.limits[i]); err != nil {
				return fmt.Errorf("add outbound %s: %w", h.GetTag(), err)
			}
		}
	}
	return c.Client.AddOutbound(ctx, h)
}
