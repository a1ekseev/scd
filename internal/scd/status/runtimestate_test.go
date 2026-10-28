package status

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/a1ekseev/scd/internal/common/inputsrc"
	"github.com/a1ekseev/scd/internal/common/subscription"
	"github.com/a1ekseev/scd/internal/scd/config"
	"github.com/a1ekseev/scd/internal/scd/normalize"
	"github.com/a1ekseev/scd/internal/scd/outbound"
	"github.com/a1ekseev/scd/internal/scd/resources"
	"github.com/a1ekseev/scd/internal/scd/state"
	"github.com/a1ekseev/scd/internal/scd/topology"
	"github.com/a1ekseev/scd/internal/scd/xrayapi"
	"github.com/a1ekseev/scd/internal/scd/xrayapi/xrayfake"

	vlesslink "github.com/a1ekseev/scd/internal/common/vless"
)

const realShapesDir = "../../../docker/fixtures/real-shapes/"

func startFake(t *testing.T, opts ...xrayfake.Option) (*xrayfake.Server, xrayapi.Client) {
	t.Helper()
	return xrayfake.Start(t, opts...)
}

// rsCase is one entry of runtime-state/cases.json.
type rsCase struct {
	Name            string          `json:"name"`
	Line            string          `json:"line"`
	Input           string          `json:"input"`
	Target          json.RawMessage `json:"target"`
	Options         struct{ IncludeRaw, IncludeSecrets bool }
	BalancerMonitor *fixtureBalancer `json:"balancerMonitor"`
	Runtime         struct {
		Outbounds []struct {
			FixedTag string `json:"fixedTag"`
			Tunnel   *int   `json:"tunnel"`
			Variant  string `json:"variant"`
		} `json:"outbounds"`
		Inbounds []struct {
			Tunnel int `json:"tunnel"`
		} `json:"inbounds"`
		Rules []struct {
			Tunnel int `json:"tunnel"`
		} `json:"rules"`
	} `json:"runtime"`
}

// rsEnv is a runtime-state scenario: config, memory and a fake Xray.
type rsEnv struct {
	deps  Deps
	fake  *xrayfake.Server
	topo  topology.Topology
	input string
}

