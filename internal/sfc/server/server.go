// Package server serves the filtered subscription outputs of sfc over HTTP.
package server

import (
	"encoding/base64"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/a1ekseev/scd/internal/common/httpserve"
	"github.com/a1ekseev/scd/internal/sfc/refresh"
)

const contentType = "text/plain; charset=utf-8"

// New returns the public handler: /healthz, the output routes of st and 404
// for everything else. The request method is not checked.
func New(st *refresh.State) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := httpserve.CleanPath(r.URL.EscapedPath())
		if p == "/healthz" {
			reply(w, http.StatusOK, "ok\n")
			return
		}
		out, ok := st.Lookup(p)
		switch {
		case !ok:
			reply(w, http.StatusNotFound, "not found\n")
		case out.LastGoodBase64 == "":
			reply(w, http.StatusBadGateway, "output is not available yet\n")
		default:
			setProfileHeaders(w.Header(), out, r.UserAgent())
			reply(w, http.StatusOK, out.LastGoodBase64)
		}
	})
}

// setProfileHeaders adds the profile headers when ua starts with one of the
// configured User-Agent prefixes of out (case-sensitive). Header names are
// lower case, as in the TS service.
func setProfileHeaders(h http.Header, out *refresh.OutputState, ua string) {
	if ua == "" {
		return
	}
	matched := false
	for _, prefix := range out.UserAgent {
		if strings.HasPrefix(ua, prefix) {
			matched = true
			break
		}
	}
	if !matched {
		return
	}
	if out.ProfileTitle != "" {
		h["profile-title"] = []string{"base64:" + base64.StdEncoding.EncodeToString([]byte(out.ProfileTitle))}
	}
	if out.ProfileUpdateInterval != 0 { // 0 means unset
		h["profile-update-interval"] = []string{strconv.Itoa(out.ProfileUpdateInterval)}
	}
}

func reply(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}
