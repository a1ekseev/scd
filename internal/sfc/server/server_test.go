package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/a1ekseev/scd/internal/sfc/config"
	"github.com/a1ekseev/scd/internal/sfc/refresh"
)

const repoRoot = "../../.."

const fixtureLine = "vless://00000000-0000-4000-8000-000000000001@de.example.com:443?type=tcp&security=tls#Germany"

const testConfig = `
subscriptions:
  - id: main
    input: sub.txt
    pathRoute: /profiles
    outputs:
      - id: available
        labelIncludeRegex: '/Germany/'
        userAgent: [Clash, Stash]
        profileTitle: Available Profile
        profileUpdateInterval: 6
      - id: plain
        labelIncludeRegex: '/Germany/'
      - id: noua
        labelIncludeRegex: '/Germany/'
        profileTitle: Hidden
        profileUpdateInterval: 3
      - id: titleonly
        labelIncludeRegex: '/Germany/'
        userAgent: [Clash]
        profileTitle: T
      - id: empty
        labelIncludeRegex: '/Nothing/'
      - id: off
        enabled: false
        labelIncludeRegex: '/.*/'
  - id: other
    input: sub.txt
    pathRoute: /s
    outputs:
      - id: elsewhere
        labelIncludeRegex: '/.*/'
runtime:
  refreshSchedule: "*/10 * * * *"
server:
  listen: 127.0.0.1:8080
`

// refreshed loads yaml (input files live next to it) and runs one refresh.
func refreshed(t *testing.T, yaml string) *refresh.State {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "sub.txt"), []byte(fixtureLine+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "config.yml")
	if err := os.WriteFile(p, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	st := refresh.NewState(cfg.Config)
	(&refresh.Refresher{Cfg: cfg, State: st}).Run(t.Context())
	return st
}

type response struct {
	status int
	ct     string
	body   string
	h      http.Header
}

func do(t *testing.T, h http.Handler, method, target, ua string) response {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), method, target, nil)
	if ua != "" {
		req.Header.Set("User-Agent", ua)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	b, _ := io.ReadAll(rec.Result().Body)
	return response{rec.Code, rec.Header().Get("Content-Type"), string(b), rec.Header()}
}

func profileHeaders(h http.Header) map[string]string {
	m := map[string]string{}
	for _, k := range []string{"profile-title", "profile-update-interval"} {
		if v, ok := h[k]; ok {
			m[k] = v[0]
		}
	}
	return m
}

func TestHealthz(t *testing.T) {
	r := do(t, New(refreshed(t, testConfig)), "GET", "/healthz", "")
	if r.status != 200 || r.body != "ok\n" || r.ct != contentType {
		t.Fatalf("got %+v", r)
	}
}

func TestUnknownIDUnderRoute404(t *testing.T) {
	r := do(t, New(refreshed(t, testConfig)), "GET", "/profiles/unknown", "")
	if r.status != 404 || r.body != "not found\n" || r.ct != contentType {
		t.Fatalf("got %+v", r)
	}
}

func TestNoPayload502(t *testing.T) {
	r := do(t, New(refreshed(t, testConfig)), "GET", "/profiles/empty", "")
	if r.status != 502 || r.body != "output is not available yet\n" || r.ct != contentType {
		t.Fatalf("got %+v", r)
	}
}

func TestDisabledOutput502(t *testing.T) {
	r := do(t, New(refreshed(t, testConfig)), "GET", "/profiles/off", "")
	if r.status != 502 || r.body != "output is not available yet\n" {
		t.Fatalf("got %+v", r)
	}
}

func TestPayloadNoUA(t *testing.T) {
	st := refreshed(t, testConfig)
	o, _ := st.Lookup("/profiles/available")
	r := do(t, New(st), "GET", "/profiles/available", "")
	if r.status != 200 || r.body != o.LastGoodBase64 || r.body == "" || r.ct != contentType {
		t.Fatalf("got %+v", r)
	}
	if len(profileHeaders(r.h)) != 0 {
		t.Fatalf("headers: %v", r.h)
	}
}

