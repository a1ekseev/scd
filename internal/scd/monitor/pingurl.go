package monitor

import (
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
)

// RemotePingURL returns the push URL for raw (03 §4.6, TS buildRemotePingUrl):
// status and msg are set, ping is removed and, when pingMs is not nil,
// appended last. Setting a key replaces its first occurrence in place and
// drops later ones; a missing key is appended; other parameters are kept in
// order. The query is re-encoded with url.QueryEscape (space → "+"); the
// byte form of WHATWG URLSearchParams is not emulated (D9). Errors never
// contain raw, which usually holds a token.
func RemotePingURL(raw, status, msg string, pingMs *int) (string, error) {
	u, err := parseAbsolute(raw)
	if err != nil {
		return "", err
	}
	q := parseQuery(u.RawQuery)
	q = q.set("status", status)
	q = q.set("msg", msg)
	q = slices.DeleteFunc(q, func(p queryPair) bool { return p.key == "ping" })
	if pingMs != nil {
		q = q.set("ping", strconv.Itoa(*pingMs))
	}
	u.RawQuery = q.encode()
	u.ForceQuery = false
	return u.String(), nil
}

// remoteHost is the host[:port] of a push URL for logs, or "<invalid>"
// (TS getRemoteHost).
func remoteHost(raw string) string {
	u, err := parseAbsolute(raw)
	if err != nil {
		return "<invalid>"
	}
	return u.Host
}

// parseAbsolute parses an absolute URL; the error does not quote raw.
func parseAbsolute(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		if ue, ok := errors.AsType[*url.Error](err); ok {
			err = ue.Err
		}
		return nil, fmt.Errorf("invalid remote ping URL: %w", err)
	}
	if !u.IsAbs() {
		return nil, errors.New("invalid remote ping URL: not an absolute URL")
	}
	return u, nil
}

type queryPair struct{ key, value string }

// query is an ordered, decoded query string.
type query []queryPair

// parseQuery splits raw on "&" keeping order and duplicates; empty parts
// are skipped and a part without "=" has an empty value. A key or value
// with a stray "%" (invalid escape) is kept literally, as WHATWG
// URLSearchParams does, so it is re-encoded as "%25…".
func parseQuery(raw string) query {
	var q query
	for part := range strings.SplitSeq(raw, "&") {
		if part == "" {
			continue
		}
		k, v, _ := strings.Cut(part, "=")
		q = append(q, queryPair{unescapeLenient(k), unescapeLenient(v)})
	}
	return q
}

// unescapeLenient is url.QueryUnescape that keeps s literally ("+" still
// meaning a space) when it has an invalid escape.
func unescapeLenient(s string) string {
	if d, err := url.QueryUnescape(s); err == nil {
		return d
	}
	return strings.ReplaceAll(s, "+", " ")
}

// set is URLSearchParams.set: the first occurrence of key takes value in
// place and later ones are removed; without one the pair is appended.
func (q query) set(key, value string) query {
	i := slices.IndexFunc(q, func(p queryPair) bool { return p.key == key })
	if i < 0 {
		return append(q, queryPair{key, value})
	}
	q[i].value = value
	out := q[:i+1]
	for _, p := range q[i+1:] {
		if p.key != key {
			out = append(out, p)
		}
	}
	return out
}

func (q query) encode() string {
	var b strings.Builder
	for i, p := range q {
		if i > 0 {
			b.WriteByte('&')
		}
		b.WriteString(url.QueryEscape(p.key))
		b.WriteByte('=')
		b.WriteString(url.QueryEscape(p.value))
	}
	return b.String()
}
