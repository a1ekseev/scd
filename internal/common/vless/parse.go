// Package vless parses VLESS share links ("vless://id@host:port?query#label")
// of subscription lines with net/url and explicit rules (D9). It is shared by
// scd and sfc; transport/security semantics are left to the caller.
package vless

import (
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/a1ekseev/scd/internal/common/subscription"
)

// Skip reason codes.
const (
	// CodeUnsupportedScheme marks a line that is not a vless:// URI.
	CodeUnsupportedScheme = "unsupported_scheme"
	// CodeInvalidURI marks a line that is not a valid URI.
	CodeInvalidURI = "invalid_uri"
	// CodeMissingRequiredField marks a link without a required field.
	CodeMissingRequiredField = "missing_required_field"
	// CodeUnsupportedValue marks a parameter value that is not supported.
	CodeUnsupportedValue = "unsupported_value"
	// CodeUnsupportedCombo marks an unsupported type/security/flow combination.
	CodeUnsupportedCombo = "unsupported_combo"
	// CodeDuplicateTag marks a link whose outbound tag is already taken.
	CodeDuplicateTag = "duplicate_tag"
)

// AllowedParams lists the known (lowercased) query parameters, shared by scd
// and sfc (D27 O7): the share-link standard parameters, cs and heartbeatPeriod
// (D33) and the non-standard generator parameters support-x25519mlkem768 and
// x_padding_bytes (O13). Any other parameter is ignored and reported in
// Link.Ignored (D28).
var AllowedParams = []string{
	"type", "security", "encryption", "flow", "sni", "fp", "alpn", "pcs", "vcn", "ech",
	"pbk", "sid", "spx", "pqv", "allowinsecure", "headertype", "host", "path", "mode",
	"extra", "fm", "cs", "heartbeatperiod", "servicename", "authority", "seed", "mtu",
	"tti", "support-x25519mlkem768", "x_padding_bytes",
}

// Param is one decoded query parameter with a lowercased key.
type Param struct {
	Key, Value string
}

// Link is a parsed VLESS share link.
type Link struct {
	// Line is the 1-based line number in the subscription.
	Line int
	// Raw is the untrimmed source line.
	Raw string
	// Label is the percent-decoded fragment ("" without one); invalid UTF-8
	// is replaced with U+FFFD.
	Label string
	// ID is the user id (any non-empty string, D18).
	ID string
	// Address is the host without IPv6 brackets.
	Address string
	// Port is in 1..65535.
	Port int
	// Params are the known parameters in order of first appearance; for a
	// repeated key the last value wins.
	Params []Param
	// Ignored are the unknown parameter keys in order of appearance, without
	// duplicates (D28).
	Ignored []string
}

// Get returns the value of the parameter key (case-insensitive).
func (l Link) Get(key string) (string, bool) {
	key = strings.ToLower(key)
	for _, p := range l.Params {
		if p.Key == key {
			return p.Value, true
		}
	}
	return "", false
}

// Skip describes a subscription line that does not produce a link.
type Skip struct {
	Line    int      `json:"line"`
	Raw     string   `json:"raw"`
	Label   *string  `json:"label,omitempty"`
	Code    string   `json:"reasonCode"`
	Reason  string   `json:"reason"`
	Details []string `json:"details,omitempty"`
}

// Parse parses one subscription line as a VLESS share link. It returns a Skip
// when the line is not a usable vless:// URI; unknown parameters do not skip
// the line but are listed in Link.Ignored.
func Parse(l subscription.Line) (Link, *Skip) {
	skip := func(code, reason string, label *string) (Link, *Skip) {
		return Link{}, &Skip{Line: l.Number, Raw: l.Raw, Label: label, Code: code, Reason: reason}
	}
	trimmed := l.Trimmed
	if !strings.Contains(trimmed, "://") {
		return skip(CodeUnsupportedScheme, "The line does not contain a supported URI scheme.", nil)
	}
	// The fragment is cut off first so that a malformed label gets its own
	// reason instead of failing url.Parse (O11).
	rest, frag, _ := strings.Cut(trimmed, "#")
	u, err := url.Parse(rest)
	if err != nil {
		return skip(CodeInvalidURI, "The line is not a valid URI.", nil)
	}
	if u.Scheme != "vless" {
		return skip(CodeUnsupportedScheme, `Unsupported scheme "`+u.Scheme+`:".`, nil)
	}
	label, err := url.PathUnescape(frag)
	if err != nil {
		return skip(CodeInvalidURI, "The line label is not valid percent-encoding.", nil)
	}
	// The label is cosmetic: invalid UTF-8 (e.g. "%FF") is replaced, not skipped.
	label = strings.ToValidUTF8(label, "\uFFFD")
	// Like TS: only the userinfo user name is the id (a password is dropped)
	// and a URL path ("/p") is ignored.
	id := u.User.Username()
	if id == "" {
		return skip(CodeMissingRequiredField, "The VLESS URI does not contain a user id.", &label)
	}
	// net/url splits host:port at the last colon, so "h:443:80" and an
	// unbracketed IPv6 address would give an ambiguous host; IPv6 zone ids
	// ("[fe80::1%25eth0]") are not a usable outbound address either.
	if host := u.Hostname(); (strings.Contains(host, ":") && !strings.HasPrefix(u.Host, "[")) ||
		strings.Contains(host, "%") {
		return skip(CodeInvalidURI, "The line is not a valid URI.", nil)
	}
	port, err := strconv.Atoi(u.Port())
	if u.Hostname() == "" || err != nil || port < 1 || port > 65535 {
		return skip(CodeMissingRequiredField, "The VLESS URI does not contain a valid address and port.", &label)
	}
	params, ignored := parseQuery(u.RawQuery)
	return Link{
		Line:    l.Number,
		Raw:     l.Raw,
		Label:   label,
		ID:      id,
		Address: u.Hostname(),
		Port:    port,
		Params:  params,
		Ignored: ignored,
	}, nil
}

// parseQuery splits a raw query on "&" only, skipping empty segments. Keys
// are lowercased; a segment that fails to unescape keeps its raw text.
func parseQuery(raw string) (params []Param, ignored []string) {
	for seg := range strings.SplitSeq(raw, "&") {
		if seg == "" {
			continue
		}
		k, v, _ := strings.Cut(seg, "=")
		key := strings.ToLower(unescape(k))
		value := unescape(v)
		if !slices.Contains(AllowedParams, key) {
			if !slices.Contains(ignored, key) {
				ignored = append(ignored, key)
			}
			continue
		}
		if i := slices.IndexFunc(params, func(p Param) bool { return p.Key == key }); i >= 0 {
			params[i].Value = value
			continue
		}
		params = append(params, Param{Key: key, Value: value})
	}
	return params, ignored
}

func unescape(s string) string {
	if d, err := url.QueryUnescape(s); err == nil {
		return d
	}
	return s
}
