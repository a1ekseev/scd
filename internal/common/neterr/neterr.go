// Package neterr keeps network error texts the same from attempt to
// attempt, so edge-triggered logs see one failure, not a new one each time.
package neterr

import (
	"errors"
	"net"
	"regexp"
	"strings"
)

// Stable returns err with the local address of its first *net.OpError
// ("<local>->", an ephemeral port that changes with every connection)
// dropped from the text; Op, Net, the remote Addr and Err stay, and the
// result still unwraps to err. Other errors are returned as they are.
func Stable(err error) error {
	oe, ok := errors.AsType[*net.OpError](err)
	if !ok || oe.Source == nil || oe.Addr == nil {
		return err
	}
	src := oe.Source.String() + "->"
	if !strings.Contains(err.Error(), src) {
		return err
	}
	return stableError{err, strings.Replace(err.Error(), src, "", 1)}
}

// stableError is err with the text msg.
type stableError struct {
	err error
	msg string
}

func (e stableError) Error() string { return e.msg }
func (e stableError) Unwrap() error { return e.err }

// localAddr is the "<local>->" part of a network error text.
var localAddr = regexp.MustCompile(`\b((?:tcp|udp)[46]?) \S+?->`)

// StableText is Stable for an error that exists only as text (a gRPC
// status message): "read tcp <local>-><remote>: …" loses "<local>->"
// (up to the first "->", so the remote address stays).
func StableText(msg string) string {
	return localAddr.ReplaceAllString(msg, "${1} ")
}
