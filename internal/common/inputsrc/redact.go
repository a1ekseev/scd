package inputsrc

import (
	"net/url"
	"strings"
)

// RedactInput hides the secret part of a subscription input (D33): an
// http(s) URL becomes "<scheme>://<host>/<redacted>" (path, query, fragment
// and userinfo dropped) or "<redacted>" when it cannot be parsed; a file
// path or "-" is returned unchanged.
func RedactInput(input string) string {
	if !httpRe.MatchString(input) {
		return input
	}
	u, err := url.Parse(input)
	if err != nil || u.Host == "" {
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
