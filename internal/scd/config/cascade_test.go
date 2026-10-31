package config

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// cascadeBase is a cascade: vless (balancer x-balancer) carries warp
// (masque) through chain-blanc, warp carries proton (wireguard) through
// chain-warp.
const cascadeBase = `subscriptions:
  - id: s1
    sources:
      - id: vless
        type: vless
        input: ./sub.txt
        portRange: {start: 20000, end: 20009}
        balancer: x-balancer
      - id: warp
        type: masque
        input: /app/wgp/warp
        portRange: {start: 20100, end: 20101}
        maxActive: 1
        selection: {strategy: label, switchMarginPercent: 20, switchConfirmChecks: 2, deadPairCooldownMinutes: 10, replaceAfterFailures: 2}
        balancer: x-warp-balancer
        dialerProxy: chain-blanc
        dependsOn: [{source: vless, health: balancer}]
      - id: proton
        type: wireguard
        input: /app/wgp/proton
        portRange: {start: 20110, end: 20111}
        maxActive: 1
        selection: {strategy: score, switchMarginPercent: 20, switchConfirmChecks: 2, deadPairCooldownMinutes: 10, replaceAfterFailures: 2}
        wireguard: {endpointDomainStrategy: UseIPv4}
        balancer: x-proton-balancer
        dialerProxy: chain-warp
        dependsOn: [{source: warp, health: balancer, recoverGraceSeconds: 240}]
    target:
      address: 127.0.0.1:8080
      inboundSocks: {listen: 127.0.0.1}
      fixedOutbounds: [chain-blanc, chain-warp]
      visionUdp443Override: true
      balancers:
        - {tag: x-balancer, selection: {strategy: leastPing}}
runtime: {}
logging: {}
resources:
  inbounds: {enabled: true}
  routing: {enabled: true}
`

// cascade returns cascadeBase with each old fragment replaced by the next
// argument (once, in order); a missing fragment fails the test.
func cascade(t *testing.T, pairs ...string) string {
	t.Helper()
	s := cascadeBase
	for i := 0; i < len(pairs); i += 2 {
		if !strings.Contains(s, pairs[i]) {
			t.Fatalf("fragment %q not in the config", pairs[i])
		}
		s = strings.Replace(s, pairs[i], pairs[i+1], 1)
	}
	return s
}

func TestCascadeKeysLoad(t *testing.T) {
	l, warnings := mustLoad(t, cascadeBase)
	if len(warnings) != 0 {
		t.Fatalf("warnings %q", warnings)
	}
	src := l.Config.Subscriptions[0].Sources
	if src[0].Balancer != "x-balancer" || src[0].DialerProxy != "" || src[0].DependsOn != nil {
		t.Fatalf("vless %+v", src[0])
	}
	if src[1].Balancer != "x-warp-balancer" || src[1].DialerProxy != "chain-blanc" ||
		!reflect.DeepEqual(src[1].DependsOn, []DependencyConfig{{Source: "vless", Health: HealthBalancer, RecoverGraceSeconds: 180}}) {
		t.Fatalf("warp %+v", src[1])
	}
	if !reflect.DeepEqual(src[2].DependsOn, []DependencyConfig{{Source: "warp", Health: HealthBalancer, RecoverGraceSeconds: 240}}) {
		t.Fatalf("proton %+v", src[2].DependsOn)
	}
}

func TestCascadeRules(t *testing.T) {
	const p = "subscriptions.0.sources."
	cases := []struct {
		name  string
		pairs []string
		want  string
	}{
		{
			"dialerProxy on vless",
			[]string{"        balancer: x-balancer\n", "        balancer: x-balancer\n        dialerProxy: chain-blanc\n"},
			p + `0.dialerProxy: not allowed for type "vless"`,
		},
		{"dialerProxy not fixed", []string{"dialerProxy: chain-blanc", "dialerProxy: nope"}, p + "1.dialerProxy: must be in target.fixedOutbounds"},
		{"unknown source", []string{"{source: vless, health: balancer}", "{source: blanc, health: balancer}"}, p + `1.dependsOn.0.source: unknown source "blanc"`},
		{"itself", []string{"{source: vless, health: balancer}", "{source: warp, health: balancer}"}, p + "1.dependsOn.0.source: a source cannot depend on itself"},
		{
			"duplicate",
			[]string{"[{source: vless, health: balancer}]", "[{source: vless, health: balancer}, {source: vless, health: balancer}]"},
			p + `1.dependsOn.1.source: duplicate dependency "vless"`,
		},
		{"health required", []string{"{source: vless, health: balancer}", "{source: vless}"}, p + "1.dependsOn.0.health: required"},
		{"health value", []string{"{source: vless, health: balancer}", "{source: vless, health: monitor}"}, p + "1.dependsOn.0.health: must be one of balancer"},
		{"negative grace", []string{"recoverGraceSeconds: 240", "recoverGraceSeconds: -1"}, p + "2.dependsOn.0.recoverGraceSeconds: must be at least 0"},
		{"unknown key", []string{"{source: vless, health: balancer}", "{source: vless, health: balancer, mode: x}"}, p + `1.dependsOn.0: unknown key "mode"`},
		{
			"disabled dependency",
			[]string{"        balancer: x-balancer\n", "        balancer: x-balancer\n        enabled: false\n"},
			p + `1.dependsOn.0.source: source "vless" is disabled`,
		},
		{"dependency without balancer", []string{"        balancer: x-balancer\n", ""}, p + `1.dependsOn.0.health: source "vless" has no balancer`},
		{
			"balancer of a dependency",
			[]string{"balancer: x-proton-balancer", "balancer: x-balancer"},
			p + `2.balancer: balancer "x-balancer" is also the balancer of dependency "vless"`,
		},
		{
			"cycle",
			[]string{"        balancer: x-balancer\n", "        balancer: x-balancer\n        dependsOn: [{source: proton, health: balancer}]\n"},
			p + "0.dependsOn: dependsOn cycle: vless → proton → warp → vless",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if msg := mustFail(t, cascade(t, c.pairs...), c.want); strings.Count(msg, "\n  - ") != 1 {
				t.Errorf("want one issue:\n%s", msg)
			}
		})
	}
}

