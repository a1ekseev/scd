package monitor

import (
	"net/url"
	"slices"
	"strings"
	"testing"
)

// queryOrder returns the decoded keys of rawQuery in order.
func queryOrder(t *testing.T, rawQuery string) []string {
	t.Helper()
	var keys []string
	for pair := range strings.SplitSeq(rawQuery, "&") {
		k, _, _ := strings.Cut(pair, "=")
		dk, err := url.QueryUnescape(k)
		if err != nil {
			t.Fatal(err)
		}
		keys = append(keys, dk)
	}
	return keys
}

// 03 §10.6 p.25 and §4.6: set replaces the first occurrence in place and
// drops the others, ping is removed and appended last, other parameters are
// kept. Compared by key order and decoded values, not bytes (D9, F20).
func TestRemotePingURL(t *testing.T) {
	cases := []struct {
		name, raw, status, msg string
		ping                   *int
		base                   string
		keys                   []string
		values                 url.Values
		fragment               string
	}{
		{
			name:   "overwrites push params in place",
			raw:    "https://push.example.test/api/push/token?keep=1&status=down&msg=old&ping=999",
			status: "up", msg: "OK", ping: new(42),
			base:   "https://push.example.test/api/push/token",
			keys:   []string{"keep", "status", "msg", "ping"},
			values: url.Values{"keep": {"1"}, "status": {"up"}, "msg": {"OK"}, "ping": {"42"}},
		},
		{
			name:   "drops ping without latency",
			raw:    "https://push.example.test/api/push/token?keep=1&ping=999",
			status: "down", msg: "failed",
			base:   "https://push.example.test/api/push/token",
			keys:   []string{"keep", "status", "msg"},
			values: url.Values{"keep": {"1"}, "status": {"down"}, "msg": {"failed"}},
		},
		{
			name:   "form-encodes the message",
			raw:    "https://push.example.test/api/push/token",
			status: "down", msg: "Expected HTTP 204, got 200.",
			base:   "https://push.example.test/api/push/token",
			keys:   []string{"status", "msg"},
			values: url.Values{"status": {"down"}, "msg": {"Expected HTTP 204, got 200."}},
		},
		{
			name:   "ping moves to the end, duplicates collapse",
			raw:    "https://h.test/p?ping=1&status=a&x=1&status=b&msg=m&msg=n",
			status: "up", msg: "OK", ping: new(7),
			base:   "https://h.test/p",
			keys:   []string{"status", "x", "msg", "ping"},
			values: url.Values{"status": {"up"}, "x": {"1"}, "msg": {"OK"}, "ping": {"7"}},
		},
		{
			name:   "keeps a stray percent literally",
			raw:    "https://h.test/p?a=%zz&b%zz=1+2%",
			status: "up", msg: "OK",
			base:   "https://h.test/p",
			keys:   []string{"a", "b%zz", "status", "msg"},
			values: url.Values{"a": {"%zz"}, "b%zz": {"1 2%"}, "status": {"up"}, "msg": {"OK"}},
		},
		{
			name:   "keeps other params and the fragment",
			raw:    "https://h.test/p?q=a+b&r=%2F&&flag#frag",
			status: "up", msg: "OK",
			base:     "https://h.test/p",
			keys:     []string{"q", "r", "flag", "status", "msg"},
			values:   url.Values{"q": {"a b"}, "r": {"/"}, "flag": {""}, "status": {"up"}, "msg": {"OK"}},
			fragment: "frag",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := RemotePingURL(c.raw, c.status, c.msg, c.ping)
			if err != nil {
				t.Fatal(err)
			}
			u, err := url.Parse(got)
			if err != nil {
				t.Fatal(err)
			}
			if base := u.Scheme + "://" + u.Host + u.EscapedPath(); base != c.base {
				t.Errorf("base = %q, want %q", base, c.base)
			}
			if keys := queryOrder(t, u.RawQuery); !slices.Equal(keys, c.keys) {
				t.Errorf("keys = %q, want %q (%s)", keys, c.keys, got)
			}
			values, err := url.ParseQuery(u.RawQuery)
			if err != nil {
				t.Fatal(err)
			}
			for k, v := range c.values {
				if !slices.Equal(values[k], v) {
					t.Errorf("%s = %q, want %q", k, values[k], v)
				}
			}
			if len(values) != len(c.values) {
				t.Errorf("values = %v, want %v", values, c.values)
			}
			if u.Fragment != c.fragment {
				t.Errorf("fragment = %q, want %q", u.Fragment, c.fragment)
			}
		})
	}
	// url.QueryEscape matches the form encoding of the TS examples.
	got, err := RemotePingURL("https://push.example.test/api/push/token", "down", "Expected HTTP 204, got 200.", nil)
	if err != nil || got != "https://push.example.test/api/push/token?status=down&msg=Expected+HTTP+204%2C+got+200." {
		t.Errorf("RemotePingURL = %q, %v", got, err)
	}
}

func TestRemotePingURLInvalid(t *testing.T) {
	for _, raw := range []string{"", "not a url", "/relative?x=1", "https://SECRET/%zz"} {
		got, err := RemotePingURL(raw, "up", "OK", nil)
		if err == nil {
			t.Errorf("RemotePingURL(%q) = %q, want error", raw, got)
			continue
		}
		if strings.Contains(err.Error(), "SECRET") {
			t.Errorf("error leaks the URL: %v", err)
		}
	}
}

func TestRemoteHost(t *testing.T) {
	cases := map[string]string{
		"https://push.example.test/api/push/token?t=1": "push.example.test",
		"https://push.example.test:8443/x":             "push.example.test:8443",
		"http://[fd00::1]:80/x":                        "[fd00::1]:80",
		"https://SECRET/%zz":                           "<invalid>",
		"not a url":                                    "<invalid>",
	}
	for raw, want := range cases {
		if got := remoteHost(raw); got != want {
			t.Errorf("remoteHost(%q) = %q, want %q", raw, got, want)
		}
	}
}
