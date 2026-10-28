package apply

import (
	"context"
	"slices"
	"strings"

	"github.com/a1ekseev/scd/internal/scd/config"
	"github.com/a1ekseev/scd/internal/scd/xrayapi"
)

// Namespace is the tag namespace of one source on its target: outbounds
// "<ObsPrefix><SourceID>-" (in the balancer) and "<SourceID>-" (after a
// repair), inbounds "in-<SourceID>-" and routing rules "route-<SourceID>-".
type Namespace struct {
	ObsPrefix string
	SourceID  string
}

// Owns reports whether the resource of kind with tag lies in n. An empty
// tag (a rule without ruleTag) is never owned.
func (n Namespace) Owns(kind Kind, tag string) bool {
	p := n.SourceID + "-"
	switch kind {
	case KindOutbound:
		return strings.HasPrefix(tag, n.ObsPrefix+p) || strings.HasPrefix(tag, p)
	case KindInbound:
		return strings.HasPrefix(tag, "in-"+p)
	case KindRouting:
		return strings.HasPrefix(tag, "route-"+p)
	}
	return false
}

// Orphan is a resource RemoveOrphans removed.
type Orphan struct {
	Kind Kind   `json:"kind"`
	Tag  string `json:"tag"`
}

// RemoveOrphans removes, for the enabled kinds in the order routing →
// inbound → outbound, every listed resource of target t that is not fixed
// and lies in none of namespaces: tags of removed or disabled sources and
// tags from before sources existed. Rules without ruleTag are never
// touched (RemoveRule("") is not called). Each kind is listed right before
// its removals; the first error (List or Remove) stops the removal and is
// returned with what was removed so far.
func RemoveOrphans(ctx context.Context, c xrayapi.Client, t config.SubscriptionTargetConfig, kinds []Kind, namespaces []Namespace) ([]Orphan, error) {
	orphan := func(kind Kind, fixed []string, tag string) bool {
		if tag == "" || slices.Contains(fixed, tag) {
			return false
		}
		for _, n := range namespaces {
			if n.Owns(kind, tag) {
				return false
			}
		}
		return true
	}
	var removed []Orphan
	for _, kind := range []Kind{KindRouting, KindInbound, KindOutbound} {
		if !slices.Contains(kinds, kind) {
			continue
		}
		var (
			tags   []string
			fixed  []string
			remove func(context.Context, string) error
		)
		switch kind {
		case KindRouting:
			list, err := c.ListRules(ctx)
			if err != nil {
				return removed, err
			}
			for _, r := range list {
				tags = append(tags, r.RuleTag)
			}
			fixed, remove = t.FixedRouting, c.RemoveRule
		case KindInbound:
			list, err := c.ListInbounds(ctx)
			if err != nil {
				return removed, err
			}
			for _, h := range list {
				tags = append(tags, h.GetTag())
			}
			fixed, remove = t.FixedInbounds, c.RemoveInbound
		case KindOutbound:
			list, err := c.ListOutbounds(ctx)
			if err != nil {
				return removed, err
			}
			for _, h := range list {
				tags = append(tags, h.GetTag())
			}
			fixed, remove = t.FixedOutbounds, c.RemoveOutbound
		}
		for _, tag := range tags {
			if !orphan(kind, fixed, tag) {
				continue
			}
			if err := remove(ctx, tag); err != nil {
				return removed, err
			}
			removed = append(removed, Orphan{Kind: kind, Tag: tag})
		}
	}
	return removed, nil
}
