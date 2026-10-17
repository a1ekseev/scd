package manifest

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/a1ekseev/scd/internal/common/inputsrc"
	"github.com/a1ekseev/scd/internal/common/vless"
	"github.com/a1ekseev/scd/internal/scd/config"
	"github.com/a1ekseev/scd/internal/scd/normalize"
)

// fakeLoader records the calls of LoadSubscriptions and returns content
// (or err) for each input.
type fakeLoader struct {
	calls []string
	opts  []inputsrc.Options
	fail  map[string]error
}

func (f *fakeLoader) load(_ context.Context, input string, o inputsrc.Options) (inputsrc.Loaded, error) {
	f.calls = append(f.calls, input)
	f.opts = append(f.opts, o)
	if err := f.fail[input]; err != nil {
		return inputsrc.Loaded{}, err
	}
	return inputsrc.Loaded{Source: "src:" + input, Content: "content:" + input, Encoding: "plain"}, nil
}

func sub(id, input string) config.SubscriptionConfig {
	return config.SubscriptionConfig{ID: id, Input: input, Enabled: true, Format: "auto", FetchTimeoutMs: 5000, AllowInsecure: "deny"}
}

func ids[T any](list []T, id func(T) string) []string {
	out := []string{}
	for _, x := range list {
		out = append(out, id(x))
	}
	return out
}

func TestLoadSubscriptionsSortsEnabledByID(t *testing.T) { // S15
	disabled := sub("source-0", "in-0")
	disabled.Enabled = false
	f := &fakeLoader{}
	loaded, failed := LoadSubscriptions(context.Background(),
		[]config.SubscriptionConfig{sub("source-b", "in-b"), disabled, sub("source-a", "in-z"), sub("source-a", "in-y"), sub("Source-c", "in-c")},
		f.load)
	got := ids(loaded, func(s LoadedSubscription) string { return s.Config.ID + "/" + s.Config.Input })
	// strings.Compare (D23): upper case first, then id, then input.
	if want := []string{"Source-c/in-c", "source-a/in-y", "source-a/in-z", "source-b/in-b"}; !reflect.DeepEqual(got, want) || len(failed) != 0 {
		t.Fatalf("loaded %v failed %+v", got, failed)
	}
	if want := []string{"in-c", "in-y", "in-z", "in-b"}; !reflect.DeepEqual(f.calls, want) {
		t.Fatalf("calls %v", f.calls)
	}
	l := loaded[1]
	if l.Source != "src:in-y" || l.Content != "content:in-y" || l.Encoding != "plain" {
		t.Fatalf("loaded %+v", l)
	}
}

func TestLoadSubscriptionsContinuesAfterFailure(t *testing.T) {
	f := &fakeLoader{fail: map[string]error{"in-a": errors.New("boom")}}
	loaded, failed := LoadSubscriptions(context.Background(), []config.SubscriptionConfig{sub("b", "in-b"), sub("a", "in-a")}, f.load)
	if len(loaded) != 1 || loaded[0].Config.ID != "b" || len(failed) != 1 || failed[0].Config.ID != "a" || failed[0].Error != "boom" {
		t.Fatalf("loaded %+v failed %+v", loaded, failed)
	}
	if !reflect.DeepEqual(f.calls, []string{"in-a", "in-b"}) {
		t.Fatalf("calls %v", f.calls)
	}
}

func TestLoadSubscriptionsPassesFormatAndTimeout(t *testing.T) {
	s := sub("a", "in-a")
	s.Format, s.FetchTimeoutMs = "base64", 1234
	f := &fakeLoader{}
	LoadSubscriptions(context.Background(), []config.SubscriptionConfig{s}, f.load)
	if o := f.opts[0]; o.Format != "base64" || o.FetchTimeout != 1234*time.Millisecond {
		t.Fatalf("options %+v", o)
	}
}

func TestLoadSubscriptionsPassesUserAgent(t *testing.T) {
	a, b := sub("a", "in-a"), sub("b", "in-b")
	a.FetchUserAgent = "Happ/1.0"
	f := &fakeLoader{}
	LoadSubscriptions(context.Background(), []config.SubscriptionConfig{a, b}, f.load)
	if f.opts[0].UserAgent != "Happ/1.0" || f.opts[1].UserAgent != "" {
		t.Fatalf("user agents %q %q", f.opts[0].UserAgent, f.opts[1].UserAgent)
	}
}

