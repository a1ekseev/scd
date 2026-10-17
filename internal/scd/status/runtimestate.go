package status

import (
	"cmp"
	"context"
	"errors"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	routercmd "github.com/xtls/xray-core/app/router/command"
	"github.com/xtls/xray-core/core"

	"github.com/a1ekseev/scd/internal/common/inputsrc"
	"github.com/a1ekseev/scd/internal/scd/config"
	"github.com/a1ekseev/scd/internal/scd/manifest"
	"github.com/a1ekseev/scd/internal/scd/resources"
	"github.com/a1ekseev/scd/internal/scd/state"
	"github.com/a1ekseev/scd/internal/scd/topology"
	"github.com/a1ekseev/scd/internal/scd/xrayapi"
)

// Runtime-state texts (03 §6.3–§6.4).
const (
	msgTargetNotFound    = "Target not found in loaded config."
	msgExpectedNoRefetch = "Expected topology is unavailable for remote or stdin inputs; runtime-state does not refetch subscription sources."
)

// Runtime item classifications.
const (
	classFixed           = "fixed"
	classManaged         = "managed"
	classManagedInitial  = "managed-initial"
	classManagedFallback = "managed-fallback"
	classUnmanaged       = "unmanaged"
)

var remoteInputRe = regexp.MustCompile(`^(?i)https?://`)

// errorBody is the JSON error response of the status API.
type errorBody struct {
	Error string `json:"error"`
}

// runtimeStateSnapshot is /api/runtime-state (TS CurrentRuntimeStateSnapshot).
type runtimeStateSnapshot struct {
	SubscriptionID string         `json:"subscriptionId"`
	TargetAddress  string         `json:"targetAddress"`
	CapturedAt     string         `json:"capturedAt"`
	Config         rsConfig       `json:"config"`
	ServiceState   rsServiceState `json:"serviceState"`
	Expected       rsExpected     `json:"expected"`
	Runtime        rsRuntime      `json:"runtime"`
	Diff           rsDiff         `json:"diff"`
}

type rsConfig struct {
	Subscription config.PrintedSubscription      `json:"subscription"`
	Target       config.SubscriptionTargetConfig `json:"target"`
	Resources    config.ResourceConfig           `json:"resources"`
}

type rsServiceState struct {
	BalancerMonitor *rsBalancerMonitor `json:"balancerMonitor,omitempty"`
	// BalancerSelection is set when target.balancerSelection is enabled
	// (D22, 10 §6.6).
	BalancerSelection *rsBalancerSelection `json:"balancerSelection,omitempty"`
}

// rsBalancerSelection is the complete balancer selection state: the
// grouped view plus the state-only fields and every observatory status
// (sorted by tag; omitted when the observatory is disabled or failed).
type rsBalancerSelection struct {
	BalancerSelectionView
	PreviousSelectedTag  string          `json:"previousSelectedTag,omitempty"`
	ConsecutiveFailures  int             `json:"consecutiveFailures"`
	Observatory          []rsObservation `json:"observatory,omitzero"`
	ObservatoryCheckedAt string          `json:"observatoryCheckedAt,omitempty"`
}

// rsObservation is one ObservatoryService status (10 §3.1); healthPing is
// set by burstObservatory only.
type rsObservation struct {
	Tag        string        `json:"tag"`
	Alive      bool          `json:"alive"`
	DelayMs    *int64        `json:"delayMs,omitempty"`
	LastError  string        `json:"lastError,omitempty"`
	LastSeenAt string        `json:"lastSeenAt,omitempty"`
	LastTryAt  string        `json:"lastTryAt,omitempty"`
	HealthPing *rsHealthPing `json:"healthPing,omitempty"`
}

type rsHealthPing struct {
	All         int64   `json:"all"`
	Fail        int64   `json:"fail"`
	AverageMs   float64 `json:"averageMs"`
	DeviationMs float64 `json:"deviationMs"`
	MinMs       float64 `json:"minMs"`
	MaxMs       float64 `json:"maxMs"`
}

