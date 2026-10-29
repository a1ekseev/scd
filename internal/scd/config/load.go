package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/a1ekseev/scd/internal/common/configload"
	"github.com/a1ekseev/scd/internal/common/inputsrc"
	"github.com/a1ekseev/scd/internal/common/labelfilter"
	"github.com/a1ekseev/scd/internal/common/schema"
	"github.com/a1ekseev/scd/internal/scd/country"
)

const defaultTimeoutMs = 5000

var (
	required = schema.StrOpt{Required: true, Trim: true, MinLen: 1}
	// optional is a trimmed optional string; an empty one counts as absent.
	optional = schema.StrOpt{Trim: true, EmptyToAbsent: true}
	// tags is a list of non-empty strings defaulting to [].
	tags = schema.StrOpt{Trim: true, MinLen: 1, AllowEmptyList: true}
)

// Load reads and validates the scd config at path. ${NAME} references are
// expanded in values (not in keys or comments) before they are typed; an
// undefined variable is an error. Relative inputs are resolved against the
// config directory. All validation problems are reported together; warnings
// are problems that do not make the config invalid.
func Load(path string) (Loaded, []string, error) {
	doc, err := configload.Load(path, configload.EnvRawStrict, os.LookupEnv)
	if err != nil {
		return Loaded{}, nil, fmt.Errorf("failed to read config %s: %w", path, err)
	}

	var issues schema.Issues
	root := schema.Root(doc.Root, &issues)
	root.Strict("subscriptions", "runtime", "logging", "resources", "statusServer")
	var cfg AppConfig
	var subNodes []*schema.Node
	for _, sn := range root.Array("subscriptions", true, 1) {
		if sn != nil {
			cfg.Subscriptions = append(cfg.Subscriptions, parseSubscription(sn, doc.Dir))
			subNodes = append(subNodes, sn)
		}
	}
	cfg.Runtime = parseRuntime(root.Object("runtime", true))
	cfg.Logging = parseLogging(root.Object("logging", true))
	cfg.Resources = parseResources(root.Object("resources", true))
	cfg.StatusServer = parseStatusServer(root.Object("statusServer", false))

	warnings := checkRules(root, &cfg, subNodes)
	if err := issues.Err(path); err != nil {
		return Loaded{}, nil, err
	}
	return Loaded{Path: doc.Path, Config: cfg}, warnings, nil
}

func positive(def int) schema.IntOpt {
	minV := 1
	return schema.IntOpt{Default: &def, Min: &minV}
}

func intRange(lo, hi int, req bool) schema.IntOpt {
	return schema.IntOpt{Required: req, Min: &lo, Max: &hi}
}

func parseSubscription(n *schema.Node, dir string) SubscriptionConfig {
	n.Strict("id", "enabled", "sources", "target")
	var s SubscriptionConfig
	s.ID, _ = n.String("id", required)
	s.Enabled = n.Bool("enabled", true)
	for _, sn := range n.Array("sources", true, 1) {
		if sn != nil {
			s.Sources = append(s.Sources, parseSource(sn, dir))
		}
	}
	s.Target = parseTarget(n.Object("target", true))
	return s
}

// Input size limits of a source (sources[].maxInputBytes).
const (
	minInputBytes     = 1 << 10
	defaultInputBytes = 64 << 20
)

// vlessOnlyKeys and wireguardOnlyKeys are the source keys of one type only.
var (
	vlessOnlyKeys     = []string{"format", "fetchTimeoutMs", "allowInsecure", "fetchUserAgent", "fetchHeaders"}
	wireguardOnlyKeys = []string{"maxActive", "selection", "wireguard"}
)

