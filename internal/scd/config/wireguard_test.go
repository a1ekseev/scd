package config

import (
	"encoding/json"
	"strings"
	"testing"
)

// wgBase has a vless and a wireguard source on a tunnelled target.
const wgBase = `subscriptions:
  - id: s1
    sources:
      - id: vless
        type: vless
        input: ./sub.txt
        portRange: {start: 20000, end: 20009}
      - id: wg
        type: wireguard
        input: /app/wgp/wg
        portRange: {start: 20100, end: 20102}
        maxActive: 2
        selection:
          strategy: score
          switchMarginPercent: 20
          switchConfirmChecks: 2
          deadPairCooldownMinutes: 10
          replaceAfterFailures: 2
        filters:
          countryAllowlist: [nl]
          entryCountryAllowlist: [CH]
        wireguard:
          endpointDomainStrategy: UseIPv4
    target:
      address: 127.0.0.1:8080
      inboundSocks: {listen: 127.0.0.1}
runtime: {}
logging: {}
resources:
  inbounds: {enabled: true}
  routing: {enabled: true}
`

func wgReplace(t *testing.T, old, repl string) string {
	t.Helper()
	if !strings.Contains(wgBase, old) {
		t.Fatalf("fragment %q not in wgBase", old)
	}
	return strings.Replace(wgBase, old, repl, 1)
}

// wgWith adds lines to the wireguard source.
func wgWith(t *testing.T, lines string) string {
	return wgReplace(t, "        input: /app/wgp/wg\n", "        input: /app/wgp/wg\n"+lines)
}

// vlessWith adds lines to the vless source.
func vlessWith(t *testing.T, lines string) string {
	return wgReplace(t, "        input: ./sub.txt\n", "        input: ./sub.txt\n"+lines)
}

func TestWireGuardSourceLoads(t *testing.T) {
	l, _ := mustLoad(t, wgBase)
	src := l.Config.Subscriptions[0].Sources[1]
	if src.Type != SourceWireGuard || src.Input != "/app/wgp/wg" || src.MaxActive != 2 ||
		src.Format != "" || src.FetchTimeoutMs != 0 || src.AllowInsecure != "" {
		t.Fatalf("source %+v", src)
	}
	if s := src.Selection; s == nil || *s != (SelectionConfig{"score", 20, 2, 10, 2}) {
		t.Fatalf("selection %+v", s)
	}
	if src.WireGuard == nil || src.WireGuard.EndpointDomainStrategy != "UseIPv4" {
		t.Fatalf("wireguard %+v", src.WireGuard)
	}
	if f := src.Filters; f.CountryAllowlist[0] != "NL" || f.EntryCountryAllowlist[0] != "CH" {
		t.Fatalf("filters %+v", f)
	}
}

func TestWireGuardSourceErrors(t *testing.T) {
	const p = "subscriptions.0.sources.1."
	for _, k := range []string{"format: auto", "fetchTimeoutMs: 5000", "allowInsecure: deny", "fetchUserAgent: x", "fetchHeaders: {x-a: b}"} {
		key, _, _ := strings.Cut(k, ":")
		mustFail(t, wgWith(t, "        "+k+"\n"), p+key+`: not allowed for type "wireguard"`)
	}
	for _, input := range []string{"./wg", "-", "https://example.com/wg"} {
		mustFail(t, wgReplace(t, "/app/wgp/wg", "'"+input+"'"), p+`input: must be an absolute file path for type "wireguard"`)
	}
	mustFail(t, wgReplace(t, "        maxActive: 2\n", ""), p+"maxActive: required")
	mustFail(t, wgReplace(t, "        maxActive: 2\n", "        maxActive: 0\n"), p+"maxActive: must be at least 1")
	mustFail(t, wgReplace(t, "        maxActive: 2\n", "        maxActive: 4\n"), p+"portRange: has 3 ports, fewer than maxActive 4")
	mustFail(t, wgReplace(t, "          strategy: score\n", "          strategy: ping\n"), p+"selection.strategy: must be one of score|load|label")
	mustFail(t, wgReplace(t, "          switchMarginPercent: 20\n", "          switchMarginPercent: 101\n"), p+"selection.switchMarginPercent: must be at most 100")
	mustFail(t, wgReplace(t, "          switchConfirmChecks: 2\n", "          switchConfirmChecks: 11\n"), p+"selection.switchConfirmChecks: must be at most 10")
	mustFail(t, wgReplace(t, "          deadPairCooldownMinutes: 10\n", "          deadPairCooldownMinutes: 3\n"), p+"selection.deadPairCooldownMinutes: must be at least 4")
	mustFail(t, wgReplace(t, "          replaceAfterFailures: 2\n", ""), p+"selection.replaceAfterFailures: required")
	mustFail(t, wgReplace(t, "          endpointDomainStrategy: UseIPv4\n", "          endpointDomainStrategy: UseIPv5\n"),
		p+"wireguard.endpointDomainStrategy: must be one of AsIs|UseIP|UseIPv4|UseIPv6|UseIPv4v6|UseIPv6v4")
	mustFail(t, wgReplace(t, "        wireguard:\n          endpointDomainStrategy: UseIPv4\n", ""), p+"wireguard: required")
	mustFail(t, wgReplace(t, "  routing: {enabled: true}\n", ""),
		p+`type: "wireguard" requires resources.inbounds.enabled and resources.routing.enabled`)
}

