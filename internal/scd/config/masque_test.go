package config

import (
	"strings"
	"testing"
)

// mqBase is wgBase with the wireguard source as a masque source.
func mqBase(t *testing.T) string {
	t.Helper()
	s := wgReplace(t, "        type: wireguard\n", "        type: masque\n")
	return strings.Replace(s, "        wireguard:\n          endpointDomainStrategy: UseIPv4\n", "", 1)
}

func TestMasqueSourceLoads(t *testing.T) {
	l, _ := mustLoad(t, mqBase(t))
	src := l.Config.Subscriptions[0].Sources[1]
	if src.Type != SourceMasque || src.MaxActive != 2 || src.Selection == nil || src.WireGuard != nil ||
		src.Filters.EntryCountryAllowlist[0] != "CH" {
		t.Fatalf("source %+v", src)
	}
}

func TestMasqueSourceErrors(t *testing.T) {
	const p = "subscriptions.0.sources.1."
	base := mqBase(t)
	with := func(lines string) string {
		return strings.Replace(base, "        input: /app/wgp/wg\n", "        input: /app/wgp/wg\n"+lines, 1)
	}
	for _, k := range []string{
		"format: auto", "fetchTimeoutMs: 5000", "allowInsecure: deny", "fetchUserAgent: x",
		"fetchHeaders: {x-a: b}", "wireguard: {endpointDomainStrategy: UseIPv4}",
	} {
		key, _, _ := strings.Cut(k, ":")
		mustFail(t, with("        "+k+"\n"), p+key+`: not allowed for type "masque"`)
	}
	mustFail(t, strings.Replace(base, "/app/wgp/wg", "./mq", 1), p+`input: must be an absolute file path for type "masque"`)
	mustFail(t, strings.Replace(base, "        maxActive: 2\n", "", 1), p+"maxActive: required")
	mustFail(t, strings.Replace(base, "  routing: {enabled: true}\n", "  routing: {enabled: false}\n", 1),
		p+`type: "masque" requires resources.inbounds.enabled and resources.routing.enabled`)
}