// parseSource reads one sources[] object; the keys of another type are
// errors.
func parseSource(n *schema.Node, dir string) SourceConfig {
	n.Strict("id", "type", "input", "enabled", "format", "fetchTimeoutMs", "allowInsecure",
		"fetchUserAgent", "fetchHeaders", "maxInputBytes", "portRange", "filters",
		"maxActive", "selection", "monitor", "wireguard")
	var s SourceConfig
	s.ID, _ = n.String("id", required)
	s.Type = n.Enum("type", []string{SourceVLESS, SourceWireGuard}, "")
	foreign := wireguardOnlyKeys
	if s.Type == SourceWireGuard {
		foreign = vlessOnlyKeys
	}
	for _, k := range foreign {
		if s.Type != "" && n.Has(k) {
			n.Addf(k, "not allowed for type %q", s.Type)
		}
	}
	if input, ok := n.String("input", required); ok {
		if s.Type == SourceWireGuard && !filepath.IsAbs(input) {
			n.Addf("input", "must be an absolute file path for type %q", s.Type)
		}
		s.Input = configload.ResolveInput(dir, input)
	}
	s.Enabled = n.Bool("enabled", true)
	if s.Type == SourceVLESS {
		s.Format = n.Enum("format", []string{"auto", "plain", "base64"}, "auto")
		s.FetchTimeoutMs = n.Int("fetchTimeoutMs", positive(defaultTimeoutMs))
		s.AllowInsecure = n.Enum("allowInsecure", []string{"deny", "allow"}, "deny")
		s.FetchUserAgent, _ = n.String("fetchUserAgent", schema.StrOpt{Trim: true, MinLen: 1})
		if h := n.StringMap("fetchHeaders"); len(h) > 0 {
			for _, is := range inputsrc.CheckHeaders(h) {
				n.Addf("fetchHeaders."+is.Key, "%s", is.Message)
			}
			s.FetchHeaders = inputsrc.Headers(h)
		}
	}
	limit := defaultInputBytes
	s.MaxInputBytes = int64(n.Int("maxInputBytes", schema.IntOpt{Default: &limit, Min: new(minInputBytes), Max: new(defaultInputBytes)}))
	if pn := n.Object("portRange", false); pn != nil {
		s.PortRange = parsePortRange(pn)
	}
	if fn := n.Object("filters", false); fn != nil {
		s.Filters = parseFilters(fn, s.Type)
	}
	if mn := n.Object("monitor", false); mn != nil {
		s.Monitor = parseSourceMonitor(mn)
	}
	if s.Type == SourceWireGuard {
		s.MaxActive = n.Int("maxActive", schema.IntOpt{Required: true, Min: new(1)})
		s.Selection = parseSelection(n.Object("selection", true))
		wn := n.Object("wireguard", true)
		wn.Strict("endpointDomainStrategy")
		s.WireGuard = &WireGuardConfig{EndpointDomainStrategy: wn.Enum("endpointDomainStrategy", EndpointDomainStrategies, "")}
	}
	return s
}

// parseSelection reads the selection object of a wireguard source; n may
// be nil (already reported as required).
func parseSelection(n *schema.Node) *SelectionConfig {
	if n == nil {
		return nil
	}
	n.Strict("strategy", "switchMarginPercent", "switchConfirmChecks", "deadPairCooldownMinutes", "replaceAfterFailures")
	return &SelectionConfig{
		Strategy:                n.Enum("strategy", []string{StrategyScore, StrategyLoad, StrategyLabel}, ""),
		SwitchMarginPercent:     n.Int("switchMarginPercent", intRange(0, 100, true)),
		SwitchConfirmChecks:     n.Int("switchConfirmChecks", intRange(1, 10, true)),
		DeadPairCooldownMinutes: n.Int("deadPairCooldownMinutes", schema.IntOpt{Required: true, Min: new(4)}),
		ReplaceAfterFailures:    n.Int("replaceAfterFailures", intRange(1, 10, true)),
	}
}

// parseSourceMonitor reads sources[].monitor: request.timeoutMs and
// retry.{attempts,delayMs}, each optional.
func parseSourceMonitor(n *schema.Node) *SourceMonitorConfig {
	n.Strict("request", "retry")
	m := &SourceMonitorConfig{}
	if rn := n.Object("request", false); rn != nil {
		rn.Strict("timeoutMs")
		m.Request = &SourceMonitorRequestConfig{TimeoutMs: rn.Int("timeoutMs", schema.IntOpt{Required: true, Min: new(1)})}
	}
	if rn := n.Object("retry", false); rn != nil {
		rn.Strict("attempts", "delayMs")
		m.Retry = &SourceRetryConfig{Attempts: rn.Int("attempts", schema.IntOpt{Min: new(1)})}
		if rn.Has("delayMs") {
			m.Retry.DelayMs = new(rn.Int("delayMs", schema.IntOpt{Min: new(0)}))
		}
	}
	return m
}

func parsePortRange(n *schema.Node) *PortRange {
	n.Strict("start", "end")
	port := intRange(1, 65535, true)
	return &PortRange{Start: n.Int("start", port), End: n.Int("end", port)}
}

