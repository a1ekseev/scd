// Package cliutil holds helpers shared by the command packages of all services.
package cliutil

import "errors"

// ErrReportFailed signals that a report was printed but contains failed items;
// the process must exit non-zero without printing an additional error.
var ErrReportFailed = errors.New("report has failed items")