// runtimeSelection is the runtime-state form of s.
func runtimeSelection(t config.SubscriptionTargetConfig, s *state.SelectionState, tunnels []*state.Tunnel) *rsBalancerSelection {
	out := &rsBalancerSelection{
		BalancerSelectionView: selectionView(t.BalancerSelection, t.ObservatorySubjectSelectorPrefix, s, tunnels),
		PreviousSelectedTag:   s.PreviousSelectedTag,
		ConsecutiveFailures:   s.ConsecutiveFailures,
		ObservatoryCheckedAt:  formatOptTime(s.ObservatoryCheckedAt),
	}
	if s.Observatory != nil {
		out.Observatory = make([]rsObservation, 0, len(s.Observatory))
		for _, tag := range slices.Sorted(maps.Keys(s.Observatory)) {
			o := s.Observatory[tag]
			ro := rsObservation{
				Tag: o.Tag, Alive: o.Alive, DelayMs: clone(o.DelayMs), LastError: o.LastError,
				LastSeenAt: formatOptTime(o.LastSeenAt), LastTryAt: formatOptTime(o.LastTryAt),
			}
			if hp := o.HealthPing; hp != nil {
				ro.HealthPing = &rsHealthPing{
					All: hp.All, Fail: hp.Fail, AverageMs: durationMs(hp.Average), DeviationMs: durationMs(hp.Deviation),
					MinMs: durationMs(hp.Min), MaxMs: durationMs(hp.Max),
				}
			}
			out.Observatory = append(out.Observatory, ro)
		}
	}
	return out
}

// rsBalancerMonitor is the complete balancer monitor state.
type rsBalancerMonitor struct {
	BalancerMonitorView
	ConsecutiveFailures int `json:"consecutiveFailures"`
}

type rsExpected struct {
	Source       string             `json:"source"`
	Outbounds    []expectedOutbound `json:"outbounds"`
	Inbounds     []expectedInbound  `json:"inbounds"`
	RoutingRules []expectedRule     `json:"routingRules"`
	Error        string             `json:"error,omitempty"`
}

type expectedOutbound struct {
	BaseTunnelID string `json:"baseTunnelId"`
	DisplayName  string `json:"displayName"`
	CountryISO2  string `json:"countryIso2,omitempty"`
	InitialTag   string `json:"initialTag"`
	FallbackTag  string `json:"fallbackTag"`
	Flow         string `json:"flow,omitempty"`
}

type expectedInbound struct {
	BaseTunnelID string `json:"baseTunnelId"`
	DisplayName  string `json:"displayName"`
	CountryISO2  string `json:"countryIso2,omitempty"`
	Tag          string `json:"tag"`
	Endpoint     string `json:"endpoint"`
}

type expectedRule struct {
	BaseTunnelID        string `json:"baseTunnelId"`
	DisplayName         string `json:"displayName"`
	CountryISO2         string `json:"countryIso2,omitempty"`
	RuleTag             string `json:"ruleTag"`
	InboundTag          string `json:"inboundTag"`
	OutboundInitialTag  string `json:"outboundInitialTag"`
	OutboundFallbackTag string `json:"outboundFallbackTag"`
}

type rsRuntime struct {
	Outbounds    []runtimeItem `json:"outbounds"`
	Inbounds     []runtimeItem `json:"inbounds"`
	RoutingRules []runtimeRule `json:"routingRules"`
}

// runtimeItem is a runtime outbound or inbound. Parsed is nil for a
// handler scd does not describe (e.g. freedom); RawBase64 is set only with
// includeRaw and holds the unredacted protobuf.
type runtimeItem struct {
	Tag            string         `json:"tag"`
	Classification string         `json:"classification"`
	Parsed         map[string]any `json:"parsed,omitempty"`
	RawBase64      string         `json:"rawBase64,omitempty"`
}

type runtimeRule struct {
	RuleTag        string         `json:"ruleTag"`
	Classification string         `json:"classification"`
	Parsed         map[string]any `json:"parsed"`
	RawBase64      string         `json:"rawBase64,omitempty"`
}

type rsDiff struct {
	Outbounds    outboundDiff `json:"outbounds"`
	Inbounds     tagDiff      `json:"inbounds"`
	RoutingRules tagDiff      `json:"routingRules"`
}

type outboundDiff struct {
	Matched         []string `json:"matched"`
	MatchedFallback []string `json:"matchedFallback"`
	Missing         []string `json:"missing"`
	Unexpected      []string `json:"unexpected"`
}

type tagDiff struct {
	Matched    []string `json:"matched"`
	Missing    []string `json:"missing"`
	Unexpected []string `json:"unexpected"`
}

