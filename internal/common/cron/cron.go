// Package cron parses the cron schedules used by every scheduled loop
// (scd sync and monitoring, sfc refresh, balancer selection) and by config
// validation, so both always agree on what an expression means.
//
// The syntax is exactly what github.com/robfig/cron/v3 accepts with an
// optional leading seconds field and descriptors (D24, D28): classic 5-field
// crontab or 6 fields with leading seconds, lists, ranges, steps, month and
// weekday names, @yearly/@annually/@monthly/@weekly/@daily/@midnight/@hourly,
// and the library extras (?, @every, TZ=/CRON_TZ=). On top of the library
// only two guards are added: an empty expression and an expression that never
// fires are errors. Times are evaluated in time.Local.
package cron

import (
	"errors"
	"time"

	robfig "github.com/robfig/cron/v3"
)

// Schedule yields the activation times of a parsed expression.
type Schedule interface {
	// Next returns the first activation strictly after t, or the zero time
	// when there is none.
	Next(t time.Time) time.Time
}

var parser = robfig.NewParser(
	robfig.SecondOptional | robfig.Minute | robfig.Hour | robfig.Dom | robfig.Month | robfig.Dow | robfig.Descriptor,
)

// neverProbe is the reference instant used to detect expressions that never fire.
var neverProbe = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

var (
	errEmpty = errors.New("cron expression must not be empty")
	errNever = errors.New("cron expression never matches")
)

// Parse parses a cron expression. Library errors are returned with the
// library's text.
func Parse(expr string) (Schedule, error) {
	if expr == "" {
		return nil, errEmpty
	}
	s, err := parser.Parse(expr)
	if err != nil {
		return nil, err
	}
	if s.Next(neverProbe).IsZero() {
		return nil, errNever
	}
	return s, nil
}