// A disabled source may depend on a disabled one; a cycle of two is
// reported once.
func TestCascadeDisabledAndShortCycle(t *testing.T) {
	mustLoad(t, cascade(t,
		"        balancer: x-balancer\n", "        balancer: x-balancer\n        enabled: false\n",
		"        balancer: x-warp-balancer\n", "        balancer: x-warp-balancer\n        enabled: false\n",
		"        balancer: x-proton-balancer\n", "        balancer: x-proton-balancer\n        enabled: false\n"))
	msg := mustFail(t, cascade(t, "{source: vless, health: balancer}", "{source: proton, health: balancer}"),
		"subscriptions.0.sources.1.dependsOn: dependsOn cycle: warp → proton → warp")
	if n := strings.Count(msg, "cycle"); n != 1 {
		t.Errorf("cycle reported %d times:\n%s", n, msg)
	}
}

func TestCascadeWarnings(t *testing.T) {
	_, warnings := mustLoad(t, cascade(t, "visionUdp443Override: true", "visionUdp443Override: false"))
	want := []string{"subscriptions.0.sources.1.dialerProxy: " + masqueVisionWarning}
	if !reflect.DeepEqual(warnings, want) {
		t.Errorf("vision warnings %q", warnings)
	}
	// A disabled masque source gets no warning (proton is disabled too: it
	// may not depend on a disabled warp).
	_, warnings = mustLoad(t, cascade(t, "visionUdp443Override: true", "visionUdp443Override: false",
		"        balancer: x-warp-balancer\n", "        balancer: x-warp-balancer\n        enabled: false\n",
		"        balancer: x-proton-balancer\n", "        balancer: x-proton-balancer\n        enabled: false\n"))
	if len(warnings) != 0 {
		t.Errorf("disabled source warnings %q", warnings)
	}
	for _, st := range []string{"random", "roundRobin"} {
		_, warnings = mustLoad(t, cascade(t, "strategy: leastPing", "strategy: "+st))
		want = []string{`subscriptions.0.sources.1.dependsOn.0.health: balancer "x-balancer" has strategy "` + st +
			`", which selects every candidate; the dependency is always healthy`}
		if !reflect.DeepEqual(warnings, want) {
			t.Errorf("%s warnings %q", st, warnings)
		}
	}
}

// PrintedSource mirrors SourceConfig: the same fields in the same order with
// the same JSON tags, except FetchHeaders (a plain map when printed).
func TestPrintedSourceMirrorsSourceConfig(t *testing.T) {
	src, printed := reflect.TypeFor[SourceConfig](), reflect.TypeFor[PrintedSource]()
	if src.NumField() != printed.NumField() {
		t.Fatalf("SourceConfig has %d fields, PrintedSource %d", src.NumField(), printed.NumField())
	}
	for i := range src.NumField() {
		a, b := src.Field(i), printed.Field(i)
		if a.Name != b.Name || a.Tag.Get("json") != b.Tag.Get("json") || (a.Name != "FetchHeaders" && a.Type != b.Type) {
			t.Errorf("field %d: SourceConfig %s %v %q, PrintedSource %s %v %q", i, a.Name, a.Type, a.Tag.Get("json"), b.Name, b.Type, b.Tag.Get("json"))
		}
	}
}

// Printed copies every field: with every field of SourceConfig set, its JSON
// has every key of SourceConfig.
func TestPrintedCopiesEveryField(t *testing.T) {
	var src SourceConfig
	fill(reflect.ValueOf(&src).Elem())
	var printed map[string]any
	b, err := json.Marshal(src.Printed(true))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &printed); err != nil {
		t.Fatal(err)
	}
	for f := range reflect.TypeFor[SourceConfig]().Fields() {
		key, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if _, ok := printed[key]; !ok {
			t.Errorf("Printed drops %s (%q)", f.Name, key)
		}
	}
}

// fill sets every settable value under v to a non-zero value.
func fill(v reflect.Value) {
	switch v.Kind() {
	case reflect.String:
		v.SetString("x")
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int64:
		v.SetInt(1)
	case reflect.Pointer:
		v.Set(reflect.New(v.Type().Elem()))
		fill(v.Elem())
	case reflect.Slice:
		v.Set(reflect.MakeSlice(v.Type(), 1, 1))
		fill(v.Index(0))
	case reflect.Map:
		v.Set(reflect.MakeMap(v.Type()))
		k, e := reflect.New(v.Type().Key()).Elem(), reflect.New(v.Type().Elem()).Elem()
		fill(k)
		fill(e)
		v.SetMapIndex(k, e)
	case reflect.Struct:
		for i := range v.NumField() {
			if v.Type().Field(i).IsExported() {
				fill(v.Field(i))
			}
		}
	}
}