func TestLoadSubscriptionsPassesFetchHeaders(t *testing.T) { // D33
	var seen string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("x-hwid")
		http.Error(w, "nope", http.StatusForbidden)
	}))
	defer srv.Close()
	s := sub("a", srv.URL+"/sub")
	s.FetchHeaders = inputsrc.Headers{"x-hwid": "s3cr3t"}

	f := &fakeLoader{}
	LoadSubscriptions(context.Background(), []config.SubscriptionConfig{s}, f.load)
	if f.opts[0].Headers.Plain()["x-hwid"] != "s3cr3t" {
		t.Fatalf("headers %v", f.opts[0].Headers.Plain())
	}

	loaded, failed := LoadSubscriptions(context.Background(), []config.SubscriptionConfig{s}, inputsrc.Load)
	if seen != "s3cr3t" || len(loaded) != 0 || len(failed) != 1 {
		t.Fatalf("seen %q loaded %+v failed %+v", seen, loaded, failed)
	}
	if strings.Contains(failed[0].Error, "s3cr3t") || !strings.Contains(failed[0].Error, "403") {
		t.Fatalf("error %q", failed[0].Error)
	}
}

func TestValidateNoValidEntriesMessage(t *testing.T) {
	m := Build("trojan://example\n", "src", deny, testNow)
	err := Validate(m, "source-1")
	want := `Subscription source "source-1" produced no valid outbound entries. parsed=0 skipped=1 filtered=0 ` +
		`filteredByCountry=0 filteredByLabelRegex=0 unsupportedScheme=1 unsupportedParam=0 unsupportedValue=0 ` +
		`unsupportedCombo=0 invalidUri=0 missingRequiredField=0 duplicateTag=0`
	if err == nil || err.Error() != want || !errors.Is(err, ErrNoValidEntries) {
		t.Fatalf("got %v\nwant %s", err, want)
	}
	// Every counter is printed from the summary.
	m.Summary = Summary{1, 0, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}
	want = `Subscription source "x" produced no valid outbound entries. parsed=0 skipped=2 filtered=3 ` +
		`filteredByCountry=4 filteredByLabelRegex=5 unsupportedScheme=6 unsupportedParam=7 unsupportedValue=8 ` +
		`unsupportedCombo=9 invalidUri=10 missingRequiredField=11 duplicateTag=12`
	if err := Validate(m, "x"); err == nil || err.Error() != want {
		t.Fatalf("got %v\nwant %s", err, want)
	}
	if err := Validate(links(t, "a"), "x"); err != nil {
		t.Fatal(err)
	}
}

func loadedSub(s config.SubscriptionConfig, content string) LoadedSubscription {
	return LoadedSubscription{Config: s, Source: s.Input, Content: content, Encoding: "plain"}
}

func TestFromSubscriptionTrimsContent(t *testing.T) {
	// Like TS sync: the whole content is trimmed, so leading blank lines do
	// not count and the first line's surrounding white space is cut.
	m, err := FromSubscription(loadedSub(sub("source-1", "in"), "\n\n  "+link(1, "a")+"  \n\n"), testNow)
	if err != nil {
		t.Fatal(err)
	}
	if m.SourceFile != "source-1" || m.Summary.TotalLines != 1 || m.Entries[0].Line != 1 || m.Entries[0].Normalized.Raw != link(1, "a") {
		t.Fatalf("manifest %+v", m)
	}
	if want := Build(link(1, "a"), "source-1", deny, testNow); !reflect.DeepEqual(m, want) {
		t.Fatalf("trimmed manifest differs")
	}
}

func TestFromSubscriptionAppliesFiltersAndValidates(t *testing.T) {
	s := sub("source-1", "in")
	s.Filters = filters(t, []string{"US"}, nil, nil)
	_, err := FromSubscription(loadedSub(s, link(1, "🇦🇹 Wien")), testNow)
	if !errors.Is(err, ErrNoValidEntries) || !strings.Contains(err.Error(), `"source-1"`) || !strings.Contains(err.Error(), "filteredByCountry=1") {
		t.Fatalf("err %v", err)
	}
}

func TestFromSubscriptionUsesAllowInsecurePolicy(t *testing.T) {
	l := "vless://11111111-1111-4111-8111-111111111119@198.51.100.10:443?type=ws&security=tls&sni=cdn.example.test&path=%2Fws&allowInsecure=1#ws"
	a := sub("a", "in")
	a.AllowInsecure = "allow"
	m, err := FromSubscription(loadedSub(a, l), testNow)
	if err != nil || len(m.Entries) != 1 || m.Entries[0].Warnings[0].Code != normalize.WarnAllowInsecureIgnored {
		t.Fatalf("allow: %v %+v", err, m.Entries)
	}
	m, err = FromSubscription(loadedSub(sub("a", "in"), l), testNow)
	if !errors.Is(err, ErrNoValidEntries) || len(m.Skipped) != 1 || m.Skipped[0].Code != vless.CodeUnsupportedValue {
		t.Fatalf("deny: %v %+v", err, m.Skipped)
	}
	if PolicyFor(a) != (normalize.Policy{AllowInsecure: true}) || PolicyFor(sub("a", "in")) != (normalize.Policy{}) {
		t.Fatal("PolicyFor")
	}
}
