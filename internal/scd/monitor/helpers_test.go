package monitor

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/a1ekseev/scd/internal/common/logging"
	"github.com/a1ekseev/scd/internal/scd/apply"
	"github.com/a1ekseev/scd/internal/scd/config"
	"github.com/a1ekseev/scd/internal/scd/manifest"
	"github.com/a1ekseev/scd/internal/scd/normalize"
	"github.com/a1ekseev/scd/internal/scd/outbound"
	"github.com/a1ekseev/scd/internal/scd/resources"
	"github.com/a1ekseev/scd/internal/scd/socks"
	"github.com/a1ekseev/scd/internal/scd/state"
	"github.com/a1ekseev/scd/internal/scd/topology"
	"github.com/a1ekseev/scd/internal/scd/xrayapi"
	"github.com/a1ekseev/scd/internal/scd/xrayapi/xrayfake"
)

const (
	linkAT = "vless://7d1b6590-1069-4372-92be-8d0a0ae6eaf5@example.com:443?type=tcp&security=tls#🇦🇹 Вена, Австрия, Extra"
	linkBE = "vless://7d1b6590-1069-4372-92be-8d0a0ae6eaf6@example.com:443?type=tcp&security=tls#🇧🇪 Брюссель, Бельгия, Extra"
	linkBG = "vless://7d1b6590-1069-4372-92be-8d0a0ae6eaf7@example.com:443?type=tcp&security=tls#🇧🇬 София, Болгария, Extra"

	prefix = "x-observe-"

	realShapeConfig = "../../../docker/scd/config.real-shape.go.yml"
)

// lockedBuffer is a goroutine-safe log sink.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// events returns the parsed JSON log lines with the given event.
func (l *lockedBuffer) events(t *testing.T, event string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for line := range strings.Lines(l.String()) {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("log line %q: %v", line, err)
		}
		if m["event"] == event {
			out = append(out, m)
		}
	}
	return out
}

// newSub is the TS createTarget: one target with a monitor request and the
// balancer monitor disabled.
func newSub(mut func(c *config.SubscriptionConfig)) config.SubscriptionConfig {
	c := config.SubscriptionConfig{
		ID:      "source-1",
		Enabled: true,
		Target: config.SubscriptionTargetConfig{
			Address:   "127.0.0.1:8080",
			TimeoutMs: 5000,
			InboundSocks: &config.InboundSocksConfig{
				Listen:    "127.0.0.1",
				PortRange: config.PortRange{Start: 20000, End: 20010},
			},
			Monitor: config.TargetMonitorConfig{
				Enabled:     true,
				Schedule:    "*/2 * * * *",
				MaxParallel: 10,
				Retry:       config.RetryConfig{Attempts: 1, DelayMs: 250},
				Request: &config.TargetMonitorRequestConfig{
					URL: "https://example.test/health", Method: "GET", ExpectedStatus: 200, TimeoutMs: 5000,
				},
			},
		},
	}
	if mut != nil {
		mut(&c)
	}
	return c
}

// withBalancer enables the balancer monitor via 127.0.0.10:1080 (TS tests).
func withBalancer(expected int, remoteURL string, viaSocks bool) func(c *config.SubscriptionConfig) {
	return func(c *config.SubscriptionConfig) {
		c.Target.BalancerMonitor = config.TargetBalancerMonitorConfig{
			Enabled:  true,
			Schedule: "*/2 * * * *",
			Socks5:   &config.BalancerMonitorSocks5Config{Host: "127.0.0.10", Port: 1080},
			Request: &config.TargetMonitorRequestConfig{
				URL: "https://example.test/health", Method: "GET", ExpectedStatus: expected, TimeoutMs: 5000,
			},
			Retry: config.RetryConfig{Attempts: 1, DelayMs: 250},
		}
		if remoteURL != "" {
			c.Target.BalancerMonitor.RemotePing = config.TargetBalancerRemotePingConfig{
				Enabled: true, URL: remoteURL, TimeoutMs: 5000, ViaSocks: viaSocks,
			}
		}
	}
}

// env is one target in memory with a fake Xray behind Deps.Dial.
type env struct {
	t    *testing.T
	mem  *state.Memory
	sub  config.SubscriptionConfig
	key  string
	logs *lockedBuffer
	xray *xrayfake.Server
	// client is a direct client of the fake for setting up its state.
	client xrayapi.Client
	dials  atomic.Int32
	// onDial runs at every Dial before the client is created.
	onDial func()
	// wrap, if set, wraps every dialed client.
	wrap func(xrayapi.Client) xrayapi.Client
	bg   sync.WaitGroup
	deps Deps
}

