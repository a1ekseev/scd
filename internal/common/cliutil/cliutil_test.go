package cliutil

import "testing"

func TestInterruptedExitCode(t *testing.T) {
	for sig, want := range map[string]int{"SIGINT": 130, "SIGTERM": 143, "context canceled": 1} {
		if got := (&InterruptedError{Signal: sig}).ExitCode(); got != want {
			t.Errorf("%s: exit code %d, want %d", sig, got, want)
		}
	}
}
