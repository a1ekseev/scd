// Package httpserve holds the HTTP server plumbing shared by the services:
// listener creation, a run loop with graceful shutdown (D7), panic recovery
// (D23) and URL path cleaning that matches WHATWG pathname semantics.
package httpserve

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"path"
	"strings"
	"time"
)

// Listen opens a TCP listener on addr.
func Listen(addr string) (net.Listener, error) {
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", addr, err)
	}
	return ln, nil
}

// Run serves h on ln until ctx is done, then shuts the server down waiting at
// most shutdownTimeout for in-flight requests. It returns nil after a clean
// stop, the serve error if serving failed, or the shutdown error (remaining
// connections are closed) if the timeout elapsed.
func Run(ctx context.Context, ln net.Listener, h http.Handler, shutdownTimeout time.Duration) error {
	// No WriteTimeout: responses (gtl payloads) can be large and slow.
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 60 * time.Second}
	serveErr := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		serveErr <- srv.Serve(ln)
	}()

	select {
	case err := <-serveErr:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
	}

	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		_ = srv.Close()
		<-done
		return fmt.Errorf("shutdown: %w", err)
	}
	<-done
	return nil
}

// Recover turns a panic in h into a 500 response and an error log record
// (event http_handler_panic); the process keeps running (D23). A panic with
// http.ErrAbortHandler is re-raised so net/http aborts the connection. If the
// handler already wrote headers, the 500 cannot replace them.
func Recover(log *slog.Logger, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			rec := recover()
			if rec == nil {
				return
			}
			if rec == http.ErrAbortHandler { //nolint:errorlint // net/http documents comparing this sentinel panic value directly
				panic(rec)
			}
			log.Error("HTTP handler panicked.", "event", "http_handler_panic",
				"panic", fmt.Sprint(rec), "path", r.URL.Path)
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte("internal server error\n"))
		}()
		h.ServeHTTP(w, r)
	})
}

// CleanPath removes dot segments and duplicate slashes from an escaped URL
// path without decoding %xx sequences and keeps a trailing slash, like the
// WHATWG URL pathname (/load/ stays /load/, /a/b/.. becomes /a/, "" becomes /).
func CleanPath(escaped string) string {
	p := "/" + escaped
	cleaned := path.Clean(p)
	if cleaned == "/" {
		return cleaned
	}
	last := p[strings.LastIndexByte(p, '/')+1:]
	if last == "" || last == "." || last == ".." {
		cleaned += "/"
	}
	return cleaned
}