// BuildRuntimeState builds the runtime-state diagnostic of one configured
// target (03 §6.4) and returns the JSON value with its HTTP status: 200 and
// the snapshot, 404 when the pair is not configured, 502 for an Xray API
// error (*xrayapi.Error) and 500 for any other error (also returned as
// err). Secrets are redacted unless statusServer.runtimeState.includeSecrets
// (03 §6.5, D33); rawBase64 is added with includeRaw.
func BuildRuntimeState(ctx context.Context, d Deps, subscriptionID, targetAddress string) (any, int, error) {
	cfg := d.Config.Config
	i := slices.IndexFunc(cfg.Subscriptions, func(s config.SubscriptionConfig) bool { return s.ID == subscriptionID })
	if i < 0 || cfg.Subscriptions[i].Target.Address != targetAddress {
		return errorBody{msgTargetNotFound}, http.StatusNotFound, nil
	}
	sub := cfg.Subscriptions[i]
	opts := cfg.StatusServer.RuntimeState
	now := time.Now
	if d.Now != nil {
		now = d.Now
	}

	expected := buildExpected(ctx, sub, cfg.Resources, opts.IncludeSecrets, now())
	outbounds, inbounds, rules, err := listRuntime(ctx, d.Dial, sub.Target)
	if err != nil {
		code := http.StatusInternalServerError
		if _, ok := errors.AsType[*xrayapi.Error](err); ok {
			code = http.StatusBadGateway
		}
		return errorBody{err.Error()}, code, err
	}

	snap := runtimeStateSnapshot{
		SubscriptionID: subscriptionID,
		TargetAddress:  targetAddress,
		CapturedAt:     formatTime(now()),
		Config:         redactedConfig(sub, cfg.Resources, opts.IncludeSecrets),
		Expected:       expected,
		Diff: rsDiff{
			Outbounds:    outboundDiff{Matched: []string{}, MatchedFallback: []string{}, Missing: []string{}, Unexpected: []string{}},
			Inbounds:     tagDiff{Matched: []string{}, Missing: []string{}, Unexpected: []string{}},
			RoutingRules: tagDiff{Matched: []string{}, Missing: []string{}, Unexpected: []string{}},
		},
	}
	if d.Memory != nil {
		d.Memory.View(state.Key(subscriptionID, targetAddress), func(t *state.Target) {
			if t.SyncSeen {
				snap.ServiceState.BalancerMonitor = &rsBalancerMonitor{balancerView(&t.Balancer), t.Balancer.ConsecutiveFailures}
			}
			if sub.Target.BalancerSelection.Enabled {
				snap.ServiceState.BalancerSelection = runtimeSelection(sub.Target, &t.BalancerSelection, t.Tunnels)
			}
		})
	}
	snap.Runtime.Outbounds = classifyOutbounds(outbounds, sub.Target, expected.Outbounds, opts, &snap.Diff.Outbounds)
	snap.Runtime.Inbounds = classifyInbounds(inbounds, sub.Target, expected.Inbounds, opts, &snap.Diff.Inbounds)
	snap.Runtime.RoutingRules = classifyRules(rules, sub.Target, expected.RoutingRules, opts, &snap.Diff.RoutingRules)
	return snap, http.StatusOK, nil
}

// listRuntime lists outbounds, inbounds and rules of the target in
// parallel (03 §6.4). The first error in that order wins.
func listRuntime(ctx context.Context, dial xrayapi.Dialer, t config.SubscriptionTargetConfig) (
	[]*core.OutboundHandlerConfig, []*core.InboundHandlerConfig, []xrayapi.Rule, error,
) {
	if dial == nil {
		dial = xrayapi.Dial
	}
	client, err := dial(t.Address, time.Duration(t.TimeoutMs)*time.Millisecond)
	if err != nil {
		return nil, nil, nil, err
	}
	defer func() { _ = client.Close() }()

	var (
		wg                     sync.WaitGroup
		outbounds              []*core.OutboundHandlerConfig
		inbounds               []*core.InboundHandlerConfig
		rules                  []xrayapi.Rule
		outErr, inErr, ruleErr error
	)
	wg.Go(func() { outbounds, outErr = client.ListOutbounds(ctx) })
	wg.Go(func() { inbounds, inErr = client.ListInbounds(ctx) })
	wg.Go(func() { rules, ruleErr = client.ListRules(ctx) })
	wg.Wait()
	if err := cmp.Or(outErr, inErr, ruleErr); err != nil {
		return nil, nil, nil, err
	}
	return outbounds, inbounds, rules, nil
}

