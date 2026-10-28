// Package server implements the gtl HTTP handler.
package server

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/a1ekseev/scd/internal/common/httpserve"
	"github.com/a1ekseev/scd/internal/gtl/config"
	"github.com/a1ekseev/scd/internal/gtl/payload"
)

// New returns the gtl handler: GET /healthz and GET <load.path>/:sizeKb.
// Each handler has its own random payload key (see payload.New).
func New(cfg config.Config) http.Handler {
	prefix := cfg.Load.Path + "/"
	maxKb := uint64(cfg.Load.MaxSizeKb)
	src := payload.New()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			text(w, http.StatusMethodNotAllowed, "method not allowed\n")
			return
		}
		p := httpserve.CleanPath(r.URL.EscapedPath())
		if p == "/healthz" {
			text(w, http.StatusOK, "ok")
			return
		}
		tail, ok := strings.CutPrefix(p, prefix)
		if !ok {
			text(w, http.StatusNotFound, "not found\n")
			return
		}
		if !isDigits(tail) {
			text(w, http.StatusBadRequest, "invalid load route, expected "+cfg.Load.Path+"/:sizeKb\n")
			return
		}
		kb, err := strconv.ParseUint(tail, 10, 64)
		if err != nil || kb > maxKb {
			text(w, http.StatusBadRequest, "bad request\n")
			return
		}
		h := w.Header()
		h.Set("Content-Type", "application/octet-stream")
		h.Set("Content-Length", strconv.FormatUint(kb*1024, 10))
		w.WriteHeader(http.StatusOK)
		// Stop as soon as the client disconnects (write error or cancelled context).
		_ = src.Write(&ctxWriter{r: r, w: w}, kb*1024)
	})
}

type ctxWriter struct {
	r *http.Request
	w http.ResponseWriter
}

func (c *ctxWriter) Write(b []byte) (int, error) {
	if err := c.r.Context().Err(); err != nil {
		return 0, err
	}
	return c.w.Write(b)
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func text(w http.ResponseWriter, status int, body string) {
	h := w.Header()
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}