func TestVLESSSourceRejectsWireGuardKeys(t *testing.T) {
	const p = "subscriptions.0.sources.0."
	for _, k := range []string{"maxActive: 1", "selection: {strategy: label}", "wireguard: {endpointDomainStrategy: AsIs}"} {
		key, _, _ := strings.Cut(k, ":")
		mustFail(t, vlessWith(t, "        "+k+"\n"), p+key+`: not allowed for type "vless"`)
	}
	mustFail(t, vlessWith(t, "        filters: {entryCountryAllowlist: [NL]}\n"), p+`filters.entryCountryAllowlist: not allowed for type "vless"`)
}

// sources[].monitor is accepted on every type.
func TestSourceMonitor(t *testing.T) {
	mon := "        monitor:\n          request: {timeoutMs: 10000}\n          retry: {attempts: 2, delayMs: 0}\n"
	l, _ := mustLoad(t, vlessWith(t, mon))
	m := l.Config.Subscriptions[0].Sources[0].Monitor
	if m == nil || m.Request.TimeoutMs != 10000 || m.Retry.Attempts != 2 || m.Retry.DelayMs == nil || *m.Retry.DelayMs != 0 {
		t.Fatalf("monitor %+v", m)
	}
	l, _ = mustLoad(t, wgWith(t, "        monitor: {retry: {attempts: 3}}\n"))
	if m := l.Config.Subscriptions[0].Sources[1].Monitor; m.Request != nil || m.Retry.Attempts != 3 || m.Retry.DelayMs != nil {
		t.Fatalf("monitor %+v", m)
	}
	mustFail(t, wgWith(t, "        monitor: {request: {url: x}}\n"),
		`subscriptions.0.sources.1.monitor.request: unknown key "url"`, "subscriptions.0.sources.1.monitor.request.timeoutMs: required")
	mustFail(t, wgWith(t, "        monitor: {retry: {attempts: 0}}\n"), "subscriptions.0.sources.1.monitor.retry.attempts: must be at least 1")
}

// print-config shows the wireguard keys and no vless-only defaults.
func TestWireGuardSourcePrinted(t *testing.T) {
	l, _ := mustLoad(t, wgBase)
	p := l.Config.Subscriptions[0].Sources[1].Printed(false)
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"id":"wg","type":"wireguard","input":"/app/wgp/wg","enabled":true,"maxInputBytes":67108864,` +
		`"portRange":{"start":20100,"end":20102},"filters":{"countryAllowlist":["NL"],"entryCountryAllowlist":["CH"]},"maxActive":2,` +
		`"selection":{"strategy":"score","switchMarginPercent":20,"switchConfirmChecks":2,"deadPairCooldownMinutes":10,"replaceAfterFailures":2},` +
		`"wireguard":{"endpointDomainStrategy":"UseIPv4"}}`
	if string(b) != want {
		t.Fatalf("printed\n%s\nwant\n%s", b, want)
	}
}
