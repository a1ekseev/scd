//go:build xraylive

package livetest

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/xtls/xray-core/core"
	"google.golang.org/protobuf/proto"

	"github.com/a1ekseev/scd/internal/common/subscription"
	"github.com/a1ekseev/scd/internal/common/vless"
	"github.com/a1ekseev/scd/internal/scd/normalize"
	"github.com/a1ekseev/scd/internal/scd/outbound"
	"github.com/a1ekseev/scd/internal/scd/outbound/paramcheck"
	"github.com/a1ekseev/scd/internal/scd/xrayapi"
)

const (
	fixturesDir   = root + "docker/fixtures/"
	realShapesDir = fixturesDir + "real-shapes/"

	reasonInvalidKey = "Invalid VLESS encryption key."
	mlkemPrefix      = "mlkem768x25519plus.native.0rtt."
)

// checkedRows are the ✓ rows of 11 §3 (the 3#… matrixRow values of the
// corpus entry rows): each must reach Xray through at least one line.
var checkedRows = []string{
	"3#1", "3#2", "3#3", "3#4", "3#5", "3#6", "3#8", "3#9", "3#10", "3#11", "3#12", "3#13", "3#14",
}

// liveCase is one link sent to Xray with the parameters to check in the
// handler Xray lists back.
type liveCase struct {
	name      string
	line      subscription.Line
	matrixRow string
	params    []string
}

// liveCoverage is what one pass over all cases proved against live Xray.
type liveCoverage struct {
	params  map[string]bool // parameters whose check passed at least once
	rows    map[string]bool // 3#… matrix rows with a passing line
	sources map[string]int  // passing lines per source
}

var (
	coverageMu sync.Mutex
	coverage   *liveCoverage
)

// liveCases collects the D26 corpus entry lines, every line of
// docker/fixtures/subscription.txt and the 54 real-shape lines (12 §9 p.12).
func liveCases(t *testing.T) []liveCase {
	t.Helper()
	var cases []liveCase
	var rs1, rs2 []string
	for _, c := range loadD26(t) {
		switch {
		case strings.HasPrefix(c.Note, "RS1:"):
			rs1 = c.Params
		case strings.HasPrefix(c.Note, "RS2:"):
			rs2 = c.Params
		}
		if c.Expect != "entry" {
			continue
		}
		cases = append(cases, liveCase{
			name:      "d26/line-" + strconv.Itoa(c.Line),
			line:      subscription.Line{Number: c.Line, Raw: c.Raw, Trimmed: strings.TrimSpace(c.Raw)},
			matrixRow: c.MatrixRow,
			params:    c.Params,
		})
	}
	if rs1 == nil || rs2 == nil {
		t.Fatal("d26-expect.json has no RS1/RS2 rows")
	}
	for _, l := range subscriptionLines(t, fixturesDir+"subscription.txt") {
		cases = append(cases, liveCase{name: "subscription/line-" + strconv.Itoa(l.Number), line: l})
	}
	for _, src := range []struct {
		file   string
		params []string
	}{{"remnawave.txt", rs1}, {"provider2.txt", rs2}} {
		for _, l := range subscriptionLines(t, realShapesDir+src.file) {
			cases = append(cases, liveCase{
				name:   strings.TrimSuffix(src.file, ".txt") + "/line-" + strconv.Itoa(l.Number),
				line:   l,
				params: src.params,
			})
		}
	}
	return cases
}

// source is the prefix of a case name (d26, subscription, remnawave,
// provider2).
func source(name string) string {
	s, _, _ := strings.Cut(name, "/")
	return s
}

// runEveryParam sends every case to Xray and checks it in ListOutbounds; it
// records and returns what passed.
func runEveryParam(t *testing.T, c xrayapi.Client) *liveCoverage {
	t.Helper()
	cov := &liveCoverage{params: map[string]bool{}, rows: map[string]bool{}, sources: map[string]int{}}
	for _, lc := range liveCases(t) {
		t.Run(lc.name, func(t *testing.T) {
			passed := checkCase(t, c, lc)
			if t.Failed() {
				return
			}
			for _, p := range passed {
				cov.params[p] = true
			}
			if lc.matrixRow != "" {
				cov.rows[lc.matrixRow] = true
			}
			cov.sources[source(lc.name)]++
		})
	}
	coverageMu.Lock()
	coverage = cov
	coverageMu.Unlock()
	return cov
}

