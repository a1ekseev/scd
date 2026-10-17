package httpserve

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRecoverReturns500(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))
	h := Recover(log, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") }))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/x/y", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("code = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/plain; charset=utf-8" {
		t.Fatalf("content-type = %q", ct)
	}
	if rec.Body.String() != "internal server error\n" {
		t.Fatalf("body = %q", rec.Body.String())
	}
	for _, s := range []string{`"event":"http_handler_panic"`, `"panic":"boom"`, `"path":"/x/y"`, `"level":"ERROR"`} {
		if !strings.Contains(buf.String(), s) {
			t.Errorf("log missing %s: %s", s, buf.String())
		}
	}
}

func TestRecoverPassesThrough(t *testing.T) {
	h := Recover(slog.New(slog.DiscardHandler), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil))
	if rec.Code != http.StatusTeapot {
		t.Fatalf("code = %d", rec.Code)
	}
}

func TestRecoverRepanicsAbortHandler(t *testing.T) {
	h := Recover(slog.New(slog.DiscardHandler), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic(http.ErrAbortHandler)
	}))
	defer func() {
		if r := recover(); !errors.Is(asErr(r), http.ErrAbortHandler) {
			t.Fatalf("recover = %v", r)
		}
	}()
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil))
}

func TestRunShutsDownOnCancel(t *testing.T) {
	ln, err := Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		_, _ = io.WriteString(w, "done")
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- Run(ctx, ln, h, 5*time.Second) }()

	type result struct {
		body string
		err  error
	}
	res := make(chan result, 1)
	go func() {
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+ln.Addr().String()+"/", nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			res <- result{err: err}
			return
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		res <- result{string(b), err}
	}()
	<-started
	cancel()
	select {
	case err := <-runErr:
		t.Fatalf("Run returned before in-flight request finished: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	if r := <-res; r.err != nil || r.body != "done" {
		t.Fatalf("response = %+v", r)
	}
	if err := <-runErr; err != nil {
		t.Fatalf("Run = %v, want nil", err)
	}
}

func TestRunReturnsServeError(t *testing.T) {
	ln, err := Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_ = ln.Close()
	if err := Run(t.Context(), ln, http.NotFoundHandler(), time.Second); err == nil {
		t.Fatal("expected error from closed listener")
	}
}

func TestRunShutdownTimeout(t *testing.T) {
	ln, err := Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	h := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		close(started)
		<-release
	})
	ctx, cancel := context.WithCancel(t.Context())
	runErr := make(chan error, 1)
	go func() { runErr <- Run(ctx, ln, h, 50*time.Millisecond) }()
	go func() {
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+ln.Addr().String()+"/", nil)
		if resp, err := http.DefaultClient.Do(req); err == nil {
			resp.Body.Close()
		}
	}()
	<-started
	cancel()
	if err := <-runErr; err == nil {
		t.Fatal("expected shutdown timeout error")
	}
}

func TestListenError(t *testing.T) {
	if _, err := Listen("256.0.0.1:99999"); err == nil {
		t.Fatal("expected error")
	}
}

func TestCleanPathKeepsTrailingSlash(t *testing.T) {
	tests := []struct{ in, want string }{
		{"/load/", "/load/"},
		{"/load/1/", "/load/1/"},
		{"/a/../load/1", "/load/1"},
		{"/s/../s/test-all", "/s/test-all"},
		{"", "/"},
		{"/", "/"},
		{"/a/./b", "/a/b"},
		{"/a//b", "/a/b"},
		{"/a/b/..", "/a/"},
		{"/a/.", "/a/"},
		{"/..", "/"},
		{"/../../x", "/x"},
		{"/a%2Fb/../c", "/c"},
		{"/a%2e%2e/b", "/a%2e%2e/b"},
		{"no-leading", "/no-leading"},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			if got := CleanPath(tc.in); got != tc.want {
				t.Fatalf("CleanPath(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func asErr(v any) error {
	err, _ := v.(error)
	return err
}
