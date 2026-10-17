package inputsrc

import (
	"net/url"
	"strings"
)

// RedactInput hides the secret part of a subscription input (D33): an
// http(s) URL becomes RedactURL(input); a file path or "-" is returned
// unchanged.
func RedactInput(input string) string {
	if !httpRe.MatchString(input) {
		return input
	}
	return RedactURL(input)
}

// RedactURL hides everything after the authority of a URL of any scheme:
// "<scheme>://<host>/<redacted>" with the host and port as written (path,
// query, fragment and userinfo dropped), or "<redacted>" when value does not
// parse or has no scheme or host (D9: no WHATWG "null" origin).
func RedactURL(value string) string {
	u, err := url.Parse(value)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return redacted
	}
	return u.Scheme + "://" + u.Host + "/" + redacted
}

// RedactInputIn replaces every occurrence of input in text (such as a Load
// error) with RedactInput(input).
func RedactInputIn(text, input string) string {
	if input == "" {
		return text
	}
	r := RedactInput(input)
	if r == input {
		return text
	}
	return strings.ReplaceAll(text, input, r)
}
