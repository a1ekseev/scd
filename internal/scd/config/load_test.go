package config

import (
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/a1ekseev/scd/internal/common/jsonout"
)

const realShapeGo = "../../../docker/scd/config.real-shape.go.yml"

// base is a minimal valid config (01 §2.6); tests replace parts of it.
const base = `subscriptions:
  - id: s1
    input: ./sub.txt
    target:
      address: 127.0.0.1:8080
runtime: {}
logging: {}
resources: {}
`

func write(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func mustLoad(t *testing.T, body string) (Loaded, []string) {
	t.Helper()
	l, warnings, err := Load(write(t, body))
	if err != nil {
		t.Fatal(err)
	}
	return l, warnings
}

func mustFail(t *testing.T, body string, want ...string) string {
	t.Helper()
	_, _, err := Load(write(t, body))
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	for _, w := range want {
		if !strings.Contains(msg, w) {
			t.Errorf("missing %q in:\n%s", w, msg)
		}
	}
	return msg
}

// replace swaps one exact fragment of base.
func replace(t *testing.T, old, repl string) string {
	t.Helper()
	if !strings.Contains(base, old) {
		t.Fatalf("fragment %q not in base", old)
	}
	return strings.Replace(base, old, repl, 1)
}

// withTarget appends lines (indented for the target object) to the target.
func withTarget(t *testing.T, lines string) string {
	t.Helper()
	return replace(t, "      address: 127.0.0.1:8080\n", "      address: 127.0.0.1:8080\n"+lines)
}

// withSub appends lines (indented for the subscription object) to s1.
func withSub(t *testing.T, lines string) string {
	t.Helper()
	return replace(t, "    input: ./sub.txt\n", "    input: ./sub.txt\n"+lines)
}

// allResources enables outbounds, inbounds and routing.
const allResources = "resources:\n  outbounds: {enabled: true}\n  inbounds: {enabled: true}\n  routing: {enabled: true}\n"

const socks = "      inboundSocks: {listen: 0.0.0.0, portRange: {start: 20000, end: 20099}}\n"

const request = "{url: https://probe.example.com/generate_204, expectedStatus: 204}"

func TestS1EnvRelativeInputAndDefaults(t *testing.T) {
	t.Setenv("XRAY_API_ADDRESS", "127.0.0.1:8080")
	body := `subscriptions:
  - id: main
    input: subscription.txt
    fetchTimeoutMs: 7000
    filters:
      countryAllowlist: [' de ']
      labelIncludeRegex: '/Germany|Berlin/i'
    target:
      address: ${XRAY_API_ADDRESS}
      fixedOutbounds: [direct]
      observatorySubjectSelectorPrefix: ' x-observe- '
runtime: {}
logging: {}
resources: {}
`
	p := write(t, body)
	l, warnings, err := Load(p)
	if err != nil || len(warnings) != 0 {
		t.Fatal(err, warnings)
	}
	abs, _ := filepath.Abs(p)
	if l.Path != abs {
		t.Fatalf("path %q, want %q", l.Path, abs)
	}
	s := l.Config.Subscriptions[0]
	if s.Input != filepath.Join(filepath.Dir(abs), "subscription.txt") {
		t.Fatalf("input %q", s.Input)
	}
	if s.FetchTimeoutMs != 7000 || s.Target.Address != "127.0.0.1:8080" || s.Target.TimeoutMs != 5000 {
		t.Fatalf("sub %+v", s)
	}
	if !reflect.DeepEqual(s.Filters.CountryAllowlist, []string{"DE"}) {
		t.Fatalf("allowlist %v", s.Filters.CountryAllowlist)
	}
	if !reflect.DeepEqual(s.Filters.Label.IncludeSrc, []string{"/Germany|Berlin/i"}) || !s.Filters.Label.Match("🇩🇪 berlin") {
		t.Fatalf("filter %+v", s.Filters.Label)
	}
	if !reflect.DeepEqual(s.Target.FixedOutbounds, []string{"direct"}) || s.Target.FixedInbounds == nil || len(s.Target.FixedInbounds) != 0 {
		t.Fatalf("fixed %#v %#v", s.Target.FixedOutbounds, s.Target.FixedInbounds)
	}
	if s.Target.ObservatorySubjectSelectorPrefix != "x-observe-" {
		t.Fatalf("prefix %q", s.Target.ObservatorySubjectSelectorPrefix)
	}
	ss := l.Config.StatusServer
	if !ss.Enabled || ss.Listen != "127.0.0.1:9090" || ss.RemoteInstances == nil || len(ss.RemoteInstances) != 0 {
		t.Fatalf("statusServer %+v", ss)
	}
}

func TestEnvMissingIsError(t *testing.T) {
	mustFail(t, withSub(t, "    fetchUserAgent: ${SCD_TEST_NOPE}\n"), "failed to read config", `missing environment variable "SCD_TEST_NOPE"`)
	// Comments are not expanded.
	mustLoad(t, base+"# ${SCD_TEST_NOPE}\n")
}

func TestS2VisionUDP443Override(t *testing.T) {
	body := `subscriptions:
  - id: a
    input: ./a.txt
    target: {address: 127.0.0.1:8080, visionUdp443Override: true}
  - id: b
    input: ./b.txt
    target: {address: 127.0.0.1:8081}
runtime: {}
logging: {}
resources: {}
`
	l, _ := mustLoad(t, body)
	if !l.Config.Subscriptions[0].Target.VisionUDP443Override || l.Config.Subscriptions[1].Target.VisionUDP443Override {
		t.Fatalf("got %+v", l.Config.Subscriptions)
	}
}

func TestS3DuplicateTargetAddress(t *testing.T) {
	body := base + ""
	body = strings.Replace(body, "runtime: {}", `  - id: s2
    input: ./b.txt
    enabled: false
    target: {address: ' 127.0.0.1:8080 '}
runtime: {}`, 1)
	mustFail(t, body, `subscriptions: duplicate target address "127.0.0.1:8080"`)
}

func TestS4LegacyTargetsArray(t *testing.T) {
	body := replace(t, "    target:\n      address: 127.0.0.1:8080\n", "    targets:\n      - address: 127.0.0.1:8080\n")
	mustFail(t, body, "subscriptions.0.target: required", `subscriptions.0: unknown key "targets"`)
}

func TestS5MixedTargetAndTargets(t *testing.T) {
	mustFail(t, withSub(t, "    targets: []\n"), `subscriptions.0: unknown key "targets"`)
}

func TestS6InvalidDaemonCron(t *testing.T) {
	mustFail(t, replace(t, "runtime: {}", "runtime: {mode: daemon, schedule: not-a-cron}"), "runtime.schedule: ")
}

func TestS7UnsupportedCountry(t *testing.T) {
	msg := mustFail(t, withSub(t, "    filters: {countryAllowlist: [DE, zz, USA]}\n"),
		`subscriptions.0.filters.countryAllowlist.1: unsupported ISO2 country code "ZZ"`,
		`subscriptions.0.filters.countryAllowlist.2: must be a 2-letter ISO2 country code`)
	if strings.Count(msg, "\n  - ") != 2 {
		t.Fatalf("want 2 issues:\n%s", msg)
	}
}

func TestEmptyCountryAllowlistAllowed(t *testing.T) {
	l, _ := mustLoad(t, withSub(t, "    filters: {countryAllowlist: []}\n"))
	f := l.Config.Subscriptions[0].Filters
	if f == nil || len(f.CountryAllowlist) != 0 {
		t.Fatalf("filters %+v", f)
	}
}

func TestS8InvalidRegexLiteral(t *testing.T) {
	mustFail(t, withSub(t, "    filters: {labelIncludeRegex: '/(/i'}\n"), "subscriptions.0.filters.labelIncludeRegex: ")
}

func TestS9LegacySpeedtest(t *testing.T) {
	mustFail(t, withTarget(t, "      speedtest: {enabled: true}\n"), `subscriptions.0.target: unknown key "speedtest"`)
}

func TestS10MonitorAndRuntimeStateDefaults(t *testing.T) {
	l, _ := mustLoad(t, withTarget(t, "      monitor: {maxParallel: 4}\n"))
	m := l.Config.Subscriptions[0].Target.Monitor
	if m.Enabled || m.MaxParallel != 4 || m.Retry != (RetryConfig{Attempts: 1, DelayMs: 250}) || m.Request != nil {
		t.Fatalf("monitor %+v", m)
	}
	rs := l.Config.StatusServer.RuntimeState
	if !rs.Enabled || rs.IncludeRaw || rs.IncludeSecrets {
		t.Fatalf("runtimeState %+v", rs)
	}
}

func TestS11BalancerMonitor(t *testing.T) {
	valid := withTarget(t, `      balancerMonitor:
        enabled: true
        schedule: "*/1 * * * *"
        socks5: {host: xray, port: 1080}
        request: `+request+`
        remotePing: {enabled: true, url: https://ping.example.com/push, timeoutMs: 3000, viaSocks: true}
`)
	l, _ := mustLoad(t, valid)
	bm := l.Config.Subscriptions[0].Target.BalancerMonitor
	if !bm.Enabled || bm.Socks5 == nil || bm.Socks5.Port != 1080 || bm.Request == nil || bm.Request.Method != http.MethodGet || bm.Request.TimeoutMs != 5000 {
		t.Fatalf("balancerMonitor %+v", bm)
	}
	if rp := bm.RemotePing; !rp.Enabled || rp.TimeoutMs != 3000 || !rp.ViaSocks || rp.URL != "https://ping.example.com/push" {
		t.Fatalf("remotePing %+v", rp)
	}
	mustFail(t, withTarget(t, "      balancerMonitor: {enabled: true}\n"),
		"subscriptions.0.target.balancerMonitor.schedule: required when balancerMonitor.enabled is true",
		"subscriptions.0.target.balancerMonitor.socks5: required when balancerMonitor.enabled is true",
		"subscriptions.0.target.balancerMonitor.request: required when balancerMonitor.enabled is true")
}

func TestS12Retry(t *testing.T) {
	l, _ := mustLoad(t, withTarget(t, "      monitor: {retry: {attempts: 2, delayMs: 100}}\n      balancerMonitor: {retry: {attempts: 3, delayMs: 150}}\n"))
	tg := l.Config.Subscriptions[0].Target
	if tg.Monitor.Retry != (RetryConfig{2, 100}) || tg.BalancerMonitor.Retry != (RetryConfig{3, 150}) {
		t.Fatalf("retry %+v %+v", tg.Monitor.Retry, tg.BalancerMonitor.Retry)
	}
	mustFail(t, withTarget(t, "      monitor: {retry: {attempts: 0}}\n"), "subscriptions.0.target.monitor.retry.attempts: must be at least 1")
	mustFail(t, withTarget(t, "      balancerMonitor: {retry: {delayMs: -1}}\n"), "subscriptions.0.target.balancerMonitor.retry.delayMs: must be at least 0")
}

func TestS13RemotePing(t *testing.T) {
	enabled := "      balancerMonitor:\n        enabled: true\n        schedule: '@hourly'\n        socks5: {host: x, port: 1}\n        request: " + request + "\n"
	mustFail(t, withTarget(t, enabled+"        remotePing: {enabled: true}\n"),
		"subscriptions.0.target.balancerMonitor.remotePing.url: required when remotePing.enabled is true")
	mustFail(t, withTarget(t, enabled+"        remotePing: {timeoutMs: 0}\n"),
		"subscriptions.0.target.balancerMonitor.remotePing.timeoutMs: must be at least 1")
	mustFail(t, withTarget(t, "      balancerMonitor: {remotePing: {enabled: true, url: 'https://p.example.com/'}}\n"),
		"subscriptions.0.target.balancerMonitor.remotePing.enabled: requires balancerMonitor.enabled")
	mustFail(t, withTarget(t, "      monitor: {remotePing: {}}\n"), `subscriptions.0.target.monitor: unknown key "remotePing"`)
}

func TestS14LegacySuccessGet(t *testing.T) {
	mustFail(t, withTarget(t, "      balancerMonitor: {successGet: {}}\n"), `unknown key "successGet"`)
}

func TestMonitorCrossFieldRules(t *testing.T) {
	mustFail(t, withTarget(t, "      monitor: {enabled: true}\n"),
		"subscriptions.0.target.monitor.schedule: required when monitor.enabled is true",
		"subscriptions.0.target.monitor.request: required when monitor.enabled is true",
		"subscriptions.0.target.inboundSocks: required when monitor.enabled is true",
		"subscriptions.0.target.monitor.enabled: requires resources.outbounds.enabled, resources.inbounds.enabled and resources.routing.enabled")
	ok := strings.Replace(withTarget(t, socks+"      monitor: {enabled: true, schedule: '*/1 * * * *', request: "+request+"}\n"), "resources: {}\n", allResources, 1)
	l, _ := mustLoad(t, ok)
	if r := l.Config.Subscriptions[0].Target.Monitor.Request; r == nil || r.ExpectedStatus != 204 {
		t.Fatalf("request %+v", r)
	}
}

func TestResourcesRules(t *testing.T) {
	mustFail(t, replace(t, "resources: {}", "resources: {outbounds: {enabled: false}, inbounds: {enabled: true}, routing: {enabled: true}}"),
		"resources.inbounds.enabled: requires resources.outbounds.enabled",
		"resources.routing.enabled: requires resources.outbounds.enabled and resources.inbounds.enabled",
		"subscriptions.0.target.inboundSocks: required when resources.inbounds.enabled or resources.routing.enabled is true")
}

func TestInboundSocksPortRange(t *testing.T) {
	mustFail(t, withTarget(t, "      inboundSocks: {listen: 0.0.0.0, portRange: {start: 20100, end: 20000}}\n"),
		"subscriptions.0.target.inboundSocks.portRange.end: must be greater than or equal to start")
	mustFail(t, withTarget(t, "      inboundSocks: {listen: 0.0.0.0, portRange: {start: 20000, end: 70000}}\n"),
		"subscriptions.0.target.inboundSocks.portRange.end: must be at most 65535")
}

func TestInboundSocksListen(t *testing.T) {
	for _, listen := range []string{"127.0.0.1", "0.0.0.0", "'::'", "'::1'", "localhost"} {
		mustLoad(t, withTarget(t, "      inboundSocks: {listen: "+listen+", portRange: {start: 20000, end: 20099}}\n"))
	}
	for _, listen := range []string{"socks.example.test", "LOCALHOST", "'[::1]'", "127.0.0.1:1080"} {
		mustFail(t, withTarget(t, "      inboundSocks: {listen: "+listen+", portRange: {start: 20000, end: 20099}}\n"),
			`subscriptions.0.target.inboundSocks.listen: must be an IP address or "localhost" (Xray cannot listen on a domain)`)
	}
}

func TestStatusServerRules(t *testing.T) {
	mustFail(t, base+"statusServer: {listen: ''}\n", "statusServer.listen: required when statusServer.enabled is true")
	mustFail(t, base+"statusServer: {listen: 'localhost'}\n", "statusServer.listen: ")
	body := base + `statusServer:
  remoteInstances:
    - {id: a, apiUrl: 'http://a.example.com:9090', schedule: '*/1 * * * *'}
    - {id: a, apiUrl: 'not a url', schedule: 'nope'}
`
	mustFail(t, body,
		`statusServer.remoteInstances.1.id: duplicate remote instance id "a"`,
		"statusServer.remoteInstances.1.apiUrl: invalid URL",
		"statusServer.remoteInstances.1.schedule: ")
}

func TestListenEmptyWhenDisabledOK(t *testing.T) {
	l, _ := mustLoad(t, base+"statusServer: {enabled: false, listen: ''}\n")
	if l.Config.StatusServer.Enabled || l.Config.StatusServer.Listen != "" {
		t.Fatalf("statusServer %+v", l.Config.StatusServer)
	}
}

func TestAtLeastOneEnabled(t *testing.T) {
	mustFail(t, withSub(t, "    enabled: false\n"), "subscriptions: at least one subscription must be enabled")
}

func TestRequiredSections(t *testing.T) {
	mustFail(t, "", "subscriptions: required", "runtime: required", "logging: required", "resources: required")
}

func TestAllowInsecureDefaultDeny(t *testing.T) {
	l, _ := mustLoad(t, base)
	if got := l.Config.Subscriptions[0].AllowInsecure; got != "deny" {
		t.Fatalf("allowInsecure %q", got)
	}
	l, _ = mustLoad(t, withSub(t, "    allowInsecure: allow\n"))
	if got := l.Config.Subscriptions[0].AllowInsecure; got != "allow" {
		t.Fatalf("allowInsecure %q", got)
	}
}

func TestAllowInsecureRejectsOther(t *testing.T) {
	mustFail(t, withSub(t, "    allowInsecure: true\n"), "subscriptions.0.allowInsecure: expected string, got boolean")
	mustFail(t, withSub(t, "    allowInsecure: yes\n"), "subscriptions.0.allowInsecure: must be one of deny|allow")
}

func TestBalancerSelectionDefaults(t *testing.T) {
	l, _ := mustLoad(t, base)
	if bs := l.Config.Subscriptions[0].Target.BalancerSelection; bs != (BalancerSelectionConfig{Observatory: true}) {
		t.Fatalf("balancerSelection %+v", bs)
	}
	mustFail(t, withTarget(t, "      balancerSelection: {strategy: fastest}\n"),
		"subscriptions.0.target.balancerSelection.strategy: must be one of leastPing|leastLoad|random|roundRobin")
	mustFail(t, withTarget(t, "      balancerSelection: {tag: x}\n"), `subscriptions.0.target.balancerSelection: unknown key "tag"`)
}

func TestBalancerSelectionRequiresTagAndSchedule(t *testing.T) {
	mustFail(t, withTarget(t, "      balancerSelection: {enabled: true}\n"),
		"subscriptions.0.target.balancerSelection.balancerTag: required when balancerSelection.enabled is true",
		"subscriptions.0.target.balancerSelection.schedule: required when balancerSelection.enabled is true")
	mustFail(t, withTarget(t, "      balancerSelection: {enabled: true, balancerTag: b, schedule: 'nope'}\n"),
		"subscriptions.0.target.balancerSelection.schedule: ")
}

func TestBalancerSelectionSixFieldCron(t *testing.T) {
	l, warnings := mustLoad(t, withTarget(t, "      observatorySubjectSelectorPrefix: x-observe-\n"+
		"      balancerSelection: {enabled: true, balancerTag: x-balancer, schedule: '*/15 * * * * *', observatory: false, strategy: leastPing}\n"))
	if len(warnings) != 0 {
		t.Fatalf("warnings %v", warnings)
	}
	want := BalancerSelectionConfig{Enabled: true, BalancerTag: "x-balancer", Schedule: "*/15 * * * * *", Strategy: "leastPing"}
	if bs := l.Config.Subscriptions[0].Target.BalancerSelection; bs != want {
		t.Fatalf("balancerSelection %+v", bs)
	}
}

func TestBalancerSelectionWarnsWithoutPrefix(t *testing.T) {
	_, warnings := mustLoad(t, withTarget(t, "      balancerSelection: {enabled: true, balancerTag: x-balancer, schedule: '*/15 * * * * *'}\n"))
	want := []string{"subscriptions.0.target.balancerSelection: balancerSelection is enabled without observatorySubjectSelectorPrefix; scd tunnels are unlikely to match the balancer selector"}
	if !reflect.DeepEqual(warnings, want) {
		t.Fatalf("warnings %q", warnings)
	}
}

func TestScheduleCronFollowsLibrary(t *testing.T) {
	for _, bad := range []string{"0 0 L * *", "0 0 31 2 *", "   "} {
		mustFail(t, replace(t, "runtime: {}", "runtime: {mode: daemon, schedule: '"+bad+"'}"), "runtime.schedule: ")
	}
	for _, good := range []string{"0 0 ? * *", "@every 5m", "*/15 * * * * *", "@midnight"} {
		mustLoad(t, replace(t, "runtime: {}", "runtime: {mode: daemon, schedule: '"+good+"'}"))
	}
}

func TestFetchUserAgentOptional(t *testing.T) {
	l, _ := mustLoad(t, base)
	if ua := l.Config.Subscriptions[0].FetchUserAgent; ua != "" {
		t.Fatalf("fetchUserAgent %q", ua)
	}
	l, _ = mustLoad(t, withSub(t, "    fetchUserAgent: ' Happ/1.0 '\n"))
	if ua := l.Config.Subscriptions[0].FetchUserAgent; ua != "Happ/1.0" {
		t.Fatalf("fetchUserAgent %q", ua)
	}
	mustFail(t, withSub(t, "    fetchUserAgent: '  '\n"), "subscriptions.0.fetchUserAgent: must not be empty")
}

func TestFetchHeaders(t *testing.T) {
	t.Setenv("HW", "s3cr3t")
	l, _ := mustLoad(t, withSub(t, "    fetchHeaders: {x-hwid: \"${HW}\"}\n"))
	if got := l.Config.Subscriptions[0].FetchHeaders.Plain()["x-hwid"]; got != "s3cr3t" {
		t.Fatalf("x-hwid %q", got)
	}
	l, _ = mustLoad(t, withSub(t, "    fetchHeaders: {}\n"))
	if h := l.Config.Subscriptions[0].FetchHeaders; h != nil {
		t.Fatalf("empty fetchHeaders %v", h)
	}
	mustFail(t, withSub(t, "    fetchHeaders: {User-Agent: x}\n"), "subscriptions.0.fetchHeaders.User-Agent: use fetchUserAgent instead")
	mustFail(t, withSub(t, "    fetchHeaders: {\"bad name\": x}\n"), "subscriptions.0.fetchHeaders.bad name: invalid header name")
	mustFail(t, withSub(t, "    fetchHeaders: {x-a: 1}\n"), "subscriptions.0.fetchHeaders.x-a: expected string")
}

func TestLoggingFormatEnum(t *testing.T) {
	l, _ := mustLoad(t, replace(t, "logging: {}", "logging: {format: pretty, level: debug}"))
	if l.Config.Logging != (LoggingConfig{Level: "debug", Format: "pretty"}) {
		t.Fatalf("logging %+v", l.Config.Logging)
	}
	mustFail(t, replace(t, "logging: {}", "logging: {format: text}"), "logging.format: must be one of json|pretty")
}

func TestLookaheadRegexRejected(t *testing.T) {
	mustFail(t, withSub(t, "    filters:\n      labelIncludeRegex: '/^(?!.*Extra).*$/i'\n      labelExcludeRegex: [ok, '(?<=a)b']\n"),
		"subscriptions.0.filters.labelIncludeRegex: lookahead, lookbehind and backreferences are not supported",
		"subscriptions.0.filters.labelExcludeRegex.1: lookahead")
}

func TestLabelExcludeRegex(t *testing.T) {
	l, _ := mustLoad(t, withSub(t, "    filters: {labelIncludeRegex: [/Extra/i, /WhiteList/i], labelExcludeRegex: /Test/}\n"))
	f := l.Config.Subscriptions[0].Filters.Label
	if !f.Match("extra whitelist") || f.Match("extra") || f.Match("Extra WhiteList Test") {
		t.Fatalf("filter %+v", f)
	}
}

func TestAllCrossFieldIssuesReported(t *testing.T) {
	body := replace(t, "runtime: {}\nlogging: {}\nresources: {}\n", "runtime: {mode: daemon}\nlogging: {}\nresources: {inbounds: {enabled: true}, outbounds: {enabled: false}}\n")
	mustFail(t, body,
		`runtime.schedule: required when runtime.mode is "daemon"`,
		"resources.inbounds.enabled: requires resources.outbounds.enabled")
}

func TestUnknownKeysPolicy(t *testing.T) {
	// strip objects: root, filters, runtime, logging, resources, statusServer.
	mustLoad(t, base+"extra: 1\nstatusServer: {foo: 1, runtimeState: {bar: 2}}\n")
	mustFail(t, withSub(t, "    foo: 1\n"), `subscriptions.0: unknown key "foo"`)
	mustFail(t, base+"statusServer: {remoteInstances: [{id: a, apiUrl: 'http://a.example.com', schedule: '@hourly', x: 1}]}\n",
		`statusServer.remoteInstances.0: unknown key "x"`)
}

func TestIntTypes(t *testing.T) {
	mustFail(t, withSub(t, "    fetchTimeoutMs: 0\n"), "subscriptions.0.fetchTimeoutMs: must be at least 1")
	mustFail(t, withSub(t, "    fetchTimeoutMs: '5000'\n"), "subscriptions.0.fetchTimeoutMs: expected integer, got string")
	mustFail(t, withTarget(t, "      monitor: {request: {url: 'https://a.example.com', expectedStatus: 600}}\n"),
		"subscriptions.0.target.monitor.request.expectedStatus: must be at most 599")
	mustFail(t, withTarget(t, "      monitor: {request: {url: 'mailto:a@b'}}\n"),
		"subscriptions.0.target.monitor.request.url: invalid URL",
		"subscriptions.0.target.monitor.request.expectedStatus: required")
}

func TestReadErrorHasPath(t *testing.T) {
	_, _, err := Load("nope.yml")
	if err == nil || !strings.HasPrefix(err.Error(), "failed to read config nope.yml: ") {
		t.Fatalf("err %v", err)
	}
}

func TestFiltersJSON(t *testing.T) {
	l, _ := mustLoad(t, withSub(t, "    filters: {countryAllowlist: [de], labelExcludeRegex: '/Extra|WhiteList/i'}\n"))
	b, err := jsonout.Marshal(l.Config.Subscriptions[0].Filters)
	if err != nil {
		t.Fatal(err)
	}
	want := `{
  "countryAllowlist": [
    "DE"
  ],
  "labelExcludeRegex": [
    "/Extra|WhiteList/i"
  ]
}`
	if string(b) != want {
		t.Fatalf("got %s", b)
	}
}

func TestSCDRealShapeGoFormValid(t *testing.T) {
	l, warnings, err := Load(realShapeGo)
	if err != nil || len(warnings) != 0 {
		t.Fatal(err, warnings)
	}
	c := l.Config
	if len(c.Subscriptions) != 2 || c.Runtime.Mode != "daemon" || c.StatusServer.InstanceID != "paper" || c.StatusServer.Listen != "172.18.0.8:8080" {
		t.Fatalf("config %+v", c)
	}
	rs := c.StatusServer.RuntimeState
	if !rs.Enabled || rs.IncludeRaw || rs.IncludeSecrets {
		t.Fatalf("runtimeState %+v", rs)
	}
	for _, s := range c.Subscriptions {
		f := s.Filters
		if f == nil || len(f.CountryAllowlist) != 24 || !reflect.DeepEqual(f.Label.ExcludeSrc, []string{"/Extra|WhiteList/i"}) || f.Label.IncludeSrc != nil {
			t.Fatalf("%s filters %+v", s.ID, f)
		}
		tg := s.Target
		if tg.ObservatorySubjectSelectorPrefix != "x-observe-" || tg.VisionUDP443Override || tg.InboundSocks == nil || tg.InboundSocks.PortRange.End != 21000 {
			t.Fatalf("%s target %+v", s.ID, tg)
		}
		bm := tg.BalancerMonitor
		if !bm.Enabled || bm.Socks5 == nil || bm.Socks5.Port != 1080 || !bm.RemotePing.ViaSocks || bm.Retry != (RetryConfig{3, 500}) {
			t.Fatalf("%s balancerMonitor %+v", s.ID, bm)
		}
		if tg.Monitor.MaxParallel != 50 || tg.Monitor.Request == nil {
			t.Fatalf("%s monitor %+v", s.ID, tg.Monitor)
		}
	}
	if got := c.Subscriptions[1].Target.FixedInbounds; !reflect.DeepEqual(got, []string{"socks-proxy", "wg-proxy", "ru-zone"}) {
		t.Fatalf("fixedInbounds %v", got)
	}
}

func TestDisabledScheduleStillValidated(t *testing.T) {
	mustFail(t, withTarget(t, "      monitor: {schedule: '61 * * * *'}\n      balancerMonitor: {schedule: nope}\n      balancerSelection: {schedule: '0 0 L * *'}\n"),
		"subscriptions.0.target.monitor.schedule: ",
		"subscriptions.0.target.balancerMonitor.schedule: ",
		"subscriptions.0.target.balancerSelection.schedule: ")
	mustFail(t, replace(t, "runtime: {}", "runtime: {schedule: nope}"), "runtime.schedule: ")
	mustLoad(t, withTarget(t, "      monitor: {schedule: '*/1 * * * *'}\n"))
}
