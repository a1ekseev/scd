package state

import "time"

// Dependency health states (DepHealth.State).
const (
	DepHealthy   = "healthy"
	DepUnhealthy = "unhealthy"
	DepUnknown   = "unknown"
)

// SourceWait is the dependency wait of a source with dependsOn (chain
// design §5.1), written by the tunnel monitor. The source waits from the
// tick that found a dependency unhealthy until every dependency is healthy
// and its recoverGraceSeconds pause is over; meanwhile its failed tunnels
// are only repaired (no replacement, no quarantine, no episode without a
// live tunnel, no flaps).
type SourceWait struct {
	// Since is the start of the current wait, nil when the source does not
	// wait.
	Since *time.Time
	// ResetAt is the end of the last wait: the FailuresInRow of the
	// source's tunnels were reset then. A repair series that started
	// before it is not a new series.
	ResetAt *time.Time
	// Deps is the health of each dependency by DependencyConfig.Name (a source
	// id or "balancer:<tag>").
	Deps map[string]*DepHealth
}

// DepHealth is the last known health of one dependency.
type DepHealth struct {
	// State is DepHealthy, DepUnhealthy or DepUnknown (the check failed);
	// "" before the first check.
	State string
	// Since is when State was entered.
	Since *time.Time
	// RecoveredAt is when the dependency became healthy during a wait (the
	// start of its pause) and GraceUntil the end of the pause; both nil
	// otherwise.
	RecoveredAt, GraceUntil *time.Time
}

// SourceError is the current failure of a source in sync, for the status
// API: Code is the reason (fetch_failed or <type>_input_refused,
// manifest_invalid, apply_failed, orphan_remove_failed, or a check code),
// Message its redacted text and At the start of the failure episode.
type SourceError struct {
	Code, Message string
	At            time.Time
}

// SetSourceError records the failure of source id; an episode in progress
// keeps its start. Call it only inside Update.
func (t *Target) SetSourceError(id, code, msg string, now time.Time) {
	if t.SourceErrors == nil {
		t.SourceErrors = map[string]SourceError{}
	}
	at := now
	if prev, ok := t.SourceErrors[id]; ok {
		at = prev.At
	}
	t.SourceErrors[id] = SourceError{Code: code, Message: msg, At: at}
}

// EnsureWait returns the wait state of source id, created on first use.
// Call it only inside Update.
func (t *Target) EnsureWait(id string) *SourceWait {
	w, ok := t.Waits[id]
	if !ok {
		w = &SourceWait{Deps: map[string]*DepHealth{}}
		if t.Waits == nil {
			t.Waits = map[string]*SourceWait{}
		}
		t.Waits[id] = w
	}
	return w
}

// SourceWaiting reports whether the monitor treats source id as waiting:
// it is in a dependency wait, the Xray API of the target is unavailable
// while it has dependencies (their health is unknown), or its last check
// before an apply failed (CheckFailed). Call it inside View or Update.
func (t *Target) SourceWaiting(id string) bool {
	if t.CheckFailed[id] != "" {
		return true
	}
	w := t.Waits[id]
	return w != nil && (w.Since != nil || t.XrayDown != nil)
}