// buildExpected derives the expected topology from the subscription source
// (03 §6.4): remote and stdin inputs are not refetched; a file is reloaded
// and built as sync does (manifest.FromSubscription, topology.Build).
func buildExpected(ctx context.Context, sub config.SubscriptionConfig, res config.ResourceConfig, includeSecrets bool, now time.Time) rsExpected {
	source := sub.Input
	if !includeSecrets {
		source = inputsrc.RedactInput(sub.Input)
	}
	empty := rsExpected{Source: source, Outbounds: []expectedOutbound{}, Inbounds: []expectedInbound{}, RoutingRules: []expectedRule{}}
	if sub.Input == "-" || remoteInputRe.MatchString(sub.Input) {
		empty.Error = msgExpectedNoRefetch
		return empty
	}

	fail := func(err error) rsExpected {
		msg := err.Error()
		if !includeSecrets {
			msg = inputsrc.RedactInputIn(msg, sub.Input)
		}
		empty.Error = msg
		return empty
	}
	l, err := inputsrc.Load(ctx, sub.Input, inputsrc.Options{
		Format: sub.Format, FetchTimeout: time.Duration(sub.FetchTimeoutMs) * time.Millisecond,
	})
	if err != nil {
		return fail(err)
	}
	m, err := manifest.FromSubscription(manifest.LoadedSubscription{Config: sub, Source: l.Source, Content: l.Content, Encoding: l.Encoding}, now)
	if err != nil {
		return fail(err)
	}
	topo, err := topology.Build(m, sub.Target)
	if err != nil {
		return fail(err)
	}

	exp := rsExpected{Source: l.Source, Outbounds: []expectedOutbound{}, Inbounds: []expectedInbound{}, RoutingRules: []expectedRule{}}
	for _, tn := range topo.Tunnels {
		if res.Outbounds.Enabled {
			exp.Outbounds = append(exp.Outbounds, expectedOutbound{
				BaseTunnelID: tn.BaseTunnelID, DisplayName: tn.DisplayName, CountryISO2: tn.CountryISO2,
				InitialTag: tn.OutboundTagInitial, FallbackTag: tn.OutboundWithoutPrefix.Tag, Flow: tn.OutboundInitial.Normalized.Flow,
			})
		}
		if res.Inbounds.Enabled {
			exp.Inbounds = append(exp.Inbounds, expectedInbound{
				BaseTunnelID: tn.BaseTunnelID, DisplayName: tn.DisplayName, CountryISO2: tn.CountryISO2,
				Tag: tn.InboundTag, Endpoint: tn.Listen + ":" + strconv.Itoa(tn.Port),
			})
		}
		if res.Routing.Enabled {
			exp.RoutingRules = append(exp.RoutingRules, expectedRule{
				BaseTunnelID: tn.BaseTunnelID, DisplayName: tn.DisplayName, CountryISO2: tn.CountryISO2,
				RuleTag: tn.RouteTag, InboundTag: tn.InboundTag,
				OutboundInitialTag: tn.OutboundTagInitial, OutboundFallbackTag: tn.OutboundWithoutPrefix.Tag,
			})
		}
	}
	return exp
}

// redactedConfig is the config section: the subscription (input and
// fetchHeaders redacted) and its target (monitor URLs redacted, also in the
// copy inside the subscription) unless includeSecrets.
func redactedConfig(sub config.SubscriptionConfig, res config.ResourceConfig, includeSecrets bool) rsConfig {
	out := sub.Printed(includeSecrets)
	return rsConfig{Subscription: out, Target: out.Target, Resources: res}
}

// sortedStrings returns s sorted with strings.Compare (D23).
func sortedStrings(s []string) []string {
	slices.Sort(s)
	return s
}

