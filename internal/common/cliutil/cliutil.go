// Package cliutil holds helpers shared by the command packages of all services.
package cliutil

import "errors"

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
