package syncer

import (
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/a1ekseev/scd/internal/common/candidate"
	"github.com/a1ekseev/scd/internal/scd/config"
	"github.com/a1ekseev/scd/internal/scd/monitor"
	"github.com/a1ekseev/scd/internal/scd/state"
)

// mqFixtureLines are the lines of the masque fixture: 1/2 the h3/h2 lines
// of device A (NL), 3/4 of device B (DE), 5 the h3 line of device C (US).
func mqFixtureLines(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile("../../../docker/fixtures/masque/candidates.txt")
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

// mqSub is a subscription with one masque source "mq" reading path:
// maxActive 2 by label on ports 20100..20103, replaced after one failure.
func mqSub(path string) config.SubscriptionConfig {
	sub := wgSub(path)
	src := &sub.Sources[0]
	src.ID, src.Type, src.WireGuard = "mq", config.SourceMasque, nil
	src.Selection.Strategy = config.StrategyLabel
	return sub
}

// mqActive are the base tags of the masque tunnels in slot order.
func mqActive(t *testing.T, f *fixture) []string {
	t.Helper()
	var tags []string
	f.s.Memory.View(state.Key("sub", addrA), func(tg *state.Target) {
		for _, tn := range tg.Tunnels {
			tags = append(tags, tn.Mapping.BaseTunnelID)
			if tn.Mapping.Protocol != "masque" {
				t.Errorf("tunnel %s protocol %q", tn.Mapping.BaseTunnelID, tn.Mapping.Protocol)
			}
		}
	})
	return tags
}

// mqKeyForms are the device keys of the fixture as the line, Xray JSON and
// proto hold them.
func mqKeyForms(t *testing.T, lines []string) []string {
	t.Helper()
	var out []string
	for _, l := range lines {
		if c, err := candidate.ParseMasque(l); err == nil {
			p8, _ := x509.MarshalPKCS8PrivateKey(c.PrivateKey)
			sec1, _ := x509.MarshalECPrivateKey(c.PrivateKey)
			out = append(out, base64.RawURLEncoding.EncodeToString(p8), base64.StdEncoding.EncodeToString(p8), base64.StdEncoding.EncodeToString(sec1))
		}
	}
	return out
}

// One tunnel per device, h3 first; a failed h3 is quarantined and its h2
// takes the slot; after the quarantine expires the healthy h2 stays. Xray
// never has more than maxActive outbounds of the source, and no output
// holds a device key.
func TestMasqueTransportFallback(t *testing.T) {
	all := mqFixtureLines(t)
	path := filepath.Join(t.TempDir(), "mq")
	writeCandidates(t, path, all, 1, 2, 3, 4, 5, 6, 7)
	sub := mqSub(path)
	f := newSourcesFixture(t, sub, nil)
	srv := f.x.servers[addrA]
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	f.s.Now = func() time.Time { return now }

	r := f.run(t)
	if r.Failed != 0 {
		t.Fatalf("report %+v", r)
	}
	tags := mqActive(t, f)
	// By label: B (DE) before A (NL); h3 of each.
	if len(tags) != 2 || !strings.HasPrefix(tags[0], "mq-de-frankfurt-mq-") || !strings.HasPrefix(tags[1], "mq-nl-amsterdam-mq-") {
		t.Fatalf("active %v", tags)
	}
	var byTransport map[string]string
	f.s.Memory.View(state.Key("sub", addrA), func(tg *state.Target) {
		byTransport = map[string]string{}
		for _, e := range tg.Sources["mq"].Candidates {
			c := e.SelectionCandidate()
			byTransport[e.Tag] = c.Transport
		}
		if n := tg.Sources["mq"].CandidateCount; n != 5 {
			t.Errorf("candidateCount %d", n)
		}
	})
	if byTransport[tags[0]] != "h3" || byTransport[tags[1]] != "h3" {
		t.Fatalf("transports %v of %v", byTransport, tags)
	}

	d := monitorDeps(f, false)
	d.Now = func() time.Time { return now }
	if done, err := monitor.ReplaceFailed(t.Context(), d, sub, sub.Sources[0], tags[1]); err != nil || !done {
		t.Fatalf("replace: %v %v", done, err)
	}
	after := mqActive(t, f)
	if after[0] != tags[0] || !strings.HasPrefix(after[1], "mq-nl-amsterdam-mq-") || byTransport[after[1]] != "h2" {
		t.Fatalf("after the failure %v (%v)", after, byTransport)
	}
	if e := f.logs.events(t, "candidate_replaced"); len(e) != 1 || e[0]["reasonCode"] != "failed" || e[0]["toTag"] != after[1] {
		t.Fatalf("candidate_replaced %v", e)
	}

	for _, at := range []time.Duration{time.Minute, 5 * time.Minute} {
		now = now.Add(at)
		n := len(srv.Calls())
		if r := f.run(t); r.Failed != 0 {
			t.Fatalf("report %+v", r)
		}
		if got := mqActive(t, f); !slices.Equal(got, after) || len(mutationCalls(srv.Calls()[n:])) != 0 {
			t.Fatalf("+%v: active %v, calls %q", at, got, srv.Calls()[n:])
		}
	}

	// A full apply (hashes reset, as after a failed replacement or a refused
	// add; then a changed config hash) keeps h2: priority only picks a new
	// transport.
	f.s.Memory.Update(state.Key("sub", addrA), func(tg *state.Target) { tg.ResetSourceHashes("mq") })
	if r := f.run(t); r.Failed != 0 {
		t.Fatalf("report %+v", r)
	}
	if e := f.logs.events(t, "resource_apply_started"); len(e) != 6 {
		t.Fatalf("no full apply: %d resource_apply_started", len(e))
	}
	if got := mqActive(t, f); !slices.Equal(got, after) {
		t.Fatalf("after a reset: active %v, want %v", got, after)
	}
	changed := mqSub(path)
	changed.Sources[0].PortRange.End = 20102
	f.s.Config = cfg(allKinds, changed)
	if r := f.run(t); r.Failed != 0 {
		t.Fatalf("report %+v", r)
	}
	if got := mqActive(t, f); !slices.Equal(got, after) {
		t.Fatalf("after a config change: active %v, want %v", got, after)
	}

	if peak := peakOutbounds(srv.Calls(), "mq"); peak > 2 {
		t.Fatalf("peak %d outbounds of the source, maxActive 2", peak)
	}
	if got := srv.OutboundTags(); len(got) != 2 {
		t.Fatalf("outbounds %v", got)
	}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range mqKeyForms(t, all) {
		if strings.Contains(string(b), k) || strings.Contains(f.logs.String(), k) {
			t.Fatalf("the report or the log holds a device key")
		}
	}
}

// A failure at any step of a repair or rejoin of a masque tunnel (sent or
// not), then a replacement by its other transport and a sync: every client
// goes through apply.Capped, so Xray never has more than maxActive
// outbounds of the source, checked after every RPC and by replaying calls.
func TestMasqueRepairRejoinThroughCap(t *testing.T) {
	for _, op := range []string{"repair", "rejoin"} {
		for _, method := range []string{"RemoveRule", "RemoveOutbound", "AddOutbound", "AddRule"} {
			for _, executed := range []bool{false, true} {
				name := op + "/" + method
				if executed {
					name += "/executed"
				}
				t.Run(name, func(t *testing.T) {
					all := mqFixtureLines(t)
					path := filepath.Join(t.TempDir(), "mq")
					writeCandidates(t, path, all, 1, 2, 3, 4, 5)
					sub := mqSub(path)
					f := newSourcesFixture(t, sub, nil)
					srv := f.x.servers[addrA]
					if r := f.run(t); r.Failed != 0 {
						t.Fatalf("report %+v", r)
					}
					before := mqActive(t, f)
					if op == "rejoin" {
						watch(t, f, nil)
						if err := monitor.Repair(t.Context(), monitorDeps(f, false), sub, before[1]); err != nil {
							t.Fatal(err)
						}
					}
					watch(t, f, failOnce(method, executed))
					run := monitor.Repair
					if op == "rejoin" {
						run = monitor.Rejoin
					}
					if err := run(t.Context(), monitorDeps(f, op == "rejoin"), sub, before[1]); err != nil {
						t.Fatal(err)
					}
					assertCap(t, srv, op)
					watch(t, f, nil)
					if _, err := monitor.ReplaceFailed(t.Context(), monitorDeps(f, false), sub, sub.Sources[0], before[1]); err != nil {
						t.Fatal(err)
					}
					assertCap(t, srv, "replace")
					f.run(t)
					assertCap(t, srv, "sync")
					f.run(t)
					assertCap(t, srv, "second sync")
					if got := sourceOutbounds(srv); len(got) != 2 {
						t.Fatalf("outbounds %v", got)
					}
					// The fake records only what reached it, so the replay is exact.
					if peak := peakOutbounds(srv.Calls(), "mq"); peak > 2 {
						t.Fatalf("peak %d outbounds of the source, maxActive 2", peak)
					}
				})
			}
		}
	}
}