func newEnv(t *testing.T, sub config.SubscriptionConfig, links ...string) *env {
	t.Helper()
	logs := &lockedBuffer{}
	log, err := logging.New(logs, "debug", "json")
	if err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, mem: state.New(), sub: sub, key: state.Key(sub.ID, sub.Target.Address), logs: logs}
	e.xray, e.client = xrayfake.Start(t)
	e.mem.MarkSynced(e.key)
	if len(links) > 0 {
		m := manifest.Build(strings.Join(links, "\n"), "inline", normalize.Policy{}, time.Unix(0, 0))
		if len(m.Entries) != len(links) {
			t.Fatalf("entries = %d, skipped = %+v", len(m.Entries), m.Skipped)
		}
		topo, err := topology.Build(m, sub.Target)
		if err != nil {
			t.Fatal(err)
		}
		e.mem.ReplaceTopology(e.key, topo, map[apply.Kind]string{
			apply.KindOutbound: "hash-a", apply.KindInbound: "hash-b", apply.KindRouting: "hash-c",
		})
	}
	e.deps = Deps{
		Memory: e.mem,
		Log:    log,
		Dial: func(addr string, timeout time.Duration) (xrayapi.Client, error) {
			e.dials.Add(1)
			if e.onDial != nil {
				e.onDial()
			}
			c, err := e.xray.Dialer()(addr, timeout)
			if err != nil || e.wrap == nil {
				return c, err
			}
			return e.wrap(c), nil
		},
		Probe: func(context.Context, socks.Request) (socks.Response, error) {
			t.Error("unexpected probe")
			return socks.Response{}, context.Canceled
		},
		Direct: func(context.Context, string, time.Duration) (int, error) {
			t.Error("unexpected direct remote ping")
			return 0, context.Canceled
		},
		Sleep:      func(context.Context, time.Duration) error { return nil },
		Background: &e.bg,
	}
	return e
}

// tunnels returns copies of the tunnels in order.
func (e *env) tunnels() []state.Tunnel {
	var out []state.Tunnel
	e.mem.View(e.key, func(tg *state.Target) {
		for _, tn := range tg.Tunnels {
			out = append(out, *tn)
		}
	})
	return out
}

func (e *env) tunnel(i int) state.Tunnel { return e.tunnels()[i] }

func (e *env) setTunnel(i int, fn func(tn *state.Tunnel)) {
	e.mem.Update(e.key, func(tg *state.Target) { fn(tg.Tunnels[i]) })
}

func (e *env) balancer() state.BalancerMonitor {
	var b state.BalancerMonitor
	e.mem.View(e.key, func(tg *state.Target) { b = tg.Balancer })
	return b
}

// repairTunnel puts tunnel i on its unprefixed outbound in memory and in the
// fake Xray, as after a successful repair.
func (e *env) repairTunnel(i int) {
	e.t.Helper()
	e.setTunnel(i, func(tn *state.Tunnel) { tn.Mapping.OutboundTagCurrent = tn.Mapping.OutboundWithoutPrefix.Tag })
	e.install(e.tunnel(i).Mapping)
}

// install adds the tunnel's current outbound and route rule to the fake.
func (e *env) install(m topology.Tunnel) {
	e.t.Helper()
	ob := m.OutboundInitial.JSON
	if m.OutboundTagCurrent == m.OutboundWithoutPrefix.Tag {
		ob = m.OutboundWithoutPrefix.JSON
	}
	h, err := outbound.ToProto(ob)
	if err != nil {
		e.t.Fatal(err)
	}
	ctx := e.t.Context()
	if err := e.client.AddOutbound(ctx, h); err != nil {
		e.t.Fatal(err)
	}
	if err := e.client.AddRule(ctx, resources.Rule(m, m.OutboundTagCurrent)); err != nil {
		e.t.Fatal(err)
	}
}

// callsSince returns the fake's calls after the first n.
func (e *env) callsSince(n int) []string { return e.xray.Calls()[n:] }

func ok(code, latency int) func(context.Context, socks.Request) (socks.Response, error) {
	return func(context.Context, socks.Request) (socks.Response, error) {
		return socks.Response{StatusCode: code, LatencyMs: latency, BodyBytes: 100}, nil
	}
}

func deref(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}

// topoOf builds the topology of links for sub.
func topoOf(t *testing.T, sub config.SubscriptionConfig, links ...string) topology.Topology {
	t.Helper()
	m := manifest.Build(strings.Join(links, "\n"), "inline", normalize.Policy{}, time.Unix(0, 0))
	topo, err := topology.Build(m, sub.Target)
	if err != nil {
		t.Fatal(err)
	}
	return topo
}

func discardLog() *slog.Logger { return slog.New(slog.DiscardHandler) }