func TestProfileHeadersOnUAPrefix(t *testing.T) {
	r := do(t, New(refreshed(t, testConfig)), "GET", "/profiles/available", "ClashMeta/1.0")
	want := map[string]string{"profile-title": "base64:QXZhaWxhYmxlIFByb2ZpbGU=", "profile-update-interval": "6"}
	got := profileHeaders(r.h)
	if r.status != 200 || len(got) != 2 || got["profile-title"] != want["profile-title"] || got["profile-update-interval"] != "6" {
		t.Fatalf("got %+v %v", r, got)
	}
}

func TestUnsetIntervalOmitted(t *testing.T) {
	r := do(t, New(refreshed(t, testConfig)), "GET", "/profiles/titleonly", "Clash")
	got := profileHeaders(r.h)
	if len(got) != 1 || got["profile-title"] != "base64:VA==" {
		t.Fatalf("got %v", got)
	}
}

func TestNoHeadersOnUAMismatch(t *testing.T) {
	h := New(refreshed(t, testConfig))
	for _, ua := range []string{"Shadowrocket/1.0", "clashmeta"} {
		r := do(t, h, "GET", "/profiles/available", ua)
		if r.status != 200 || len(profileHeaders(r.h)) != 0 {
			t.Fatalf("ua %q: %+v", ua, r)
		}
	}
}

func TestNoHeadersWithoutUserAgentConfig(t *testing.T) {
	r := do(t, New(refreshed(t, testConfig)), "GET", "/profiles/noua", "ClashMeta/1.0")
	if r.status != 200 || len(profileHeaders(r.h)) != 0 {
		t.Fatalf("got %+v", r)
	}
}

func TestOtherRoute404(t *testing.T) {
	h := New(refreshed(t, testConfig))
	for _, p := range []string{"/s/available", "/profiles/elsewhere", "/profiles", "/profiles/available/"} {
		if r := do(t, h, "GET", p, ""); r.status != 404 {
			t.Fatalf("%s: %+v", p, r)
		}
	}
}

func TestMethodNotChecked(t *testing.T) {
	h := New(refreshed(t, testConfig))
	if r := do(t, h, "POST", "/profiles/plain", ""); r.status != 200 {
		t.Fatalf("got %+v", r)
	}
}

type serverCase struct {
	Name        string            `json:"name"`
	Path        string            `json:"path"`
	UserAgent   string            `json:"userAgent"`
	Status      int               `json:"status"`
	ContentType string            `json:"contentType"`
	Body        string            `json:"body"`
	Headers     map[string]string `json:"headers"`
}

func TestServerCasesReference(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(repoRoot, "testdata/reference/sfc/server-cases.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cases []serverCase
	if err := json.Unmarshal(b, &cases); err != nil {
		t.Fatal(err)
	}
	fixture, err := filepath.Abs(filepath.Join(repoRoot, "docker/fixtures/subscription.b64"))
	if err != nil {
		t.Fatal(err)
	}
	ex, err := os.ReadFile(filepath.Join(repoRoot, "docker/sfc/config.example.yml"))
	if err != nil {
		t.Fatal(err)
	}
	yaml := strings.Replace(string(ex), "input: ../fixtures/subscription.b64", "input: "+fixture, 1)
	h := New(refreshed(t, yaml))
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			r := do(t, h, "GET", c.Path, c.UserAgent)
			if r.status != c.Status || r.body != c.Body || r.ct != c.ContentType {
				t.Fatalf("got %d %q %q, want %d %q %q", r.status, r.ct, r.body, c.Status, c.ContentType, c.Body)
			}
			got := profileHeaders(r.h)
			if len(got) != len(c.Headers) {
				t.Fatalf("headers %v, want %v", got, c.Headers)
			}
			for k, v := range c.Headers {
				if got[k] != v {
					t.Fatalf("header %s = %q, want %q", k, got[k], v)
				}
			}
		})
	}
}
