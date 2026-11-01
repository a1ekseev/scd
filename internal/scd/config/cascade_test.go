package config

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
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

func TestCascadeBalancerDependencyLoad(t *testing.T) {
	l, warnings := mustLoad(t, cascade(t, "{source: vless, health: balancer}", "{balancer: pool-en, health: balancer}"))
	if len(warnings) != 0 {
		t.Fatalf("warnings %q", warnings)
	}
	d := l.Config.Subscriptions[0].Sources[1].DependsOn
	if !reflect.DeepEqual(d, []DependencyConfig{{Balancer: "pool-en", Health: HealthBalancer, RecoverGraceSeconds: 180}}) || d[0].Name() != "balancer:pool-en" {
		t.Fatalf("dependsOn %+v", d)
	}
	// Both kinds in one list are valid.
	mustLoad(t, cascade(t, "[{source: vless, health: balancer}]", "[{source: vless, health: balancer}, {balancer: pool-en, health: balancer}]"))
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
		{"neither key", []string{"{source: vless, health: balancer}", "{health: balancer}"}, p + "1.dependsOn.0: one of source or balancer is required"},
		{"both keys", []string{"{source: vless, health: balancer}", "{source: vless, balancer: pool-en, health: balancer}"}, p + "1.dependsOn.0: source and balancer are mutually exclusive"},
		{"empty balancer", []string{"{source: vless, health: balancer}", `{balancer: "", health: balancer}`}, p + "1.dependsOn.0.balancer: must not be empty"},
		{"balancer health required", []string{"{source: vless, health: balancer}", "{balancer: pool-en}"}, p + "1.dependsOn.0.health: required"},
		{"own balancer", []string{"{source: vless, health: balancer}", "{balancer: x-warp-balancer, health: balancer}"}, p + "1.dependsOn.0.balancer: a source cannot depend on its own balancer"},
		{"balancer of another source", []string{"{source: vless, health: balancer}", "{balancer: x-balancer, health: balancer}"}, p + `1.dependsOn.0.balancer: balancer "x-balancer" is the balancer of source "vless": use dependsOn source`},
		{
			"balancer of a disabled source",
			[]string{"        balancer: x-balancer\n", "        balancer: x-balancer\n        enabled: false\n", "{source: vless, health: balancer}", "{balancer: x-balancer, health: balancer}"},
			p + `1.dependsOn.0.balancer: balancer "x-balancer" is the balancer of source "vless": use dependsOn source`,
		},
		{
			"duplicate balancer",
			[]string{"[{source: vless, health: balancer}]", "[{balancer: pool-en, health: balancer}, {balancer: pool-en, health: balancer}]"},
			p + `1.dependsOn.1.balancer: duplicate dependency "balancer:pool-en"`,
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
	// A balancer dependency is no edge: no cycle, and it is not in balancers[].
	mustLoad(t, cascade(t, "[{source: vless, health: balancer}]", "[{balancer: pool-en, health: balancer}]",
		"        balancer: x-balancer\n", "        balancer: x-balancer\n        enabled: false\n"))
	for _, st := range []string{"random", "roundRobin"} {
		_, warnings = mustLoad(t, cascade(t, "[{source: vless, health: balancer}]", "[{balancer: pool-en, health: balancer}]",
			"- {tag: x-balancer, selection: {strategy: leastPing}}", "- {tag: pool-en, selection: {strategy: "+st+"}}"))
		want = []string{`subscriptions.0.sources.1.dependsOn.0.health: balancer "pool-en" has strategy "` + st +
			`", which selects every candidate; the dependency is always healthy`}
		if !reflect.DeepEqual(warnings, want) {
			t.Errorf("%s balancer dependency warnings %q", st, warnings)
		}
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

// crossBase has two subscriptions (two Xray): warp of blanc-ru depends on
// the balancer pool-blanc of blanc-en, the balancer of its source blanc.
const crossBase = `subscriptions:
  - id: blanc-en
    sources:
      - id: blanc
        type: vless
        input: ./en.txt
        portRange: {start: 20000, end: 20009}
        balancer: pool-blanc
    target:
      address: 127.0.0.1:8080
      inboundSocks: {listen: 127.0.0.1}
      observatorySubjectSelectorPrefix: obs-
      balancers:
        - {tag: pool-blanc, selection: {enabled: true, schedule: '*/15 * * * * *'}}
  - id: blanc-ru
    sources:
      - id: warp
        type: vless
        input: ./ru.txt
        portRange: {start: 20100, end: 20109}
        balancer: pool-vpn
        dependsOn: [{subscription: blanc-en, balancer: pool-blanc, health: balancer}]
      - id: proton
        type: vless
        input: ./ru2.txt
        portRange: {start: 20110, end: 20119}
        balancer: pool-p
    target:
      address: 127.0.0.1:8081
      inboundSocks: {listen: 127.0.0.1}
      observatorySubjectSelectorPrefix: obs-
      balancers:
        - {tag: pool-vpn, selection: {enabled: true, schedule: '*/15 * * * * *'}}
        - {tag: pool-p, selection: {enabled: true, schedule: '*/15 * * * * *'}}
runtime: {}
logging: {}
resources:
  inbounds: {enabled: true}
  routing: {enabled: true}
`

// cross is cascade for crossBase.
func cross(t *testing.T, pairs ...string) string {
	t.Helper()
	s := crossBase
	for i := 0; i < len(pairs); i += 2 {
		if !strings.Contains(s, pairs[i]) {
			t.Fatalf("fragment %q not in the config", pairs[i])
		}
		s = strings.Replace(s, pairs[i], pairs[i+1], 1)
	}
	return s
}

const (
	crossDep   = "{subscription: blanc-en, balancer: pool-blanc, health: balancer}"
	enBalancer = "{tag: pool-blanc, selection: {enabled: true, schedule: '*/15 * * * * *'}}"
	thirdSub   = "  - id: blanc-en\n" + plainSub + "8082}\nruntime: {}"
	plainSub   = "    sources: [{id: x, type: vless, input: ./x.txt, portRange: {start: 20000, end: 20009}}]\n" +
		"    target: {inboundSocks: {listen: 127.0.0.1}, address: 127.0.0.1:"
	blancBalancer = "        balancer: pool-blanc\n"
)

func TestCascadeCrossDependencyLoad(t *testing.T) {
	l, warnings := mustLoad(t, crossBase)
	if len(warnings) != 0 {
		t.Fatalf("warnings %q", warnings)
	}
	d := l.Config.Subscriptions[1].Sources[0].DependsOn
	want := []DependencyConfig{{Subscription: "blanc-en", Balancer: "pool-blanc", Health: HealthBalancer, RecoverGraceSeconds: 180}}
	if !reflect.DeepEqual(d, want) || d[0].Name() != "balancer:blanc-en/pool-blanc" {
		t.Fatalf("dependsOn %+v", d)
	}
	// The balancer of a source of the other subscription is valid (that is
	// the point); so is the same tag in the own target.balancers.
	mustLoad(t, cross(t, "        - {tag: pool-vpn,", "        - {tag: pool-blanc}\n        - {tag: pool-vpn,"))
	// A disabled source may depend on a disabled subscription.
	mustLoad(t, cross(t, "  - id: blanc-en\n", "  - id: blanc-en\n    enabled: false\n",
		"        balancer: pool-vpn\n", "        balancer: pool-vpn\n        enabled: false\n"))
	// Repeated subscription ids that nobody names are no error.
	mustLoad(t, cross(t, "  - id: blanc-ru\n", "  - id: dup\n"+plainSub+"8083}\n  - id: blanc-ru\n",
		"runtime: {}", "  - id: dup\n"+plainSub+"8082}\nruntime: {}"))
}

func TestCascadeCrossRules(t *testing.T) {
	const p = "subscriptions.1.sources.0.dependsOn."
	cases := []struct {
		name  string
		pairs []string
		want  string
	}{
		{"with source", []string{crossDep, "{subscription: blanc-en, source: blanc, health: balancer}"}, p + "0: subscription requires balancer, not source"},
		{"without balancer", []string{crossDep, "{subscription: blanc-en, health: balancer}"}, p + "0: subscription requires balancer"},
		{"own", []string{crossDep, "{subscription: blanc-ru, balancer: pool-p, health: balancer}"}, p + "0.subscription: the source's own subscription: omit subscription"},
		{"unknown", []string{crossDep, "{subscription: nope, balancer: pool-blanc, health: balancer}"}, p + `0.subscription: unknown subscription "nope"`},
		{"ambiguous", []string{"runtime: {}", thirdSub}, p + `0.subscription: subscription id "blanc-en" is not unique`},
		{
			"name collision",
			[]string{"[" + crossDep + "]", `[{balancer: "blanc-en/pool-blanc", health: balancer}, ` + crossDep + "]"},
			p + `1.balancer: duplicate dependency "balancer:blanc-en/pool-blanc"`,
		},
		{"repeat", []string{"[" + crossDep + "]", "[" + crossDep + ", " + crossDep + "]"}, p + `1.balancer: duplicate dependency "balancer:blanc-en/pool-blanc"`},
		{"disabled", []string{"  - id: blanc-en\n", "  - id: blanc-en\n    enabled: false\n"}, p + `0.subscription: subscription "blanc-en" is disabled`},
		{
			"not in balancers",
			[]string{crossDep, "{subscription: blanc-en, balancer: pool-x, health: balancer}"},
			p + `0.balancer: balancer "pool-x" is not in target.balancers of subscription "blanc-en"`,
		},
		{
			"no selection",
			[]string{enBalancer, "{tag: pool-blanc}"},
			p + `0.balancer: balancer "pool-blanc" of subscription "blanc-en" has no enabled selection; nothing tracks its health`,
		},
		{
			"selection disabled",
			[]string{enBalancer, "{tag: pool-blanc, selection: {enabled: false}}"},
			p + `0.balancer: balancer "pool-blanc" of subscription "blanc-en" has no enabled selection; nothing tracks its health`,
		},
		{
			"cycle across subscriptions",
			[]string{blancBalancer, blancBalancer + "        dependsOn: [{subscription: blanc-ru, balancer: pool-vpn, health: balancer}]\n"},
			"subscriptions.0.sources.0.dependsOn: dependsOn cycle across subscriptions: blanc-en/blanc → blanc-ru/warp → blanc-en/blanc",
		},
		{
			"cycle through a source edge",
			[]string{
				blancBalancer, blancBalancer + "        dependsOn: [{subscription: blanc-ru, balancer: pool-p, health: balancer}]\n",
				"        balancer: pool-p\n", "        balancer: pool-p\n        dependsOn: [{source: warp, health: balancer}]\n",
			},
			"subscriptions.0.sources.0.dependsOn: dependsOn cycle across subscriptions: blanc-en/blanc → blanc-ru/proton → blanc-ru/warp → blanc-en/blanc",
		},
		{
			// A cycle within one subscription is reported once, by checkCycles.
			"inner cycle",
			[]string{
				"[" + crossDep + "]", "[" + crossDep + ", {source: proton, health: balancer}]",
				"        balancer: pool-p\n", "        balancer: pool-p\n        dependsOn: [{source: warp, health: balancer}]\n",
			},
			"subscriptions.1.sources.0.dependsOn: dependsOn cycle: warp → proton → warp",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			msg := mustFail(t, cross(t, c.pairs...), c.want)
			if strings.Count(msg, "\n  - ") != 1 || strings.Count(msg, "dependsOn cycle") > 1 {
				t.Errorf("want one issue:\n%s", msg)
			}
		})
	}
}

func TestCascadeCrossWarnings(t *testing.T) {
	for _, st := range []string{"random", "roundRobin"} {
		_, warnings := mustLoad(t, cross(t, "schedule: '*/15 * * * * *'}}", "schedule: '*/15 * * * * *', strategy: "+st+"}}"))
		want := []string{`subscriptions.1.sources.0.dependsOn.0.health: balancer "pool-blanc" of subscription "blanc-en" has strategy "` + st +
			`", which selects every candidate; the dependency is always healthy`}
		if !reflect.DeepEqual(warnings, want) {
			t.Errorf("%s warnings %q", st, warnings)
		}
	}
}

func TestBalancerSelectionStale(t *testing.T) {
	last := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	cases := []struct {
		schedule string
		last     time.Time
		now      time.Time
		want     bool
	}{
		{"*/15 * * * * *", last, last.Add(45 * time.Second), false},
		{"*/15 * * * * *", last, last.Add(45*time.Second + 1), true},
		// Three activations after last: :10, 1:00, 1:10.
		{"0,10 * * * * *", last, last.Add(70 * time.Second), false},
		{"0,10 * * * * *", last, last.Add(70*time.Second + 1), true},
		{"0,10 * * * * *", last.Add(10 * time.Second), last.Add(80 * time.Second), false},
		{"0,10 * * * * *", last.Add(10 * time.Second), last.Add(120*time.Second + 1), true},
		{"*/15 * * * * *", time.Time{}, last, true},
		{"", last, last, true},
	}
	for _, c := range cases {
		b := BalancerSelectionConfig{Enabled: true, Schedule: c.schedule}
		if got := b.Stale(c.last, c.now); got != c.want {
			t.Errorf("%q Stale(%v, %v) = %v, want %v", c.schedule, c.last, c.now, got, c.want)
		}
	}
}

func TestFindBalancer(t *testing.T) {
	l, _ := mustLoad(t, crossBase)
	s, b, ok := FindBalancer(l.Config.Subscriptions, "blanc-en", "pool-blanc")
	if !ok || s.ID != "blanc-en" || b.Tag != "pool-blanc" || b.Selection == nil {
		t.Fatalf("FindBalancer %v %+v %+v", ok, s, b)
	}
	if _, _, ok := FindBalancer(l.Config.Subscriptions, "blanc-en", "pool-vpn"); ok {
		t.Error("pool-vpn found in blanc-en")
	}
}