// parseFilters reads the filters object of a source of type typ;
// entryCountryAllowlist is for wireguard only.
func parseFilters(n *schema.Node, typ string) *SourceFiltersConfig {
	n.Strict("countryAllowlist", "labelIncludeRegex", "labelExcludeRegex", "entryCountryAllowlist")
	f := &SourceFiltersConfig{Label: labelfilter.Parse(n), CountryAllowlist: countryList(n, "countryAllowlist")}
	if n.Has("entryCountryAllowlist") && typ != "" && typ != SourceWireGuard {
		n.Addf("entryCountryAllowlist", "not allowed for type %q", typ)
	}
	f.EntryCountryAllowlist = countryList(n, "entryCountryAllowlist")
	return f
}

// countryList reads a list of ISO2 codes, upper-cased.
func countryList(n *schema.Node, key string) []string {
	var out []string
	for _, e := range n.StringListElems(key, schema.StrOpt{Trim: true, AllowEmptyList: true}) {
		code := strings.ToUpper(e.Value)
		switch {
		case len(code) != 2:
			n.Addf(e.Key, "must be a 2-letter ISO2 country code")
		case !country.Supported(code):
			n.Addf(e.Key, "unsupported ISO2 country code %q", code)
		default:
			out = append(out, code)
		}
	}
	return out
}

// parseTarget reads the target object; n may be nil (missing target).
func parseTarget(n *schema.Node) SubscriptionTargetConfig {
	n.Strict("address", "timeoutMs", "fixedOutbounds", "fixedInbounds", "fixedRouting",
		"visionUdp443Override", "inboundSocks", "monitor", "balancerMonitor", "balancerSelection",
		"observatorySubjectSelectorPrefix")
	var t SubscriptionTargetConfig
	t.Address, _ = n.String("address", required)
	t.TimeoutMs = n.Int("timeoutMs", positive(defaultTimeoutMs))
	t.FixedOutbounds = tagList(n, "fixedOutbounds")
	t.FixedInbounds = tagList(n, "fixedInbounds")
	t.FixedRouting = tagList(n, "fixedRouting")
	t.VisionUDP443Override = n.Bool("visionUdp443Override", false)
	if sn := n.Object("inboundSocks", false); sn != nil {
		t.InboundSocks = parseInboundSocks(sn)
	}
	t.Monitor = parseMonitor(n.Object("monitor", false))
	t.BalancerMonitor = parseBalancerMonitor(n.Object("balancerMonitor", false))
	t.BalancerSelection = parseBalancerSelection(n.Object("balancerSelection", false))
	t.ObservatorySubjectSelectorPrefix, _ = n.String("observatorySubjectSelectorPrefix", optional)
	return t
}

func tagList(n *schema.Node, key string) []string {
	if l := n.StringList(key, tags); l != nil {
		return l
	}
	return []string{}
}

func parseInboundSocks(n *schema.Node) *InboundSocksConfig {
	n.Strict("listen")
	s := &InboundSocksConfig{}
	s.Listen, _ = n.String("listen", required)
	return s
}

func parseRetry(n *schema.Node) RetryConfig {
	n.Strict("attempts", "delayMs")
	zero, delay := 0, 250
	return RetryConfig{
		Attempts: n.Int("attempts", positive(1)),
		DelayMs:  n.Int("delayMs", schema.IntOpt{Default: &delay, Min: &zero}),
	}
}

func parseRequest(n *schema.Node) *TargetMonitorRequestConfig {
	n.Strict("url", "method", "expectedStatus", "timeoutMs")
	r := &TargetMonitorRequestConfig{}
	r.URL, _ = n.URL("url", true)
	r.Method = n.Enum("method", []string{"GET", "HEAD", "POST"}, "GET")
	r.ExpectedStatus = n.Int("expectedStatus", intRange(100, 599, true))
	r.TimeoutMs = n.Int("timeoutMs", positive(defaultTimeoutMs))
	return r
}

// parseMonitor reads the monitor object; nil gives the defaults.
func parseMonitor(n *schema.Node) TargetMonitorConfig {
	n.Strict("enabled", "schedule", "maxParallel", "retry", "request")
	m := TargetMonitorConfig{
		Enabled:     n.Bool("enabled", false),
		MaxParallel: n.Int("maxParallel", positive(10)),
		Retry:       parseRetry(n.Object("retry", false)),
	}
	m.Schedule, _ = n.String("schedule", optional)
	if rn := n.Object("request", false); rn != nil {
		m.Request = parseRequest(rn)
	}
	return m
}