// checkCase: Parse → Normalize(AllowInsecure) → Build → ToProto →
// AddOutbound → ListOutbounds → the listed handler equals the sent one and
// every parameter is found in it (paramcheck on what live Xray returned).
// It returns the parameters checked.
func checkCase(t *testing.T, c xrayapi.Client, lc liveCase) []string {
	t.Helper()
	link, skip := vless.Parse(lc.line)
	if skip != nil {
		t.Fatalf("parse skip: %+v", *skip)
	}
	n, _, skip := normalize.Normalize(link, normalize.Policy{AllowInsecure: true})
	if skip != nil {
		t.Fatalf("normalize skip: %+v", *skip)
	}
	sent, err := outbound.ToProto(outbound.Build(n))
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	if err := c.AddOutbound(ctx, sent); err != nil {
		t.Fatalf("AddOutbound %s: %v", sent.Tag, err)
	}
	t.Cleanup(func() {
		// t.Context is already done in Cleanup.
		if err := c.RemoveOutbound(context.WithoutCancel(t.Context()), sent.Tag); err != nil {
			t.Errorf("RemoveOutbound %s: %v", sent.Tag, err)
		}
	})
	got := listed(t, c, sent.Tag)
	if got == nil {
		t.Fatalf("outbound %s is not in ListOutbounds", sent.Tag)
	}
	equalInstance(t, "SenderSettings", sent.SenderSettings, got.SenderSettings)
	equalInstance(t, "ProxySettings", sent.ProxySettings, got.ProxySettings)

	params := lc.params
	if params == nil { // subscription.txt: every covered parameter of the link
		for _, p := range link.Params {
			if slices.Contains(paramcheck.Covered, p.Key) {
				params = append(params, p.Key)
			}
		}
	}
	for _, p := range params {
		value, _ := link.Get(p)
		if err := paramcheck.Check(p, value, got); err != nil {
			t.Errorf("%s=%q not in ListOutbounds: %v", p, value, err)
		}
	}
	return params
}

// listed returns the handler with tag from ListOutbounds.
func listed(t *testing.T, c xrayapi.Client, tag string) *core.OutboundHandlerConfig {
	t.Helper()
	list, err := c.ListOutbounds(t.Context())
	if err != nil {
		t.Fatalf("ListOutbounds: %v", err)
	}
	for _, h := range list {
		if h.Tag == tag {
			return h
		}
	}
	return nil
}

// equalInstance compares the typed messages of two TypedMessages.
func equalInstance(t *testing.T, field string, sent, got interface {
	GetInstance() (proto.Message, error)
},
) {
	t.Helper()
	s, err := sent.GetInstance()
	if err != nil {
		t.Fatalf("%s sent: %v", field, err)
	}
	g, err := got.GetInstance()
	if err != nil {
		t.Fatalf("%s listed: %v", field, err)
	}
	if !proto.Equal(s, g) {
		t.Errorf("%s listed by Xray differs from the sent one:\nsent %v\ngot  %v", field, s, g)
	}
}

// TestLiveEveryParamReachesXray (D6, D19, D26, D33; 12 §5): every accepted
// corpus line and every fixture line is added to live Xray, listed back
// unchanged, and each of its parameters is found in what Xray lists.
func TestLiveEveryParamReachesXray(t *testing.T) {
	c, _, _ := startXray(t)
	cov := runEveryParam(t, c)
	want := map[string]int{"d26": 73, "subscription": 8, "remnawave": 4, "provider2": 50}
	if !maps.Equal(cov.sources, want) && !t.Failed() {
		t.Errorf("passing lines per source = %v, want %v", cov.sources, want)
	}
	t.Logf("passing lines per source: %v; parameters checked: %d", cov.sources, len(cov.params))
}

// TestLiveCoverage: the parameters proven live are exactly paramcheck.Covered
// (22, with cs and heartbeatperiod) and every ✓ row of 11 §3 passed with at
// least one line.
func TestLiveCoverage(t *testing.T) {
	coverageMu.Lock()
	cov := coverage
	coverageMu.Unlock()
	if cov == nil {
		c, _, _ := startXray(t)
		cov = runEveryParam(t, c)
	}
	if len(paramcheck.Covered) != 22 {
		t.Fatalf("paramcheck.Covered has %d parameters, want 22", len(paramcheck.Covered))
	}
	got := slices.Sorted(maps.Keys(cov.params))
	want := slices.Sorted(slices.Values(paramcheck.Covered))
	if !slices.Equal(got, want) {
		t.Errorf("parameters proven live = %v, want paramcheck.Covered %v", got, want)
	}
	for _, p := range []string{"cs", "heartbeatperiod"} {
		if !cov.params[p] {
			t.Errorf("D33 parameter %s not proven live", p)
		}
	}
	corpusRows := map[string]bool{}
	for _, c := range loadD26(t) {
		if c.Expect == "entry" && strings.HasPrefix(c.MatrixRow, "3#") {
			corpusRows[c.MatrixRow] = true
		}
	}
	if rows := slices.Sorted(maps.Keys(corpusRows)); !slices.Equal(rows, slices.Sorted(slices.Values(checkedRows))) {
		t.Errorf("✓ rows of 11 §3 in the corpus = %v, want %v", rows, checkedRows)
	}
	for _, r := range checkedRows {
		if !cov.rows[r] {
			t.Errorf("matrix row %s (11 §3) has no line that passed live", r)
		}
	}
}