// classifyOutbounds classifies runtime outbounds (fixed, managed-initial,
// managed-fallback, unmanaged) and fills diff.
func classifyOutbounds(list []*core.OutboundHandlerConfig, t config.SubscriptionTargetConfig, expected []expectedOutbound,
	opts config.RuntimeStateConfig, diff *outboundDiff,
) []runtimeItem {
	initial, fallback, present := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, e := range expected {
		initial[e.InitialTag], fallback[e.FallbackTag] = true, true
	}
	items := make([]runtimeItem, 0, len(list))
	for _, h := range list {
		tag := h.GetTag()
		present[tag] = true
		item := runtimeItem{Tag: tag, Classification: classUnmanaged}
		switch {
		case slices.Contains(t.FixedOutbounds, tag):
			item.Classification = classFixed
		case initial[tag]:
			item.Classification = classManagedInitial
			diff.Matched = append(diff.Matched, tag)
		case fallback[tag]:
			item.Classification = classManagedFallback
			diff.MatchedFallback = append(diff.MatchedFallback, tag)
		default:
			diff.Unexpected = append(diff.Unexpected, tag)
		}
		if p, ok := resources.DescribeOutbound(h, opts.IncludeSecrets); ok {
			item.Parsed = p
		}
		if opts.IncludeRaw {
			item.RawBase64 = resources.RawBase64(h)
		}
		items = append(items, item)
	}
	for _, e := range expected {
		if !present[e.InitialTag] && !present[e.FallbackTag] {
			diff.Missing = append(diff.Missing, e.BaseTunnelID)
		}
	}
	sortedStrings(diff.Matched)
	sortedStrings(diff.MatchedFallback)
	sortedStrings(diff.Missing)
	sortedStrings(diff.Unexpected)
	slices.SortStableFunc(items, func(a, b runtimeItem) int { return strings.Compare(a.Tag, b.Tag) })
	return items
}

// classifyInbounds classifies runtime inbounds (fixed, managed, unmanaged)
// and fills diff.
func classifyInbounds(list []*core.InboundHandlerConfig, t config.SubscriptionTargetConfig, expected []expectedInbound,
	opts config.RuntimeStateConfig, diff *tagDiff,
) []runtimeItem {
	want := map[string]bool{}
	for _, e := range expected {
		want[e.Tag] = true
	}
	present := map[string]bool{}
	items := make([]runtimeItem, 0, len(list))
	for _, h := range list {
		tag := h.GetTag()
		present[tag] = true
		item := runtimeItem{Tag: tag, Classification: classify(tag, t.FixedInbounds, want, diff)}
		if p, ok := resources.DescribeInbound(h); ok {
			item.Parsed = p
		}
		if opts.IncludeRaw {
			item.RawBase64 = resources.RawBase64(h)
		}
		items = append(items, item)
	}
	for _, e := range expected {
		if !present[e.Tag] {
			diff.Missing = append(diff.Missing, e.Tag)
		}
	}
	finishDiff(diff)
	slices.SortStableFunc(items, func(a, b runtimeItem) int { return strings.Compare(a.Tag, b.Tag) })
	return items
}

// classifyRules classifies runtime routing rules by ruleTag and fills diff.
// rawBase64 is the ListRule item (outbound tag and ruleTag).
func classifyRules(list []xrayapi.Rule, t config.SubscriptionTargetConfig, expected []expectedRule,
	opts config.RuntimeStateConfig, diff *tagDiff,
) []runtimeRule {
	want := map[string]bool{}
	for _, e := range expected {
		want[e.RuleTag] = true
	}
	present := map[string]bool{}
	items := make([]runtimeRule, 0, len(list))
	for _, r := range list {
		present[r.RuleTag] = true
		item := runtimeRule{RuleTag: r.RuleTag, Classification: classify(r.RuleTag, t.FixedRouting, want, diff), Parsed: resources.DescribeRule(r)}
		if opts.IncludeRaw {
			item.RawBase64 = resources.RawBase64(&routercmd.ListRuleItem{Tag: r.Tag, RuleTag: r.RuleTag})
		}
		items = append(items, item)
	}
	for _, e := range expected {
		if !present[e.RuleTag] {
			diff.Missing = append(diff.Missing, e.RuleTag)
		}
	}
	finishDiff(diff)
	slices.SortStableFunc(items, func(a, b runtimeRule) int { return strings.Compare(a.RuleTag, b.RuleTag) })
	return items
}

// classify returns fixed, managed or unmanaged for an inbound or rule tag
// and records managed/unmanaged tags in diff.
func classify(tag string, fixed []string, want map[string]bool, diff *tagDiff) string {
	switch {
	case slices.Contains(fixed, tag):
		return classFixed
	case want[tag]:
		diff.Matched = append(diff.Matched, tag)
		return classManaged
	default:
		diff.Unexpected = append(diff.Unexpected, tag)
		return classUnmanaged
	}
}

func finishDiff(d *tagDiff) {
	sortedStrings(d.Matched)
	sortedStrings(d.Missing)
	sortedStrings(d.Unexpected)
}
