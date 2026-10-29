package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/a1ekseev/scd/internal/common/wgpstatus"
)

func TestServer(t *testing.T) {
	at := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	st := wgpstatus.Status{Version: 1, UpdatedAt: at, Providers: []wgpstatus.Provider{
		{ID: "a", Output: "a", Type: "x", State: "ok", LastTickAt: at, Issued: 2, Limit: 10},
		{ID: "b", Output: "b", Type: "y", State: "error", Error: `<script>output "b": no candidates`, LastTickAt: at},
	}}
	srv := httptest.NewServer(New(func() wgpstatus.Status { return st }))
	defer srv.Close()

	get := func(method, path string) (int, string, string) {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), method, srv.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, resp.Header.Get("Content-Type"), string(b)
	}

	if code, _, body := get("GET", "/healthz"); code != 200 || body != "ok\n" {
		t.Fatalf("healthz %d %q", code, body)
	}
	code, ct, body := get("GET", "/api/status")
	var got wgpstatus.Status
	if code != 200 || !strings.HasPrefix(ct, "application/json") || json.Unmarshal([]byte(body), &got) != nil ||
		len(got.Providers) != 2 || got.Providers[1].Error != st.Providers[1].Error {
		t.Fatalf("api %d %s %q", code, ct, body)
	}
	code, ct, body = get("GET", "/")
	for _, want := range []string{"<h1>wgp</h1>", "2 of 10", `class="bad">error`, "&lt;script&gt;output"} {
		if code != 200 || !strings.HasPrefix(ct, "text/html") || !strings.Contains(body, want) {
			t.Errorf("page %d %s lacks %q:\n%s", code, ct, want, body)
		}
	}
	for path, want := range map[string]int{"/nope": 404, "/api/status/x": 404} {
		if code, _, _ := get("GET", path); code != want {
			t.Errorf("GET %s: %d", path, code)
		}
	}
	if code, _, _ := get("POST", "/api/status"); code != 405 {
		t.Errorf("POST: %d", code)
	}
}

func TestPageEmpty(t *testing.T) {
	srv := httptest.NewServer(New(func() wgpstatus.Status { return wgpstatus.Status{Version: 1, Providers: []wgpstatus.Provider{}} }))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/") //nolint:noctx // test against a local server
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if b, _ := io.ReadAll(resp.Body); !strings.Contains(string(b), "No provider has ticked yet.") {
		t.Fatalf("page %q", b)
	}
}
