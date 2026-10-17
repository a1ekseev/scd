//go:build xraylive

package livetest

import (
	"context"
	"strings"
	"testing"

	"github.com/xtls/xray-core/app/proxyman"
	"github.com/xtls/xray-core/common/serial"
	"github.com/xtls/xray-core/core"
	"github.com/xtls/xray-core/proxy/freedom"

	"github.com/a1ekseev/scd/internal/scd/resources"
	"github.com/a1ekseev/scd/internal/scd/topology"
	"github.com/a1ekseev/scd/internal/scd/xrayapi"
	"github.com/a1ekseev/scd/internal/scd/xrayapi/xrayfake"
)

// freedomOutbound is a plain freedom outbound with tag.
func freedomOutbound(tag string) *core.OutboundHandlerConfig {
	return &core.OutboundHandlerConfig{
		Tag:            tag,
		SenderSettings: serial.ToTypedMessage(&proxyman.SenderConfig{}),
		ProxySettings:  serial.ToTypedMessage(&freedom.Config{}),
	}
}

// paritySteps runs one fixed scenario of successes and failures and returns
// "<step>: ok" or "<step>: <error text>" for every step, then the tags and
// rules left. port is a free port for the scenario's SOCKS inbound.
func paritySteps(t *testing.T, c xrayapi.Client, port int) []string {
	t.Helper()
	ctx := t.Context()
	tun := topology.Tunnel{InboundTag: "in-parity", RouteTag: "route-parity", Listen: "127.0.0.1", Port: port}
	in, err := resources.Inbound(tun)
	if err != nil {
		t.Fatal(err)
	}
	dupFixedIn, err := resources.Inbound(topology.Tunnel{InboundTag: "socks-proxy", Listen: "127.0.0.1", Port: port})
	if err != nil {
		t.Fatal(err)
	}
	fixedRuleTun := topology.Tunnel{InboundTag: "in-parity", RouteTag: "x-balancer"}

	steps := []struct {
		name string
		call func(context.Context) error
	}{
		{"AddOutbound parity-out", func(ctx context.Context) error { return c.AddOutbound(ctx, freedomOutbound("parity-out")) }},
		{"AddOutbound parity-out again", func(ctx context.Context) error { return c.AddOutbound(ctx, freedomOutbound("parity-out")) }},
		{"AddOutbound fixed direct", func(ctx context.Context) error { return c.AddOutbound(ctx, freedomOutbound("direct")) }},
		{"AddInbound in-parity", func(ctx context.Context) error { return c.AddInbound(ctx, in) }},
		{"AddInbound in-parity again", func(ctx context.Context) error { return c.AddInbound(ctx, in) }},
		{"AddInbound fixed socks-proxy", func(ctx context.Context) error { return c.AddInbound(ctx, dupFixedIn) }},
		{"AddRule route-parity", func(ctx context.Context) error { return c.AddRule(ctx, resources.Rule(tun, "parity-out")) }},
		{"AddRule route-parity again", func(ctx context.Context) error { return c.AddRule(ctx, resources.Rule(tun, "parity-out")) }},
		{"AddRule fixed x-balancer", func(ctx context.Context) error { return c.AddRule(ctx, resources.Rule(fixedRuleTun, "parity-out")) }},
		{`RemoveRule ""`, func(ctx context.Context) error { return c.RemoveRule(ctx, "") }},
		{"RemoveRule unknown", func(ctx context.Context) error { return c.RemoveRule(ctx, "no-such-rule") }},
		{"RemoveInbound unknown", func(ctx context.Context) error { return c.RemoveInbound(ctx, "no-such-inbound") }},
		{`RemoveInbound ""`, func(ctx context.Context) error { return c.RemoveInbound(ctx, "") }},
		{"RemoveOutbound unknown", func(ctx context.Context) error { return c.RemoveOutbound(ctx, "no-such-outbound") }},
		{`RemoveOutbound ""`, func(ctx context.Context) error { return c.RemoveOutbound(ctx, "") }},
		{"GetBalancerInfo unknown", func(ctx context.Context) error {
			_, err := c.GetBalancerInfo(ctx, "no-such-balancer")
			return err
		}},
		{"RemoveRule route-parity", func(ctx context.Context) error { return c.RemoveRule(ctx, "route-parity") }},
		{"RemoveInbound in-parity", func(ctx context.Context) error { return c.RemoveInbound(ctx, "in-parity") }},
		{"RemoveOutbound parity-out", func(ctx context.Context) error { return c.RemoveOutbound(ctx, "parity-out") }},
	}
	var out []string
	for _, s := range steps {
		res := "ok"
		if err := s.call(ctx); err != nil {
			res = err.Error()
		}
		out = append(out, s.name+": "+res)
	}
	st := readState(t, c)
	out = append(out, "outbounds: "+strings.Join(st.outbounds, ","), "inbounds: "+strings.Join(st.inbounds, ","))
	for _, r := range st.rules {
		out = append(out, "rule: "+r.Tag+"/"+r.RuleTag)
	}
	return out
}

// TestLiveFakeParity (D2, 02 §2.7, 08 §3): the same scenario — duplicate
// outbound/inbound/ruleTag, RemoveRule(""), removing unknown resources, an
// unknown balancer — gives the same results and error texts on live Xray
// 26.10.10 and on xrayfake started with the template's fixed resources.
func TestLiveFakeParity(t *testing.T) {
	c, _, _ := startXray(t)
	port := freePort(t)
	live := paritySteps(t, c, port)

	_, fc := xrayfake.Start(t,
		xrayfake.WithStaticOutbounds(fixedOutbounds...),
		xrayfake.WithStaticInbounds(fixedInbounds...),
		xrayfake.WithStaticRules(fixedRule),
		xrayfake.WithBalancer("x-balancer", []string{"x-observe-"}, "leastPing"),
	)
	fake := paritySteps(t, fc, port)

	for i := range max(len(live), len(fake)) {
		var l, f string
		if i < len(live) {
			l = live[i]
		}
		if i < len(fake) {
			f = fake[i]
		}
		if l != f {
			t.Errorf("step %d:\nlive %q\nfake %q", i+1, l, f)
		}
	}
	if !t.Failed() {
		t.Logf("live and fake agree:\n%s", strings.Join(live, "\n"))
	}
}
