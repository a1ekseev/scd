package syncer

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/a1ekseev/scd/internal/scd/config"
	"github.com/a1ekseev/scd/internal/scd/state"
)

// wgFixtureLines are the lines of the wireguard fixture; private keys are
// 0x11..0x19 repeated, the preshared key 0x35.
func wgFixtureLines(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile("../../../docker/fixtures/wireguard/candidates.txt")
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

// wgSecretForms are the fixture's secret keys in every encoding scd or
// Xray uses: std and raw URL base64, hex.
func wgSecretForms() []string {
	var out []string
	for _, b := range []byte{0x11, 0x12, 0x14, 0x15, 0x16, 0x17, 0x18, 0x19, 0x35} {
		k := bytes.Repeat([]byte{b}, 32)
		out = append(out, base64.StdEncoding.EncodeToString(k), base64.RawURLEncoding.EncodeToString(k), hex.EncodeToString(k))
	}
	return out
}

func assertNoSecrets(t *testing.T, what, s string) {
	t.Helper()
	for _, k := range wgSecretForms() {
		if strings.Contains(s, k) {
			t.Errorf("%s holds the secret key %s", what, k)
		}
	}
}

// writeCandidates writes lines (1-based fixture line numbers) to path with
// mode 0600.
func writeCandidates(t *testing.T, path string, all []string, lines ...int) {
	t.Helper()
	var b strings.Builder
	for _, n := range lines {
		b.WriteString(all[n-1] + "\n")
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
}

// wgSub is a subscription with one wireguard source "wg" reading path:
// maxActive 2 by score on ports 20100..20103; no switch for a better
// candidate below a 95 % margin.
func wgSub(path string) config.SubscriptionConfig {
	t := target(addrA)
	t.InboundSocks = &config.InboundSocksConfig{Listen: "127.0.0.1"}
	src := config.SourceConfig{
		ID: "wg", Type: config.SourceWireGuard, Input: path, Enabled: true, MaxInputBytes: 1 << 20,
		PortRange: &config.PortRange{Start: 20100, End: 20103}, MaxActive: 2,
		Selection: &config.SelectionConfig{Strategy: config.StrategyScore, SwitchMarginPercent: 95, SwitchConfirmChecks: 1, DeadPairCooldownMinutes: 4, ReplaceAfterFailures: 1},
		WireGuard: &config.WireGuardConfig{EndpointDomainStrategy: "UseIPv4"},
	}
	return config.SubscriptionConfig{ID: "sub", Enabled: true, Sources: []config.SourceConfig{src}, Target: t}
}

// peakOutbounds replays calls and returns the largest number of outbounds
// of source id that existed at once, after any call; the calls at the
// indexes failed (injected failures) changed nothing.
func peakOutbounds(calls []string, id string, failed ...int) int {
	have := map[string]bool{}
	peak := 0
	for i, c := range calls {
		method, tag, _ := strings.Cut(c, ":")
		if slices.Contains(failed, i) || !strings.HasPrefix(tag, "x-observe-"+id+"-") && !strings.HasPrefix(tag, id+"-") {
			continue
		}
		switch method {
		case "AddOutbound":
			have[tag] = true
		case "RemoveOutbound":
			delete(have, tag)
		}
		peak = max(peak, len(have))
	}
	return peak
}

func activeTags(t *testing.T, f *fixture) (tags []string, ports []int) {
	t.Helper()
	f.s.Memory.View(state.Key("sub", addrA), func(tg *state.Target) {
		for _, tn := range tg.Tunnels {
			tags = append(tags, tn.Mapping.BaseTunnelID)
			ports = append(ports, tn.Mapping.Port)
			if tn.Mapping.Protocol != "wireguard" {
				t.Errorf("tunnel %s protocol %q", tn.Mapping.BaseTunnelID, tn.Mapping.Protocol)
			}
		}
	})
	return tags, ports
}

// The owner invariant: Xray never has more than maxActive outbounds of a
// wireguard source, at any RPC of a sync or of its rollback.
func TestWireGuardSourceMaxActive(t *testing.T) {
	all := wgFixtureLines(t)
	path := filepath.Join(t.TempDir(), "wg")
	writeCandidates(t, path, all, 1, 2, 3, 4, 5, 6, 7, 8, 10)
	f := newSourcesFixture(t, wgSub(path), nil)
	srv := f.x.servers[addrA]

	r := f.run(t)
	if r.Failed != 0 || r.Added != 6 {
		t.Fatalf("report %+v", r)
	}
	// By score: line 2 (1.2), line 3 has the key of line 2, line 1 (1.5).
	tags, ports := activeTags(t, f)
	if len(tags) != 2 || !strings.HasPrefix(tags[0], "wg-nl-amsterdam-wg-") || !slices.Equal(ports, []int{20100, 20101}) {
		t.Fatalf("active %v ports %v", tags, ports)
	}
	if got := srv.OutboundTags(); len(got) != 2 || got[0] != "x-observe-"+tags[1] && got[0] != "x-observe-"+tags[0] {
		t.Fatalf("outbounds %v", got)
	}
	f.s.Memory.View(state.Key("sub", addrA), func(tg *state.Target) {
		if n := tg.Sources["wg"].CandidateCount; n != 7 {
			t.Errorf("candidateCount %d", n)
		}
	})

	// Line 2 is gone: line 3 (its key, score 1.3) takes the first slot.
	writeCandidates(t, path, all, 1, 3, 4, 5, 6, 7)
	if r := f.run(t); r.Failed != 0 {
		t.Fatalf("second report %+v", r)
	}
	if next, _ := activeTags(t, f); next[1] != tags[1] || next[0] == tags[0] {
		t.Fatalf("active %v, before %v", next, tags)
	}

	// A failed add rolls back without ever exceeding maxActive.
	n := len(srv.Calls())
	srv.FailNext("AddOutbound", errors.New("boom"))
	writeCandidates(t, path, all, 1, 4, 5, 6, 7)
	if r := f.run(t); r.Failed == 0 {
		t.Fatalf("third report %+v", r)
	}
	failed := n + slices.IndexFunc(srv.Calls()[n:], func(c string) bool { return strings.HasPrefix(c, "AddOutbound:") })
	if r := f.run(t); r.Failed != 0 {
		t.Fatalf("fourth report %+v", r)
	}
	if peak := peakOutbounds(srv.Calls(), "wg", failed); peak != 2 {
		t.Fatalf("peak %d outbounds of the source, maxActive 2; calls %v", peak, srv.Calls())
	}
	if got := srv.OutboundTags(); len(got) != 2 {
		t.Fatalf("outbounds %v", got)
	}

	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	assertNoSecrets(t, "the sync report", string(b))
	assertNoSecrets(t, "the log", f.logs.String())
}

func TestWireGuardInputRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wg")
	writeCandidates(t, path, wgFixtureLines(t), 1)
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	f := newSourcesFixture(t, wgSub(path), nil)
	if r := f.run(t); r.Failed != 3 {
		t.Fatalf("report %+v", r)
	}
	if e := f.logs.events(t, "subscription_fetch_failed"); len(e) != 1 || e[0]["reasonCode"] != "wireguard_input_refused" {
		t.Fatalf("subscription_fetch_failed %v", e)
	}
	if calls := f.x.servers[addrA].Calls(); slices.ContainsFunc(calls, func(c string) bool { return strings.HasPrefix(c, "Add") }) {
		t.Fatalf("calls %v", calls)
	}
}

// A candidate line in a vless source is skipped with its key masked.
func TestCandidateLineInVLESSSourceMasked(t *testing.T) {
	line := wgFixtureLines(t)[0]
	text := vienna + "\n" + line + "\n" + strings.Replace(line, "wireguard://", "MASQUE://", 1)
	f := newSourcesFixture(t, socksSub("sub", "/in", target(addrA), 20000, 20010), map[string]string{"/in": text})
	r := f.run(t)
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	assertNoSecrets(t, "the sync report", string(b))
	assertNoSecrets(t, "the log", f.logs.String())
	var raws []string
	for _, s := range r.Skipped {
		raws = append(raws, s.Raw)
	}
	if !slices.Equal(raws, []string{"wireguard://<redacted>", "masque://<redacted>"}) {
		t.Fatalf("skipped raw %q", raws)
	}
}

// After a restart with a smaller maxActive, a failed apply does not bring
// back the larger set: from the first add on, at most the new maxActive.
func TestWireGuardSmallerMaxActiveAfterRestart(t *testing.T) {
	all := wgFixtureLines(t)
	path := filepath.Join(t.TempDir(), "wg")
	writeCandidates(t, path, all, 1, 2, 4, 5)
	sub := wgSub(path)
	sub.Sources[0].MaxActive = 3
	f := newSourcesFixture(t, sub, nil)
	srv := f.x.servers[addrA]
	if r := f.run(t); r.Failed != 0 || len(srv.OutboundTags()) != 3 {
		t.Fatalf("report %+v outbounds %v", r, srv.OutboundTags())
	}

	sub.Sources[0].MaxActive = 2
	f.s.Config = cfg(allKinds, sub)
	f.s.Memory = state.New()
	writeCandidates(t, path, all, 1, 4, 5)
	n := len(srv.Calls())
	srv.FailNext("AddOutbound", errors.New("boom"))
	if r := f.run(t); r.Failed == 0 {
		t.Fatalf("report %+v", r)
	}
	have := map[string]bool{}
	added := false
	for i, c := range srv.Calls() {
		method, tag, _ := strings.Cut(c, ":")
		switch {
		case !strings.HasPrefix(tag, "x-observe-wg-"):
		case method == "AddOutbound":
			if i >= n {
				added = true
				if i == n+slices.IndexFunc(srv.Calls()[n:], func(c string) bool { return strings.HasPrefix(c, "AddOutbound:") }) {
					continue // the injected failure
				}
			}
			have[tag] = true
		case method == "RemoveOutbound":
			delete(have, tag)
		}
		if added && len(have) > 2 {
			t.Fatalf("%d outbounds after call %d %s", len(have), i, c)
		}
	}
	if got := srv.OutboundTags(); len(got) > 2 {
		t.Fatalf("outbounds %v", got)
	}
}
