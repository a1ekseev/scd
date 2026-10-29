package cliutil

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

func TestInterruptedExitCode(t *testing.T) {
	for sig, want := range map[string]int{"SIGINT": 130, "SIGTERM": 143, "context canceled": 1} {
		if got := (&InterruptedError{Signal: sig}).ExitCode(); got != want {
			t.Errorf("%s: exit code %d, want %d", sig, got, want)
		}
	}
}

func TestLogFailure(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))
	if LogFailure(log, nil) != nil || buf.Len() != 0 {
		t.Fatalf("nil: %q", buf.String())
	}
	cause := errors.New("listen tcp: address in use")
	err := LogFailure(log, cause)
	if !errors.Is(err, ErrLogged) || !errors.Is(err, cause) || err.Error() != cause.Error() {
		t.Fatalf("err %v", err)
	}
	if !strings.Contains(buf.String(), `"event":"service_failed"`) || !strings.Contains(buf.String(), `"error":"listen tcp: address in use"`) {
		t.Fatalf("log %q", buf.String())
	}
}
