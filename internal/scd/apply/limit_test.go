package apply_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/xtls/xray-core/core"

	"github.com/a1ekseev/scd/internal/scd/apply"
	"github.com/a1ekseev/scd/internal/scd/outbound"
	"github.com/a1ekseev/scd/internal/scd/xrayapi"
	"github.com/a1ekseev/scd/internal/scd/xrayapi/xrayfake"
)

// flaky passes calls to a real (fake Xray) client, except that it fails
// RemoveOutbound of the tags in failRemove without sending it, and fails
// AddOutbound of the tags in failAdd: without sending it, or after sending
// it when addHappens (a deadline after Xray created the outbound).
type flaky struct {
	xrayapi.OutboundAPI
	failRemove, failAdd map[string]bool
	addHappens          bool
}

func (f flaky) RemoveOutbound(ctx context.Context, tag string) error {
	if f.failRemove[tag] {
		return errors.New("remove failed")
	}
	return f.OutboundAPI.RemoveOutbound(ctx, tag)
}

func (f flaky) AddOutbound(ctx context.Context, h *core.OutboundHandlerConfig) error {
	if !f.failAdd[h.GetTag()] {
		return f.OutboundAPI.AddOutbound(ctx, h)
	}
	if f.addHappens {
		_ = f.OutboundAPI.AddOutbound(ctx, h)
	}
	return context.DeadlineExceeded
}

// peak replays calls from the outbounds have and returns the most outbounds
// with prefix that existed at once.
func peak(have []string, calls []string, prefix string) int {
	set := map[string]bool{}
	for _, t := range have {
		set[t] = true
	}
	n := len(set)
	for _, c := range calls {
		method, tag, _ := strings.Cut(c, ":")
		if !strings.HasPrefix(tag, prefix) {
			continue
		}
		switch method {
		case "AddOutbound":
			set[tag] = true
		case "RemoveOutbound":
			delete(set, tag)
		}
		n = max(n, len(set))
	}
	return n
}

// A rollback never restores more outbounds than MaxOutbounds, whatever
// failed: old {a, b}, new {c, d}.
func TestOutboundRollbackKeepsTheCap(t *testing.T) {
	for _, tc := range []struct {
		name string
		f    flaky
	}{
		{"add fails, removing the added one fails", flaky{failAdd: map[string]bool{"s-d": true}, failRemove: map[string]bool{"s-c": true}}},
		{"add times out after it happened", flaky{failAdd: map[string]bool{"s-d": true}, addHappens: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, c := xrayfake.Start(t)
			ctx := context.Background()
			for _, tag := range []string{"s-a", "s-b"} {
				if err := c.AddOutbound(ctx, mustProto(t, entry(tag, "198.51.100.1"))); err != nil {
					t.Fatal(err)
				}
			}
			n := len(srv.Calls())
			tc.f.OutboundAPI = c
			m := meta()
			m.MaxOutbounds = 2
			r, err := apply.Outbounds(ctx, tc.f, nil, prefixOwner(apply.KindOutbound, "s-"),
				[]outbound.Spec{entry("s-c", "198.51.100.2"), entry("s-d", "198.51.100.3")}, m)
			if err != nil || r.Failed != 1 {
				t.Fatalf("report %+v, %v", r, err)
			}
			if p := peak([]string{"s-a", "s-b"}, srv.Calls()[n:], "s-"); p > 2 {
				t.Fatalf("peak %d > 2: %v", p, srv.Calls()[n:])
			}
			if got := srv.OutboundTags(); len(got) != 2 {
				t.Fatalf("outbounds %v", got)
			}
		})
	}
}

// Without a cap (VLESS) every backup comes back, as before.
func TestOutboundRollbackWithoutCap(t *testing.T) {
	srv, c := xrayfake.Start(t)
	ctx := context.Background()
	for _, tag := range []string{"s-a", "s-b"} {
		if err := c.AddOutbound(ctx, mustProto(t, entry(tag, "198.51.100.1"))); err != nil {
			t.Fatal(err)
		}
	}
	f := flaky{OutboundAPI: c, failAdd: map[string]bool{"s-d": true}, failRemove: map[string]bool{"s-c": true}}
	if _, err := apply.Outbounds(ctx, f, nil, prefixOwner(apply.KindOutbound, "s-"),
		[]outbound.Spec{entry("s-c", "198.51.100.2"), entry("s-d", "198.51.100.3")}, meta()); err != nil {
		t.Fatal(err)
	}
	if got := srv.OutboundTags(); len(got) != 3 {
		t.Fatalf("outbounds %v", got)
	}
}
