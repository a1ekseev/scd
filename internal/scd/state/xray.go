package state

import (
	"context"
	"log/slog"
	"time"

	"github.com/a1ekseev/scd/internal/scd/xrayapi"
)

// XrayIssue is an unavailable Xray API of a target: the last error text
// and when the episode started.
type XrayIssue struct {
	Error string
	Since time.Time
}

// SetXrayDown records that the Xray API of t does not answer. It returns
// true only when the episode starts; a new text is kept without a new
// episode. It must run inside Update.
func (t *Target) SetXrayDown(msg string, now time.Time) (first bool) {
	if t.XrayDown != nil {
		t.XrayDown.Error = msg
		return false
	}
	t.XrayDown = &XrayIssue{Error: msg, Since: now}
	return true
}

// ClearXrayDown ends the episode of SetXrayDown and returns its length;
// was is false when there was none. It must run inside Update.
func (t *Target) ClearXrayDown(now time.Time) (failedFor time.Duration, was bool) {
	if t.XrayDown == nil {
		return 0, false
	}
	failedFor, t.XrayDown = now.Sub(t.XrayDown.Since), nil
	return failedFor, true
}

// SetStateLost marks source sourceID of t as missing its managed
// resources in Xray (a marked source keeps its first mark; the hashes are
// not touched). first is true for a new mark while no loss is logged at
// warn: the start of an episode, or a new loss after a sync restored some
// sources while others stay marked (a source that never loads must not
// hide the next Xray restart). It must run inside Update.
func (t *Target) SetStateLost(sourceID string, now time.Time) (first bool) {
	if _, ok := t.StateLost[sourceID]; ok {
		return false
	}
	if len(t.StateLost) == 0 {
		t.StateLost, t.StateLostSince = map[string]time.Time{}, now
	}
	t.StateLost[sourceID] = now
	first, t.StateLostLogged = !t.StateLostLogged, true
	return first
}

// ClearStateLost unmarks source sourceID when it was marked before before
// (a later mark is not covered by the apply that clears it); the next new
// mark is warn again. restored reports that no source is marked any more,
// lostFor is then the length of the episode. It must run inside Update.
func (t *Target) ClearStateLost(sourceID string, before, now time.Time) (lostFor time.Duration, restored bool) {
	at, ok := t.StateLost[sourceID]
	if !ok || !at.Before(before) {
		return 0, false
	}
	delete(t.StateLost, sourceID)
	t.StateLostLogged = false
	if len(t.StateLost) > 0 {
		return 0, false
	}
	return now.Sub(t.StateLostSince), true
}

// NoteXray records the result of an Xray API call of the target of key
// and logs the change of its availability: xray_unavailable at the start
// of an episode (err is xrayapi.IsUnavailable), xray_recovered on the
// first success (err is nil) after it. Other errors change nothing. down
// and recovered tell the caller to log its own event of this call at debug:
// the target-level line already covers it. It locks m itself, so it must
// not run inside View or Update.
func NoteXray(ctx context.Context, m *Memory, log *slog.Logger, subID, addr string, err error, now time.Time) (down, recovered bool) {
	key := Key(subID, addr)
	switch {
	case err == nil:
		var failedFor time.Duration
		m.Update(key, func(t *Target) { failedFor, recovered = t.ClearXrayDown(now) })
		if recovered {
			log.LogAttrs(ctx, slog.LevelInfo, "Xray API of the target answers again.",
				slog.String("event", "xray_recovered"),
				slog.String("subscriptionId", subID),
				slog.String("targetAddress", addr),
				slog.Int64("failedForMs", failedFor.Milliseconds()))
		}
		return false, recovered
	case xrayapi.IsUnavailable(err):
		first := false
		m.Ensure(key)
		m.Update(key, func(t *Target) { first = t.SetXrayDown(err.Error(), now) })
		if first {
			log.LogAttrs(ctx, slog.LevelError, "Xray API of the target is unavailable.",
				slog.String("event", "xray_unavailable"),
				slog.String("subscriptionId", subID),
				slog.String("targetAddress", addr),
				slog.String("error", err.Error()))
		}
		return true, false
	}
	return false, false
}