// badMLKEMLink returns corpus line 15 (tcp + ML-KEM-768, security omitted)
// with its key replaced by 1184 random bytes: the right length, but not an
// ML-KEM encapsulation key.
func badMLKEMLink(t *testing.T) (good, bad subscription.Line, badEncryption string) {
	t.Helper()
	raw := loadD26(t)[14].Raw
	link, skip := vless.Parse(subscription.Line{Number: 15, Raw: raw, Trimmed: raw})
	if skip != nil {
		t.Fatalf("line 15: %+v", *skip)
	}
	enc, _ := link.Get("encryption")
	if !strings.HasPrefix(enc, mlkemPrefix) {
		t.Fatalf("line 15 encryption %q is not %s…", enc, mlkemPrefix)
	}
	key := make([]byte, 1184)
	_, _ = rand.Read(key)
	badEncryption = mlkemPrefix + base64.RawURLEncoding.EncodeToString(key)
	badRaw := strings.Replace(raw, "encryption="+enc, "encryption="+badEncryption, 1)
	if badRaw == raw {
		t.Fatal("line 15 encryption is not written verbatim in the link")
	}
	good = subscription.Line{Number: 15, Raw: raw, Trimmed: raw}
	bad = subscription.Line{Number: 15, Raw: badRaw, Trimmed: badRaw}
	return good, bad, badEncryption
}

// addInvalidKey adds the normalized form of the valid line with the bad key
// put back in after normalize (bypassing its check) and returns the
// AddOutbound error.
func addInvalidKey(t *testing.T, c xrayapi.Client, good subscription.Line, badEnc string) error {
	t.Helper()
	link, skip := vless.Parse(good)
	if skip != nil {
		t.Fatalf("%+v", *skip)
	}
	n, _, nskip := normalize.Normalize(link, normalize.Policy{})
	if nskip != nil {
		t.Fatalf("valid line skipped: %+v", *nskip)
	}
	n.Encryption = badEnc
	n.Tag = "live-invalid-mlkem"
	h, err := outbound.ToProto(outbound.Build(n))
	if err != nil {
		t.Fatalf("ToProto (conf.Build) rejected the key: %v", err)
	}
	err = c.AddOutbound(t.Context(), h)
	if err == nil {
		_ = c.RemoveOutbound(t.Context(), h.Tag)
		t.Fatal("live Xray accepted an invalid ML-KEM key")
	}
	return err
}

// TestLiveRejectsInvalidMLKEMKeyLikeNormalize (D25, Review Focus 6): a key of
// the right length that is not a key passes conf.Build but live Xray rejects
// it on AddOutbound; normalize skips the same link, so it never reaches
// outbound apply.
func TestLiveRejectsInvalidMLKEMKeyLikeNormalize(t *testing.T) {
	c, _, _ := startXray(t)
	good, bad, badEnc := badMLKEMLink(t)

	if err := addInvalidKey(t, c, good, badEnc); !strings.Contains(err.Error(), "invalid polynomial encoding") {
		t.Fatalf("AddOutbound error = %q, want invalid polynomial encoding", err)
	}

	link, skip := vless.Parse(bad)
	if skip != nil {
		t.Fatalf("%+v", *skip)
	}
	_, _, nskip := normalize.Normalize(link, normalize.Policy{AllowInsecure: true})
	if nskip == nil || nskip.Code != vless.CodeUnsupportedValue || nskip.Reason != reasonInvalidKey {
		t.Fatalf("normalize = %+v, want %s %q", nskip, vless.CodeUnsupportedValue, reasonInvalidKey)
	}
}

// TestLiveXrayStillAlive: after every case and the rejected invalid key, the
// same Xray process is alive, answers ListOutbounds and has not panicked.
func TestLiveXrayStillAlive(t *testing.T) {
	p := launchXray(t)
	runEveryParam(t, p.client)
	good, _, badEnc := badMLKEMLink(t)
	_ = addInvalidKey(t, p.client, good, badEnc)
	if t.Failed() {
		t.FailNow()
	}
	if p.exited() {
		t.Fatalf("xray exited: %v\n%s", p.waitErr, p.log.String())
	}
	list, err := p.client.ListOutbounds(t.Context())
	if err != nil {
		t.Fatalf("ListOutbounds after all cases: %v", err)
	}
	tags := make([]string, 0, len(list))
	for _, h := range list {
		tags = append(tags, h.Tag)
	}
	slices.Sort(tags)
	if !slices.Equal(tags, []string{"blocked", "direct"}) {
		t.Errorf("outbounds left after all cases = %v, want [blocked direct]", tags)
	}
	if strings.Contains(p.log.String(), "panic:") {
		t.Fatalf("xray panicked:\n%s", p.log.String())
	}
}
