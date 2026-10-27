package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/a1ekseev/scd/internal/gtl/config"
	"github.com/a1ekseev/scd/internal/gtl/payload"
)

func cfgWith(path string, maxKb int64) config.Config {
	var c config.Config
	c.Load.Path = path
	c.Load.MaxSizeKb = maxKb
	return c
}

type route struct {
	Path      string            `json:"path"`
	Method    string            `json:"method"`
	Status    int               `json:"status"`
	Headers   map[string]string `json:"headers"`
	Body      *string           `json:"body"`
	BodyBytes *int              `json:"bodyBytes"`
}

func do(h http.Handler, method, target string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), method, target, nil))
	return rec
}

func TestRoutesReference(t *testing.T) {
	data, err := os.ReadFile("../../../testdata/reference/gtl/routes.json")
	if err != nil {
		t.Fatal(err)
	}
	var routes []route
	if err := json.Unmarshal(data, &routes); err != nil {
		t.Fatal(err)
	}
	h := New(cfgWith("/load", 64))
	for _, rt := range routes {
		t.Run(rt.Method+" "+rt.Path, func(t *testing.T) {
			rec := do(h, rt.Method, rt.Path)
			if rec.Code != rt.Status {
				t.Fatalf("status %d want %d", rec.Code, rt.Status)
			}
			for k, v := range rt.Headers {
				if got := rec.Header().Get(k); got != v {
					t.Errorf("header %s = %q want %q", k, got, v)
				}
			}
			body := rec.Body.Bytes()
			if rt.Body != nil && string(body) != *rt.Body {
				t.Errorf("body %q want %q", body, *rt.Body)
			}
			if rt.BodyBytes != nil {
				if len(body) != *rt.BodyBytes {
					t.Fatalf("body len %d want %d", len(body), *rt.BodyBytes)
				}
				var want bytes.Buffer
				if err := payload.Write(&want, uint64(*rt.BodyBytes)); err != nil {
					t.Fatal(err)
				}
				if sha256.Sum256(body) != sha256.Sum256(want.Bytes()) {
					t.Error("body sha256 differs from generator")
				}
			}
		})
	}
}

func TestRouting(t *testing.T) {
	dl := New(cfgWith("/download", 64))
	ld := New(cfgWith("/load", 64))
	cases := []struct {
		name   string
		h      http.Handler
		method string
		target string
		status int
	}{
		{"custom path", dl, "GET", "/download/32", 200},
		{"no alias", dl, "GET", "/load/32", 404},
		{"dot segments", ld, "GET", "/a/../load/1", 200},
		{"percent not decoded", ld, "GET", "/load/%33", 400},
		{"empty tail", ld, "GET", "/load/", 400},
		{"trailing slash", ld, "GET", "/load/1/", 400},
		{"head healthz", ld, "HEAD", "/healthz", 405},
		{"post", ld, "POST", "/load/1", 405},
		{"unknown", ld, "GET", "/unknown", 404},
		{"overflow", ld, "GET", "/load/99999999999999999999", 400},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := do(c.h, c.method, c.target).Code; got != c.status {
				t.Fatalf("status %d want %d", got, c.status)
			}
		})
	}
}

func TestLoadBodyMatchesGenerator(t *testing.T) {
	rec := do(New(cfgWith("/load", 1024)), "GET", "/load/100")
	var want bytes.Buffer
	if err := payload.Write(&want, 100*1024); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(rec.Body.Bytes(), want.Bytes()) {
		t.Fatal("body differs from generator output")
	}
	if rec.Header().Get("Content-Length") != strconv.Itoa(100*1024) {
		t.Fatal("bad content-length")
	}
}

func TestStopsOnClientDisconnect(t *testing.T) {
	done := make(chan struct{})
	h := New(cfgWith("/load", 1<<20))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(done)
		h.ServeHTTP(w, r)
	}))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/load/1048576", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.CopyN(io.Discard, resp.Body, 1024)
	cancel()
	_ = resp.Body.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not stop after client disconnect")
	}
}
