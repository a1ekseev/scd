// Package listenaddr parses "host:port" listen addresses.
package listenaddr

import (
	"errors"
	"net"
	"strconv"
)

var (
	// ErrFormat reports an address that is not in host:port form.
	ErrFormat = errors.New(`listen address must be in "host:port" format`)
	// ErrPort reports a port that is not an integer in 1..65535.
	ErrPort = errors.New("listen address port must be an integer between 1 and 65535")
)

// Parse splits s into host and port. An empty host (":8080") means all
// interfaces; IPv6 hosts must be written in brackets.
func Parse(s string) (host string, port int, err error) {
	host, portStr, err := net.SplitHostPort(s)
	if err != nil {
		return "", 0, ErrFormat
	}
	port, err = strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return "", 0, ErrPort
	}
	return host, port, nil
}
