package apply_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/xtls/xray-core/core"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/a1ekseev/scd/internal/common/vless"
	"github.com/a1ekseev/scd/internal/scd/apply"
	"github.com/a1ekseev/scd/internal/scd/outbound"
	"github.com/a1ekseev/scd/internal/scd/xrayapi"
	"github.com/a1ekseev/scd/internal/scd/xrayapi/xrayfake"
)

const target = "127.0.0.1:8080"

var fixedNow = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func meta() apply.Meta {
	return apply.Meta{
		SourceID:       "source-1",
		SubscriptionID: "source-1",
		TargetAddress:  target,
		Now:            func() time.Time { return fixedNow },
	}
}

// entry is a minimal VLESS tcp outbound that conf.Build accepts.
func entry(tag, address string) outbound.Outbound {
	return outbound.Outbound{
		Tag:      tag,
		Protocol: "vless",
		Settings: outbound.Settings{Vnext: []outbound.Server{{
			Address: address,
			Port:    443,
			Users: []outbound.User{{
				ID:         "7d1b6590-1069-4372-92be-8d0a0ae6eaf5",
				Encryption: "none",
				Email:      tag,
			}},
		}}},
		StreamSettings: outbound.StreamSettings{Network: "tcp", Security: "none"},
	}
}

func mustProto(t *testing.T, o outbound.Outbound) *core.OutboundHandlerConfig {
	t.Helper()
	h, err := outbound.ToProto(o)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// seed adds outbounds to the fake through the client and returns the calls
// log offset after seeding.
func seed(t *testing.T, srv *xrayfake.Server, c xrayapi.Client, entries ...outbound.Outbound) int {
	t.Helper()
	for _, e := range entries {
		if err := c.AddOutbound(context.Background(), mustProto(t, e)); err != nil {
			t.Fatal(err)
		}
	}
	return len(srv.Calls())
}

// snapshot returns every outbound of the fake by tag.
func snapshot(srv *xrayfake.Server) map[string]*core.OutboundHandlerConfig {
	out := map[string]*core.OutboundHandlerConfig{}
	for _, tag := range srv.OutboundTags() {
		out[tag] = srv.Outbound(tag)
	}
	return out
}

func assertState(t *testing.T, srv *xrayfake.Server, want map[string]*core.OutboundHandlerConfig) {
	t.Helper()
	got := snapshot(srv)
	if len(got) != len(want) {
		t.Fatalf("outbound tags = %v, want %d tags", srv.OutboundTags(), len(want))
	}
	for tag, w := range want {
		if !proto.Equal(got[tag], w) {
			t.Errorf("outbound %q differs from the original", tag)
		}
	}
}

func statuses(r apply.Report) []string {
	out := make([]string, 0, len(r.Items))
	for _, it := range r.Items {
		out = append(out, it.ID+"="+it.Status)
	}
	return out
}

func assertCounts(t *testing.T, r apply.Report, added, replaced, removed, failed int) {
	t.Helper()
	if r.Added != added || r.Replaced != replaced || r.Removed != removed || r.Failed != failed {
		t.Errorf("counts added/replaced/removed/failed = %d/%d/%d/%d, want %d/%d/%d/%d",
			r.Added, r.Replaced, r.Removed, r.Failed, added, replaced, removed, failed)
	}
}

// P23 (parser.test.ts:396).
func TestOutboundsReplacesExisting(t *testing.T) {
	srv, c := xrayfake.Start(t)
	from := seed(t, srv, c, entry("a", "198.51.100.1"))
	next := entry("a", "198.51.100.2")

	r, err := apply.Outbounds(context.Background(), c, nil, []outbound.Outbound{next}, meta())
	if err != nil {
		t.Fatal(err)
	}
	if got, want := statuses(r), []string{"a=removed", "a=replaced"}; !slices.Equal(got, want) {
		t.Errorf("items = %v, want %v", got, want)
	}
	assertCounts(t, r, 0, 1, 1, 0)
	if !slices.Equal(r.DeletedIDs, []string{"a"}) || !slices.Equal(r.AppliedIDs, []string{"a"}) {
		t.Errorf("deletedIds = %v, appliedIds = %v", r.DeletedIDs, r.AppliedIDs)
	}
	if got, want := srv.Calls()[from:], []string{"ListOutbounds", "RemoveOutbound:a", "AddOutbound:a"}; !slices.Equal(got, want) {
		t.Errorf("calls = %v, want %v", got, want)
	}
	if !proto.Equal(srv.Outbound("a"), mustProto(t, next)) {
		t.Error("outbound a is not the new one")
	}
	if r.Kind != apply.KindOutbound || r.SourceID != "source-1" || r.SubscriptionID != "source-1" || r.TargetAddress != target {
		t.Errorf("header = %+v", r)
	}
	if r.AppliedAt != "2026-01-01T00:00:00.000Z" || r.DurationMs != 0 {
		t.Errorf("appliedAt = %q, durationMs = %d", r.AppliedAt, r.DurationMs)
	}
}

func TestOutboundsKeepsFixed(t *testing.T) {
	srv, c := xrayfake.Start(t, xrayfake.WithStaticOutbounds("direct", "blocked"))
	from := seed(t, srv, c, entry("old", "198.51.100.1"))
	direct, blocked := srv.Outbound("direct"), srv.Outbound("blocked")

	entries := []outbound.Outbound{entry("a", "198.51.100.2"), entry("b", "198.51.100.3")}
	r, err := apply.Outbounds(context.Background(), c, []string{"direct", "blocked"}, entries, meta())
	if err != nil {
		t.Fatal(err)
	}
	if got, want := statuses(r), []string{"old=removed", "a=added", "b=added"}; !slices.Equal(got, want) {
		t.Errorf("items = %v, want %v", got, want)
	}
	assertCounts(t, r, 2, 0, 1, 0)
	if got, want := srv.OutboundTags(), []string{"a", "b", "blocked", "direct"}; !slices.Equal(got, want) {
		t.Errorf("tags = %v, want %v", got, want)
	}
	if !proto.Equal(srv.Outbound("direct"), direct) || !proto.Equal(srv.Outbound("blocked"), blocked) {
		t.Error("fixed outbounds changed")
	}
	for _, call := range srv.Calls()[from:] {
		if strings.HasSuffix(call, ":direct") || strings.HasSuffix(call, ":blocked") {
			t.Errorf("RPC touched a fixed outbound: %s", call)
		}
	}
}

func TestOutboundsDeletesForeignNonFixed(t *testing.T) {
	srv, c := xrayfake.Start(t, xrayfake.WithStaticOutbounds("direct", "foreign"))

	r, err := apply.Outbounds(context.Background(), c, []string{"direct"}, []outbound.Outbound{entry("a", "198.51.100.2")}, meta())
	if err != nil {
		t.Fatal(err)
	}
	if got, want := statuses(r), []string{"foreign=removed", "a=added"}; !slices.Equal(got, want) {
		t.Errorf("items = %v, want %v", got, want)
	}
	if got, want := srv.OutboundTags(), []string{"a", "direct"}; !slices.Equal(got, want) {
		t.Errorf("tags = %v, want %v", got, want)
	}
}

func TestOutboundsFixedConflictNoRPC(t *testing.T) {
	srv, c := xrayfake.Start(t)

	entries := []outbound.Outbound{entry("a", "198.51.100.1"), entry("direct", "198.51.100.2")}
	r, err := apply.Outbounds(context.Background(), c, []string{"direct"}, entries, meta())
	if err != nil {
		t.Fatal(err)
	}
	if calls := srv.Calls(); len(calls) != 0 {
		t.Errorf("calls = %v, want none", calls)
	}
	assertCounts(t, r, 0, 0, 0, 1)
	want := []apply.Item{{
		ID:      "direct",
		Status:  "failed",
		Message: `Managed outbound tag "direct" conflicts with fixedOutbounds on target "127.0.0.1:8080".`,
	}}
	if !slices.Equal(r.Items, want) {
		t.Errorf("items = %+v, want %+v", r.Items, want)
	}
}

// failOnAdd wraps the fake client and makes the AddOutbound of tag fail
// through the fake's FailNext, then runs after (for further injections).
type failOnAdd struct {
	xrayapi.OutboundAPI
	srv   *xrayfake.Server
	tag   string
	after func()
	fired bool
}

func (f *failOnAdd) AddOutbound(ctx context.Context, h *core.OutboundHandlerConfig) error {
	if h.GetTag() == f.tag && !f.fired {
		f.fired = true
		f.srv.FailNext("AddOutbound", status.Error(codes.Unknown, "boom"))
		err := f.OutboundAPI.AddOutbound(ctx, h)
		if f.after != nil {
			f.after()
		}
		return err
	}
	return f.OutboundAPI.AddOutbound(ctx, h)
}

// P24 (parser.test.ts:423).
func TestOutboundsRollbackOnAddFailure(t *testing.T) {
	srv, c := xrayfake.Start(t, xrayfake.WithStaticOutbounds("direct", "foreign"))
	from := seed(t, srv, c, entry("a", "198.51.100.1"))
	original := snapshot(srv)

	entries := []outbound.Outbound{entry("a", "198.51.100.2"), entry("b", "198.51.100.3")}
	api := &failOnAdd{OutboundAPI: c, srv: srv, tag: "b"}
	r, err := apply.Outbounds(context.Background(), api, []string{"direct"}, entries, meta())
	if err != nil {
		t.Fatal(err)
	}
	assertCounts(t, r, 0, 1, 2, 1)
	last := r.Items[len(r.Items)-1]
	if last != (apply.Item{ID: "b", Status: "failed", Message: "boom"}) {
		t.Errorf("last item = %+v", last)
	}
	assertState(t, srv, original)

	calls := srv.Calls()[from:]
	removed := []string{}
	for _, call := range calls {
		if tag, ok := strings.CutPrefix(call, "RemoveOutbound:"); ok {
			removed = append(removed, tag)
		}
	}
	// removal phase (2), then rollback of the added "a" (1)
	if len(removed) != 3 || removed[2] != "a" {
		t.Fatalf("removals = %v", removed)
	}
	wantTail := []string{
		"AddOutbound:a", "AddOutbound:b", // apply; b fails
		"RemoveOutbound:a",                                       // rollback of added, reverse order
		"AddOutbound:" + removed[0], "AddOutbound:" + removed[1], // backups in removal order
	}
	if got := calls[len(calls)-len(wantTail):]; !slices.Equal(got, wantTail) {
		t.Errorf("calls tail = %v, want %v", got, wantTail)
	}
}

func TestOutboundsRollbackErrorsIgnored(t *testing.T) {
	srv, c := xrayfake.Start(t)
	from := seed(t, srv, c, entry("old1", "198.51.100.1"), entry("old2", "198.51.100.2"))

	entries := []outbound.Outbound{entry("a", "198.51.100.3"), entry("b", "198.51.100.4")}
	api := &failOnAdd{OutboundAPI: c, srv: srv, tag: "b", after: func() {
		srv.FailNext("RemoveOutbound", status.Error(codes.Unknown, "rollback remove failed"))
		srv.FailNext("AddOutbound", status.Error(codes.Unknown, "rollback add failed"))
	}}
	r, err := apply.Outbounds(context.Background(), api, nil, entries, meta())
	if err != nil {
		t.Fatal(err)
	}
	last := r.Items[len(r.Items)-1]
	if last != (apply.Item{ID: "b", Status: "failed", Message: "boom"}) {
		t.Errorf("last item = %+v", last)
	}
	if r.Failed != 1 {
		t.Errorf("failed = %d", r.Failed)
	}
	calls := srv.Calls()[from:]
	// every rollback step is attempted despite the failures
	tail := calls[len(calls)-3:]
	if tail[0] != "RemoveOutbound:a" || !strings.HasPrefix(tail[1], "AddOutbound:old") || !strings.HasPrefix(tail[2], "AddOutbound:old") || tail[1] == tail[2] {
		t.Errorf("rollback calls = %v", tail)
	}
	// the first backup re-add failed, the second one went through; "a" stays
	// because its rollback removal failed
	if got := srv.OutboundTags(); len(got) != 2 || got[0] != "a" || !strings.HasPrefix(got[1], "old") {
		t.Errorf("tags = %v", got)
	}
}

func TestOutboundsRollbackOnRemoveFailure(t *testing.T) {
	srv, c := xrayfake.Start(t)
	seed(t, srv, c, entry("old", "198.51.100.1"))
	original := snapshot(srv)
	srv.FailNext("RemoveOutbound", status.Error(codes.Unknown, "remove boom"))

	entries := []outbound.Outbound{entry("a", "198.51.100.3")}
	r, err := apply.Outbounds(context.Background(), c, nil, entries, meta())
	if err != nil {
		t.Fatal(err)
	}
	// no add happened: the id falls back to entries[0].tag
	want := []apply.Item{{ID: "a", Status: "failed", Message: "remove boom"}}
	if !slices.Equal(r.Items, want) {
		t.Errorf("items = %+v, want %+v", r.Items, want)
	}
	assertCounts(t, r, 0, 0, 0, 1)
	assertState(t, srv, original)
}

func TestOutboundsFailedIDUnknownWithoutEntries(t *testing.T) {
	srv, c := xrayfake.Start(t)
	seed(t, srv, c, entry("old", "198.51.100.1"))
	srv.FailNext("RemoveOutbound", status.Error(codes.Unknown, "remove boom"))

	r, err := apply.Outbounds(context.Background(), c, nil, nil, meta())
	if err != nil {
		t.Fatal(err)
	}
	if got := r.Items[len(r.Items)-1].ID; got != "unknown" {
		t.Errorf("failed id = %q, want unknown", got)
	}
}

func TestOutboundsBuildErrorRollsBack(t *testing.T) {
	srv, c := xrayfake.Start(t)
	seed(t, srv, c, entry("old", "198.51.100.1"))
	original := snapshot(srv)

	bad := entry("bad", "198.51.100.3")
	bad.Protocol = "no-such-protocol"
	r, err := apply.Outbounds(context.Background(), c, nil, []outbound.Outbound{entry("a", "198.51.100.2"), bad}, meta())
	if err != nil {
		t.Fatal(err)
	}
	last := r.Items[len(r.Items)-1]
	if last.ID != "bad" || last.Status != "failed" || !strings.HasPrefix(last.Message, "invalid outbound bad: ") {
		t.Errorf("last item = %+v", last)
	}
	assertState(t, srv, original)
}

func TestOutboundsListErrorPropagates(t *testing.T) {
	srv, c := xrayfake.Start(t)
	srv.FailNext("ListOutbounds", status.Error(codes.Unknown, "list boom"))

	_, err := apply.Outbounds(context.Background(), c, nil, []outbound.Outbound{entry("a", "198.51.100.1")}, meta())
	if err == nil || err.Error() != "list boom" {
		t.Fatalf("err = %v, want list boom", err)
	}
	if got := srv.Calls(); !slices.Equal(got, []string{"ListOutbounds"}) {
		t.Errorf("calls = %v", got)
	}
}

func TestFailedReport(t *testing.T) {
	skipped := []vless.Skip{{Line: 2, Raw: "x", Code: "invalid_uri", Reason: "bad"}}
	r := apply.FailedReport(apply.KindInbound, meta(), skipped, errors.New("Input source timed out"))
	assertCounts(t, r, 0, 0, 0, 1)
	want := []apply.Item{{ID: "inbound", Status: "failed", Message: "Input source timed out"}}
	if !slices.Equal(r.Items, want) || r.DurationMs != 0 || r.Kind != apply.KindInbound || len(r.Skipped) != 1 {
		t.Errorf("report = %+v", r)
	}
	if r.AppliedAt != "2026-01-01T00:00:00.000Z" {
		t.Errorf("appliedAt = %q", r.AppliedAt)
	}
}

func TestKindOrder(t *testing.T) {
	want := []apply.Kind{"outbound", "inbound", "routing"}
	if !slices.Equal(apply.Order, want) {
		t.Errorf("Order = %v, want %v", apply.Order, want)
	}
}

// 03 §2.4: key order and names of ApplyReport.
func TestReportJSONKeyNames(t *testing.T) {
	r := apply.FailedReport(apply.KindOutbound, meta(), nil, errors.New("Input source timed out"))
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"kind":"outbound","sourceId":"source-1","subscriptionId":"source-1","targetAddress":"127.0.0.1:8080",` +
		`"appliedAt":"2026-01-01T00:00:00.000Z","durationMs":0,"added":0,"replaced":0,"removed":0,"failed":1,` +
		`"deletedIds":[],"appliedIds":[],"items":[{"id":"outbound","status":"failed","message":"Input source timed out"}],"skipped":[]}`
	if string(b) != want {
		t.Errorf("json =\n%s\nwant\n%s", b, want)
	}

	// a successful item has no message key; empty lists are [] not null
	_, c := xrayfake.Start(t)
	ok, err := apply.Outbounds(context.Background(), c, nil, []outbound.Outbound{entry("a", "198.51.100.1")}, meta())
	if err != nil {
		t.Fatal(err)
	}
	b, err = json.Marshal(ok)
	if err != nil {
		t.Fatal(err)
	}
	for _, part := range []string{`"deletedIds":[]`, `"items":[{"id":"a","status":"added"}]`, `"skipped":[]`, `"appliedIds":["a"]`} {
		if !strings.Contains(string(b), part) {
			t.Errorf("json %s lacks %s", b, part)
		}
	}
}

// Deferred 5.3: the rollback runs under context.WithoutCancel, so a
// cancellation that arrives with the failure still restores Xray (D2).
func TestOutboundsRollbackSurvivesCancel(t *testing.T) {
	srv, c := xrayfake.Start(t, xrayfake.WithStaticOutbounds("direct", "foreign"))
	seed(t, srv, c, entry("a", "198.51.100.1"))
	original := snapshot(srv)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	entries := []outbound.Outbound{entry("a", "198.51.100.2"), entry("b", "198.51.100.3")}
	api := &failOnAdd{OutboundAPI: c, srv: srv, tag: "b", after: cancel}
	r, err := apply.Outbounds(ctx, api, []string{"direct"}, entries, meta())
	if err != nil {
		t.Fatal(err)
	}
	if r.Failed != 1 {
		t.Fatalf("failed = %d", r.Failed)
	}
	assertState(t, srv, original)
}
