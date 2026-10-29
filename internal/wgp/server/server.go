// Package server serves the local wgp status page for the owner: GET
// /healthz, GET /api/status (the status.json document) and GET / (the same
// as HTML). scd never uses it; it reads status.json from the shared volume.
package server

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"html/template"
	"net/http"

	"github.com/a1ekseev/scd/internal/common/httpserve"
	"github.com/a1ekseev/scd/internal/common/wgpstatus"
)

//go:embed page.html.tmpl
var pageSource string

var page = template.Must(template.New("page").Parse(pageSource))

// New returns the handler; status returns the current status document.
// Other paths are 404, other methods on known paths 405.
func New(status func() wgpstatus.Status) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := httpserve.CleanPath(r.URL.EscapedPath())
		if p != "/" && p != "/healthz" && p != "/api/status" {
			reply(w, http.StatusNotFound, "text/plain; charset=utf-8", []byte("not found\n"))
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			reply(w, http.StatusMethodNotAllowed, "text/plain; charset=utf-8", []byte("method not allowed\n"))
			return
		}
		switch p {
		case "/healthz":
			reply(w, http.StatusOK, "text/plain; charset=utf-8", []byte("ok\n"))
		case "/api/status":
			b, err := json.MarshalIndent(status(), "", "  ")
			if err != nil {
				panic(err) // a plain struct always encodes; Recover turns it into a 500
			}
			reply(w, http.StatusOK, "application/json; charset=utf-8", append(b, '\n'))
		default:
			var buf bytes.Buffer
			if err := page.Execute(&buf, status()); err != nil {
				panic(err)
			}
			reply(w, http.StatusOK, "text/html; charset=utf-8", buf.Bytes())
		}
	})
}

func reply(w http.ResponseWriter, code int, contentType string, body []byte) {
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_, _ = w.Write(body)
}