// parseBalancerMonitor reads the balancerMonitor object; nil gives the defaults.
func parseBalancerMonitor(n *schema.Node) TargetBalancerMonitorConfig {
	n.Strict("enabled", "schedule", "socks5", "request", "retry", "remotePing")
	b := TargetBalancerMonitorConfig{Enabled: n.Bool("enabled", false)}
	b.Schedule, _ = n.String("schedule", optional)
	if sn := n.Object("socks5", false); sn != nil {
		sn.Strict("host", "port")
		b.Socks5 = &BalancerMonitorSocks5Config{}
		b.Socks5.Host, _ = sn.String("host", required)
		b.Socks5.Port = sn.Int("port", intRange(1, 65535, true))
	}
	if rn := n.Object("request", false); rn != nil {
		b.Request = parseRequest(rn)
	}
	b.Retry = parseRetry(n.Object("retry", false))

	rp := n.Object("remotePing", false)
	rp.Strict("enabled", "url", "timeoutMs", "viaSocks")
	b.RemotePing.Enabled = rp.Bool("enabled", false)
	url, ok := rp.URL("url", false)
	if b.RemotePing.Enabled && !ok && !rp.Has("url") {
		rp.Addf("url", "required when remotePing.enabled is true")
	}
	b.RemotePing.URL = url
	b.RemotePing.TimeoutMs = rp.Int("timeoutMs", positive(defaultTimeoutMs))
	b.RemotePing.ViaSocks = rp.Bool("viaSocks", false)
	return b
}

// parseBalancerSelection reads the balancerSelection object (10 §6.5); nil
// gives the defaults.
func parseBalancerSelection(n *schema.Node) BalancerSelectionConfig {
	n.Strict("enabled", "balancerTag", "schedule", "observatory", "strategy")
	b := BalancerSelectionConfig{
		Enabled:     n.Bool("enabled", false),
		Observatory: n.Bool("observatory", true),
	}
	b.BalancerTag, _ = n.String("balancerTag", optional)
	b.Schedule, _ = n.String("schedule", optional)
	if n.Has("strategy") {
		b.Strategy = n.Enum("strategy", []string{"leastPing", "leastLoad", "random", "roundRobin"}, "")
	}
	return b
}

func parseRuntime(n *schema.Node) RuntimeConfig {
	n.Strict("mode", "schedule")
	r := RuntimeConfig{Mode: n.Enum("mode", []string{"run-once", "daemon"}, "run-once")}
	r.Schedule, _ = n.String("schedule", optional)
	return r
}

func parseLogging(n *schema.Node) LoggingConfig {
	n.Strict("level", "format")
	return LoggingConfig{
		Level:  n.Enum("level", []string{"fatal", "error", "warn", "info", "debug", "trace", "silent"}, "info"),
		Format: n.Enum("format", []string{"json", "pretty"}, "json"),
	}
}

func parseResources(n *schema.Node) ResourceConfig {
	n.Strict("outbounds", "inbounds", "routing")
	toggle := func(key string, def bool) ResourceToggle {
		tn := n.Object(key, false)
		tn.Strict("enabled")
		return ResourceToggle{Enabled: tn.Bool("enabled", def)}
	}
	return ResourceConfig{
		Outbounds: toggle("outbounds", true),
		Inbounds:  toggle("inbounds", false),
		Routing:   toggle("routing", false),
	}
}

// parseStatusServer reads the statusServer object; nil gives the defaults.
func parseStatusServer(n *schema.Node) StatusServerConfig {
	n.Strict("enabled", "listen", "instanceId", "remoteInstances", "runtimeState")
	defListen := "127.0.0.1:9090"
	s := StatusServerConfig{Enabled: n.Bool("enabled", true), RemoteInstances: []RemoteStatusInstanceConfig{}}
	s.Listen, _ = n.String("listen", schema.StrOpt{Trim: true, Default: &defListen})
	s.InstanceID, _ = n.String("instanceId", optional)
	seen := map[string]bool{}
	for _, rn := range n.Array("remoteInstances", false, 0) {
		if rn == nil {
			continue
		}
		rn.Strict("id", "apiUrl", "schedule", "timeoutMs")
		var r RemoteStatusInstanceConfig
		r.ID, _ = rn.String("id", required)
		r.APIURL, _ = rn.URL("apiUrl", true)
		r.Schedule, _ = rn.String("schedule", required)
		r.TimeoutMs = rn.Int("timeoutMs", positive(defaultTimeoutMs))
		checkRemoteInstance(rn, r, seen)
		s.RemoteInstances = append(s.RemoteInstances, r)
	}
	rs := n.Object("runtimeState", false)
	rs.Strict("enabled", "includeRaw", "includeSecrets")
	s.RuntimeState = RuntimeStateConfig{
		Enabled:        rs.Bool("enabled", true),
		IncludeRaw:     rs.Bool("includeRaw", false),
		IncludeSecrets: rs.Bool("includeSecrets", false),
	}
	return s
}
