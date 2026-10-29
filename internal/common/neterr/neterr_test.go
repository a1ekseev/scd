package neterr

import (
	"errors"
	"fmt"
	"net"
	"syscall"
	"testing"
)

func TestStable(t *testing.T) {
	op := func(port int) error {
		return fmt.Errorf("transport: GET: %w", &net.OpError{
			Op: "read", Net: "tcp",
			Source: &net.TCPAddr{IP: net.IPv4(10, 0, 0, 2), Port: port},
			Addr:   &net.TCPAddr{IP: net.IPv4(192, 0, 2, 1), Port: 443},
			Err:    syscall.ECONNRESET,
		})
	}
	a, b := Stable(op(50001)).Error(), Stable(op(50002)).Error()
	if a != b || a != "transport: GET: read tcp 192.0.2.1:443: "+syscall.ECONNRESET.Error() {
		t.Fatalf("%q, %q", a, b)
	}
	plain := errors.New("x")
	if got := Stable(plain); !errors.Is(got, plain) || got.Error() != "x" || Stable(nil) != nil {
		t.Fatal("non-network errors must pass through")
	}
}

func TestStableUnwraps(t *testing.T) {
	inner := &net.OpError{Op: "read", Net: "tcp", Source: &net.TCPAddr{Port: 1}, Addr: &net.TCPAddr{Port: 2}, Err: syscall.ECONNRESET}
	if err := Stable(fmt.Errorf("x: %w", inner)); !errors.Is(err, syscall.ECONNRESET) {
		t.Fatalf("%v does not unwrap", err)
	}
}

func TestStableText(t *testing.T) {
	for in, want := range map[string]string{
		"read tcp 10.0.0.2:5555->10.0.0.5:10085: read: connection reset": "read tcp 10.0.0.5:10085: read: connection reset",
		"write tcp6 [fd00::2]:5555->[fd00::5]:10085: broken pipe":        "write tcp6 [fd00::5]:10085: broken pipe",
		"read udp 10.0.0.2:5555->10.0.0.5:53: i/o timeout":               "read udp 10.0.0.5:53: i/o timeout",
		"dial tcp 10.0.0.5:10085: connect: connection refused":           "dial tcp 10.0.0.5:10085: connect: connection refused",
	} {
		if got := StableText(in); got != want {
			t.Errorf("StableText(%q) = %q, want %q", in, got, want)
		}
	}
}