func newRSEnv(t *testing.T, sub config.SubscriptionConfig, text string, opts config.RuntimeStateConfig, fakeOpts ...xrayfake.Option) *rsEnv {
	t.Helper()
	env := &rsEnv{}
	if text != "" {
		env.input = filepath.Join(t.TempDir(), "subscription.txt")
		if err := os.WriteFile(env.input, []byte(text+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		sub.Input = env.input
		env.topo = buildTopology(t, sub, text)
	}
	env.fake, _ = startFake(t, fakeOpts...)
	opts.Enabled = true
	cfg := config.AppConfig{
		Subscriptions: []config.SubscriptionConfig{sub},
		Resources: config.ResourceConfig{
			Outbounds: config.ResourceToggle{Enabled: true}, Inbounds: config.ResourceToggle{Enabled: true}, Routing: config.ResourceToggle{Enabled: true},
		},
		StatusServer: config.StatusServerConfig{Enabled: true, InstanceID: "local-main", RuntimeState: opts},
	}
	env.deps = Deps{
		Config: config.Loaded{Path: "/cfg/config.yml", Config: cfg}, Memory: state.New(),
		Dial: env.fake.Dialer(), Now: func() time.Time { return t0 },
		Hostname: os.Hostname, Log: discardLogger(),
	}
	return env
}

func newRSSub(input string) config.SubscriptionConfig {
	return config.SubscriptionConfig{ID: "source-1", Input: input, Enabled: true, Format: "auto", FetchTimeoutMs: 5000, AllowInsecure: "deny", Target: testTarget()}
}

func (e *rsEnv) addOutbound(t *testing.T, tn topology.Tunnel, withoutPrefix bool) {
	t.Helper()
	o := tn.OutboundInitial
	if withoutPrefix {
		o = tn.OutboundWithoutPrefix
	}
	h, err := outbound.ToProto(o.JSON)
	if err != nil {
		t.Fatal(err)
	}
	e.client(t, func(c xrayapi.Client) error { return c.AddOutbound(context.Background(), h) })
}

func (e *rsEnv) client(t *testing.T, fn func(c xrayapi.Client) error) {
	t.Helper()
	c, err := e.fake.Dialer()("bufconn", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if err := fn(c); err != nil {
		t.Fatal(err)
	}
}

// build runs BuildRuntimeState for source-1/127.0.0.1:8080 and returns the
// served JSON.
func (e *rsEnv) build(t *testing.T) (map[string]any, string) {
	t.Helper()
	v, code, err := BuildRuntimeState(context.Background(), e.deps, "source-1", "127.0.0.1:8080")
	if err != nil || code != http.StatusOK {
		t.Fatalf("BuildRuntimeState: %d %v", code, err)
	}
	b, err := marshalJSON(v)
	if err != nil {
		t.Fatal(err)
	}
	return decode(t, b).(map[string]any), string(b)
}

func loadCases(t *testing.T) []rsCase {
	t.Helper()
	data, err := os.ReadFile(refStatusDir + "runtime-state/cases.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct{ Cases []rsCase }
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	return doc.Cases
}

// applyTargetOverrides applies the cases.json target overrides.
func applyTargetOverrides(t *testing.T, tgt *config.SubscriptionTargetConfig, raw json.RawMessage) {
	t.Helper()
	var o struct {
		VisionUDP443Override *bool                               `json:"visionUdp443Override"`
		Monitor              *config.TargetMonitorConfig         `json:"monitor"`
		BalancerMonitor      *config.TargetBalancerMonitorConfig `json:"balancerMonitor"`
	}
	if err := json.Unmarshal(raw, &o); err != nil {
		t.Fatal(err)
	}
	if o.VisionUDP443Override != nil {
		tgt.VisionUDP443Override = *o.VisionUDP443Override
	}
	if o.Monitor != nil {
		o.Monitor.MaxParallel, o.Monitor.Retry = tgt.Monitor.MaxParallel, tgt.Monitor.Retry
		tgt.Monitor = *o.Monitor
	}
	if o.BalancerMonitor != nil {
		o.BalancerMonitor.Retry = tgt.BalancerMonitor.Retry
		tgt.BalancerMonitor = *o.BalancerMonitor
	}
}

// ignoredParsedKeys are not compared with the TS references: the TS
// decoder read wrong ws/fingerprint fields (D2/D6), path/host are redacted
// now (D33) and ListRule carries no inbound tags (02 §1.5).
var ignoredParsedKeys = []string{"fingerprint", "path", "host", "inboundTags"}

// comparableRuntime drops informational parts of runtime items: ignored
// parsed keys and the rawBase64 value (only its presence is compared).
func comparableRuntime(v any) any {
	m := v.(map[string]any)
	out := map[string]any{}
	for kind, items := range m {
		var list []any
		for _, it := range items.([]any) {
			item := map[string]any{}
			for k, val := range it.(map[string]any) {
				switch k {
				case "parsed":
					p := map[string]any{}
					maps.Copy(p, val.(map[string]any))
					for _, ik := range ignoredParsedKeys {
						delete(p, ik)
					}
					item[k] = p
				case "rawBase64":
					item[k] = val != ""
				default:
					item[k] = val
				}
			}
			list = append(list, item)
		}
		out[kind] = list
	}
	return out
}

// assertKeysSubset fails when a key of want (recursively, objects only) is
// missing from got: the Go schema may only add keys (D3, D31).
func assertKeysSubset(t *testing.T, path string, got, want any) {
	t.Helper()
	wm, ok := want.(map[string]any)
	if !ok {
		return
	}
	gm, ok := got.(map[string]any)
	if !ok {
		t.Errorf("%s: not an object in Go: %v", path, got)
		return
	}
	for k, wv := range wm {
		gv, ok := gm[k]
		if !ok {
			t.Errorf("%s.%s missing", path, k)
			continue
		}
		assertKeysSubset(t, path+"."+k, gv, wv)
	}
}

func dig(v any, keys ...string) any {
	for _, k := range keys {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = m[k]
	}
	return v
}

// 03 §10.5 items 1–6 against the TS references (keys, classification,
// diff, redaction).
func TestRuntimeStateMatchesTSReference(t *testing.T) {
	for _, c := range loadCases(t) {
		t.Run(c.Name, func(t *testing.T) {
			sub := newRSSub(c.Input)
			applyTargetOverrides(t, &sub.Target, c.Target)
			var fakeOpts []xrayfake.Option
			for _, o := range c.Runtime.Outbounds {
				if o.FixedTag != "" {
					fakeOpts = append(fakeOpts, xrayfake.WithStaticOutbounds(o.FixedTag))
				}
			}
			env := newRSEnv(t, sub, c.Line, config.RuntimeStateConfig{IncludeRaw: c.Options.IncludeRaw, IncludeSecrets: c.Options.IncludeSecrets}, fakeOpts...)
			for _, o := range c.Runtime.Outbounds {
				if o.Tunnel != nil {
					env.addOutbound(t, env.topo.Tunnels[*o.Tunnel], o.Variant == "withoutPrefix")
				}
			}
			for _, in := range c.Runtime.Inbounds {
				h, err := resources.Inbound(env.topo.Tunnels[in.Tunnel])
				if err != nil {
					t.Fatal(err)
				}
				env.client(t, func(cl xrayapi.Client) error { return cl.AddInbound(context.Background(), h) })
			}
			for _, r := range c.Runtime.Rules {
				tn := env.topo.Tunnels[r.Tunnel]
				env.client(t, func(cl xrayapi.Client) error {
					return cl.AddRule(context.Background(), resources.Rule(tn, tn.OutboundTagInitial))
				})
			}
			if c.BalancerMonitor != nil {
				key := state.Key("source-1", "127.0.0.1:8080")
				env.deps.Memory.MarkSynced(key)
				env.deps.Memory.Update(key, func(st *state.Target) { st.Balancer = state.BalancerMonitor(*c.BalancerMonitor) })
			}

			got, body := env.build(t)
			refData, err := os.ReadFile(refStatusDir + "runtime-state/" + c.Name + ".json")
			if err != nil {
				t.Fatal(err)
			}
			want := decode(t, []byte(strings.ReplaceAll(string(refData), "<input>", env.input))).(map[string]any)

			for _, k := range []string{"subscriptionId", "targetAddress", "capturedAt", "serviceState", "expected", "diff"} {
				if !jsonEqual(t, got[k], want[k]) {
					t.Errorf("%s differs", k)
				}
			}
			jsonEqual(t, comparableRuntime(got["runtime"]), comparableRuntime(want["runtime"]))
			assertKeysSubset(t, "config", got["config"], want["config"])
			jsonEqual(t, dig(got, "config", "resources"), dig(want, "config", "resources"))
			for _, p := range [][]string{
				{"config", "subscription", "input"},
				{"config", "target", "monitor", "request", "url"},
				{"config", "target", "balancerMonitor", "request", "url"},
				{"config", "target", "balancerMonitor", "remotePing", "url"},
				{"config", "target", "balancerMonitor", "remotePing", "viaSocks"},
				{"config", "target", "balancerMonitor", "socks5"},
			} {
				if !reflect.DeepEqual(dig(got, p...), dig(want, p...)) {
					t.Errorf("%v: got %v want %v", p, dig(got, p...), dig(want, p...))
				}
			}
			// Go also redacts the copy of the target inside the subscription
			// (TS printed it unredacted).
			if !reflect.DeepEqual(dig(got, "config", "subscription", "target"), dig(got, "config", "target")) {
				t.Errorf("subscription.target differs from target")
			}
			if !c.Options.IncludeSecrets {
				for _, s := range []string{"secret", testUUID} {
					if strings.Contains(body, s) {
						t.Errorf("%q leaked: %s", s, body)
					}
				}
			}
		})
	}
}

// D33: fetchHeaders values are secrets.
func TestRuntimeStateRedactsFetchHeaders(t *testing.T) {
	for _, include := range []bool{false, true} {
		sub := newRSSub("https://subscription.example.test/sub")
		sub.FetchHeaders = inputsrc.Headers{"x-hwid": "s3cr3t"}
		env := newRSEnv(t, sub, "", config.RuntimeStateConfig{IncludeSecrets: include})
		_, body := env.build(t)
		compact := strings.Join(strings.Fields(body), "")
		if include {
			if !strings.Contains(compact, `"x-hwid":"s3cr3t"`) {
				t.Errorf("includeSecrets: %s", body)
			}
			continue
		}
		if !strings.Contains(compact, `"x-hwid":"<redacted>"`) || strings.Contains(body, "s3cr3t") {
			t.Errorf("default: %s", body)
		}
	}
	// The config itself is not changed.
	sub := newRSSub("https://subscription.example.test/sub")
	sub.FetchHeaders = inputsrc.Headers{"x-hwid": "s3cr3t"}
	env := newRSEnv(t, sub, "", config.RuntimeStateConfig{})
	_, _ = env.build(t)
	if env.deps.Config.Config.Subscriptions[0].FetchHeaders["x-hwid"] != "s3cr3t" {
		t.Fatal("config mutated")
	}
}

// D33: the xhttp path of a real-shape line does not reach the response.
func TestRuntimeStateRedactsXHTTPPath(t *testing.T) {
	data, err := os.ReadFile(realShapesDir + "remnawave.txt")
	if err != nil {
		t.Fatal(err)
	}
	content, _ := inputsrc.DecodeAuto(string(data))
	l, skip := vlesslink.Parse(subscription.ScanLines(content)[0])
	if skip != nil {
		t.Fatalf("skip %+v", *skip)
	}
	n, _, skip := normalize.Normalize(l, normalize.Policy{})
	if skip != nil {
		t.Fatalf("skip %+v", *skip)
	}
	if n.Network != "xhttp" || n.Path == "" {
		t.Fatalf("fixture line 1 is not xhttp with a path")
	}
	h, err := outbound.ToProto(outbound.Build(n))
	if err != nil {
		t.Fatal(err)
	}

	for _, include := range []bool{false, true} {
		env := newRSEnv(t, newRSSub("https://subscription.example.test/sub"), "", config.RuntimeStateConfig{IncludeSecrets: include})
		env.client(t, func(c xrayapi.Client) error { return c.AddOutbound(context.Background(), h) })
		got, body := env.build(t)
		parsed := dig(got, "runtime", "outbounds").([]any)[0].(map[string]any)["parsed"].(map[string]any)
		if include {
			if parsed["path"] != n.Path {
				t.Errorf("includeSecrets: path not shown")
			}
			continue
		}
		if parsed["path"] != "<redacted>" || strings.Contains(body, n.Path) {
			t.Errorf("path leaked or not redacted: %v", parsed["path"])
		}
	}
}

// D15: expected uses the subscription's allowInsecure policy as sync does.
func TestRuntimeStateExpectedUsesAllowInsecurePolicy(t *testing.T) {
	line := "vless://" + testUUID + "@vpn.example.test:443?type=ws&security=tls&sni=cdn.example.test&path=%2Fws&allowInsecure=1#🇦🇹 Вена"
	for _, tc := range []struct {
		policy   string
		tunnels  int
		hasError bool
	}{{"allow", 1, false}, {"deny", 0, true}} {
		t.Run(tc.policy, func(t *testing.T) {
			sub := newRSSub("")
			sub.AllowInsecure = tc.policy
			path := filepath.Join(t.TempDir(), "sub.txt")
			if err := os.WriteFile(path, []byte(line+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			sub.Input = path
			env := newRSEnv(t, sub, "", config.RuntimeStateConfig{})
			got, _ := env.build(t)
			outs := dig(got, "expected", "outbounds").([]any)
			_, hasErr := dig(got, "expected").(map[string]any)["error"]
			if len(outs) != tc.tunnels || hasErr != tc.hasError {
				t.Fatalf("expected %v", got["expected"])
			}
		})
	}
}

func TestRuntimeStateExpectedErrors(t *testing.T) {
	t.Run("missing file", func(t *testing.T) {
		sub := newRSSub(filepath.Join(t.TempDir(), "absent.txt"))
		env := newRSEnv(t, sub, "", config.RuntimeStateConfig{})
		got, _ := env.build(t)
		exp := got["expected"].(map[string]any)
		if exp["error"] == nil || exp["source"] != sub.Input || len(exp["outbounds"].([]any)) != 0 {
			t.Fatalf("%v", exp)
		}
	})
	t.Run("stdin", func(t *testing.T) {
		env := newRSEnv(t, newRSSub("-"), "", config.RuntimeStateConfig{})
		got, _ := env.build(t)
		exp := got["expected"].(map[string]any)
		if exp["source"] != "-" || exp["error"] != "Expected topology is unavailable for remote or stdin inputs; runtime-state does not refetch subscription sources." {
			t.Fatalf("%v", exp)
		}
	})
	t.Run("resources disabled", func(t *testing.T) {
		env := newRSEnv(t, newRSSub(""), linkAT, config.RuntimeStateConfig{})
		env.deps.Config.Config.Resources.Inbounds.Enabled = false
		env.deps.Config.Config.Resources.Routing.Enabled = false
		got, _ := env.build(t)
		exp := got["expected"].(map[string]any)
		if len(exp["outbounds"].([]any)) != 1 || len(exp["inbounds"].([]any)) != 0 || len(exp["routingRules"].([]any)) != 0 {
			t.Fatalf("%v", exp)
		}
	})
}

// Unmanaged items and missing expected items end up in the diff, sorted.
func TestRuntimeStateDiffUnexpectedAndMissing(t *testing.T) {
	env := newRSEnv(t, newRSSub(""), linkAT, config.RuntimeStateConfig{}, xrayfake.WithStaticOutbounds("zz-extra", "direct", "aa-extra"))
	got, _ := env.build(t)
	d := got["diff"].(map[string]any)["outbounds"].(map[string]any)
	if !reflect.DeepEqual(d["unexpected"], []any{"aa-extra", "zz-extra"}) || !reflect.DeepEqual(d["missing"], []any{env.topo.Tunnels[0].BaseTunnelID}) {
		t.Fatalf("%v", d)
	}
	var tags []string
	for _, it := range dig(got, "runtime", "outbounds").([]any) {
		tags = append(tags, it.(map[string]any)["tag"].(string))
	}
	if strings.Join(tags, ",") != "aa-extra,direct,zz-extra" {
		t.Fatalf("%v", tags)
	}
}

// rawBase64 holds unredacted secrets: it is written only with both
// includeRaw and includeSecrets.
func TestRuntimeStateRawBase64(t *testing.T) {
	for _, c := range []struct{ raw, secrets bool }{{false, false}, {true, false}, {false, true}, {true, true}} {
		t.Run(fmt.Sprintf("raw=%v,secrets=%v", c.raw, c.secrets), func(t *testing.T) {
			env := newRSEnv(t, newRSSub(""), linkAT, config.RuntimeStateConfig{IncludeRaw: c.raw, IncludeSecrets: c.secrets}, xrayfake.WithStaticInbounds("api-in"))
			env.addOutbound(t, env.topo.Tunnels[0], false)
			tn := env.topo.Tunnels[0]
			env.client(t, func(c xrayapi.Client) error {
				return c.AddRule(context.Background(), resources.Rule(tn, tn.OutboundTagInitial))
			})
			got, _ := env.build(t)
			want := c.raw && c.secrets
			for _, kind := range []string{"outbounds", "inbounds", "routingRules"} {
				items := dig(got, "runtime", kind).([]any)
				if len(items) == 0 {
					t.Fatalf("%s: no items", kind)
				}
				for _, it := range items {
					s, ok := it.(map[string]any)["rawBase64"].(string)
					if ok != want || (want && s == "") {
						t.Errorf("%s: rawBase64 present=%v, want %v: %v", kind, ok, want, it)
					}
				}
			}
		})
	}
}

func TestRedactURLGoRules(t *testing.T) {
	for in, want := range map[string]string{
		"https://h:8443/x?t=1":              "https://h:8443/<redacted>",
		"https://user:pw@h.example.test/a":  "https://h.example.test/<redacted>",
		"http://h.example.test:80/":         "http://h.example.test:80/<redacted>",
		"socks5://127.0.0.1:1080/path":      "socks5://127.0.0.1:1080/<redacted>",
		"not a url":                         "<redacted>",
		"mailto:x@example.test":             "<redacted>",
		"https://[::1]:8443/x":              "https://[::1]:8443/<redacted>",
		"https://h.example.test/%zz?broken": "<redacted>",
	} {
		if got := inputsrc.RedactURL(in); got != want {
			t.Errorf("%q: got %q want %q", in, got, want)
		}
	}
}
