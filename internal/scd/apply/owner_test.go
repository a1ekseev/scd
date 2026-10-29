package apply_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/a1ekseev/scd/internal/scd/apply"
	"github.com/a1ekseev/scd/internal/scd/outbound"
	"github.com/a1ekseev/scd/internal/scd/topology"
	"github.com/a1ekseev/scd/internal/scd/xrayapi"
	"github.com/a1ekseev/scd/internal/scd/xrayapi/xrayfake"
)

// notFixed owns every tag not in fixed, as Apply does.
func notFixed(fixed []string) apply.Owner {
	return func(_ apply.Kind, tag string) bool { return !slices.Contains(fixed, tag) }
}

// prefixOwner owns the tags of kind with prefix and nothing else.
func prefixOwner(kind apply.Kind, prefix string) apply.Owner {
	return func(k apply.Kind, tag string) bool { return k == kind && strings.HasPrefix(tag, prefix) }
}

func TestApplyKeepsForeignNotOwned(t *testing.T) {
	ctx := context.Background()
	fixed := []string{"keep"}
	tests := []struct {
		name     string
		opts     []xrayfake.Option
		run      func(xrayapi.Client) (apply.Report, error)
		tags     func(*xrayfake.Server) []string
		wantTags []string
	}{
		{
			name: "outbound",
			opts: []xrayfake.Option{xrayfake.WithStaticOutbounds("keep", "other", "s-old")},
			run: func(c xrayapi.Client) (apply.Report, error) {
				return apply.Outbounds(ctx, c, fixed, prefixOwner(apply.KindOutbound, "s-"), []outbound.Spec{entry("s-a", "198.51.100.1")}, meta())
			},
			tags:     (*xrayfake.Server).OutboundTags,
			wantTags: []string{"keep", "other", "s-a"},
		},
		{
			name: "inbound",
			opts: []xrayfake.Option{xrayfake.WithStaticInbounds("keep", "other", "in-s-old")},
			run: func(c xrayapi.Client) (apply.Report, error) {
				return apply.Inbounds(ctx, c, fixed, prefixOwner(apply.KindInbound, "in-s-"), []topology.Tunnel{tunnel("s-a", 20000)}, meta())
			},
			tags:     (*xrayfake.Server).InboundTags,
			wantTags: []string{"in-s-a", "keep", "other"},
		},
		{
			name: "routing",
			opts: []xrayfake.Option{xrayfake.WithStaticRules(
				xrayapi.Rule{Tag: "direct", RuleTag: "keep"},
				xrayapi.Rule{Tag: "direct", RuleTag: "other"},
				xrayapi.Rule{Tag: "x-observe-s-old", RuleTag: "route-s-old"},
			)},
			run: func(c xrayapi.Client) (apply.Report, error) {
				return apply.Routing(ctx, c, fixed, prefixOwner(apply.KindRouting, "route-s-"), []topology.Tunnel{tunnel("s-a", 20000)}, meta())
			},
			tags: func(s *xrayfake.Server) []string {
				var out []string
				for _, r := range s.Rules() {
					out = append(out, r.RuleTag)
				}
				return out
			},
			wantTags: []string{"keep", "other", "route-s-a"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, c := xrayfake.Start(t, tt.opts...)
			r, err := tt.run(c)
			if err != nil {
				t.Fatal(err)
			}
			assertCounts(t, r, 1, 0, 1, 0)
			if len(r.DeletedIDs) != 1 || !strings.HasSuffix(r.DeletedIDs[0], "s-old") {
				t.Errorf("deletedIds = %v, want only the owned old tag", r.DeletedIDs)
			}
			if got := tt.tags(srv); !slices.Equal(got, tt.wantTags) {
				t.Errorf("tags = %v, want %v", got, tt.wantTags)
			}
			for _, call := range srv.Calls() {
				if strings.HasSuffix(call, ":other") || strings.HasSuffix(call, ":keep") {
					t.Errorf("RPC touched a not owned resource: %s", call)
				}
			}
		})
	}
}
