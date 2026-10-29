// Package cliutil holds helpers shared by the command packages of all services.
package cliutil

import (
	"errors"
	"log/slog"
)

// ErrReportFailed signals that a report was printed but contains failed items;
// the process must exit non-zero without printing an additional error.
var ErrReportFailed = errors.New("report has failed items")

// InterruptedError signals that a one-shot command was cut short by a
// signal after printing its partial report. The process exits with
// ExitCode without printing an additional error.
type InterruptedError struct {
	// Signal is "SIGINT", "SIGTERM" or another cancellation reason.
	Signal string
}

func (e *InterruptedError) Error() string { return "interrupted by " + e.Signal }

// ExitCode is the shell convention 128+signal: 130 for SIGINT, 143 for
// SIGTERM, otherwise 1.
func (e *InterruptedError) ExitCode() int {
	switch e.Signal {
	case "SIGINT":
		return 130
	case "SIGTERM":
		return 143
	}
	return 1
}

// ErrLogged marks an error that is already in the log: the process exits
// non-zero without printing it again.
var ErrLogged = errors.New("error already logged")

// loggedError is an error marked with ErrLogged; its text is the error's.
type loggedError struct{ error }

func (e loggedError) Is(target error) bool { return target == ErrLogged }
func (e loggedError) Unwrap() error        { return e.error }

// LogFailure logs a non-nil err of a long-running command as
// service_failed and returns it marked with ErrLogged; nil stays nil.
func LogFailure(log *slog.Logger, err error) error {
	if err == nil {
		return nil
	}
	log.Error("Service failed.", "event", "service_failed", "error", err.Error())
	return loggedError{err}
}
