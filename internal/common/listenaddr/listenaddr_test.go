package listenaddr

import (
	"errors"
	"testing"
)

func TestParseListen(t *testing.T) {
	tests := []struct {
		in   string
		host string
		port int
		err  error
	}{
		{"127.0.0.1:9090", "127.0.0.1", 9090, nil},
		{"[::]:8080", "::", 8080, nil},
		{":8080", "", 8080, nil},
		{"localhost", "", 0, ErrFormat},
		{"::1:80", "", 0, ErrFormat},
		{"h:0", "", 0, ErrPort},
		{"h:65536", "", 0, ErrPort},
		{"h:0x50", "", 0, ErrPort},
		{"h: 80", "", 0, ErrPort},
		{"h:", "", 0, ErrPort},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			host, port, err := Parse(tc.in)
			if !errors.Is(err, tc.err) {
				t.Fatalf("err = %v, want %v", err, tc.err)
			}
			if host != tc.host || port != tc.port {
				t.Fatalf("got (%q, %d), want (%q, %d)", host, port, tc.host, tc.port)
			}
		})
	}
}

func TestErrorTexts(t *testing.T) {
	if got, want := ErrFormat.Error(), `listen address must be in "host:port" format`; got != want {
		t.Fatalf("got %q", got)
	}
	if got, want := ErrPort.Error(), "listen address port must be an integer between 1 and 65535"; got != want {
		t.Fatalf("got %q", got)
	}
}
