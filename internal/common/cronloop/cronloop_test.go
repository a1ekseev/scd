package cronloop

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

// every is a fake schedule firing every d after the previous slot.
type every time.Duration

func (e every) Next(t time.Time) time.Time { return t.Add(time.Duration(e)) }

// never is a fake schedule whose Next is always zero.
type never struct{}

func (never) Next(time.Time) time.Time { return time.Time{} }

func TestRunSkipsOverrunWithoutDrift(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		t0 := time.Now()
		var (
			mu       sync.Mutex
			starts   []time.Duration
			overruns []time.Duration
		)
		release := make(chan struct{})
		var once sync.Once
		task := func(context.Context) error {
			mu.Lock()
			first := len(starts) == 0
			starts = append(starts, time.Since(t0))
			mu.Unlock()
			if first {
				<-release
			}
			return nil
		}
		hooks := Hooks{OnOverrun: func(at time.Time) {
			mu.Lock()
			overruns = append(overruns, at.Sub(t0))
			mu.Unlock()
			once.Do(func() { close(release) })
		}}
		done := make(chan struct{})
		go func() { Run(ctx, every(100*time.Millisecond), task, hooks); close(done) }()
		time.Sleep(250 * time.Millisecond)
		synctest.Wait()
		cancel()
		<-done
		if want := []time.Duration{0, 200 * time.Millisecond}; !slices.Equal(starts, want) {
			t.Errorf("starts = %v, want %v", starts, want)
		}
		if want := []time.Duration{100 * time.Millisecond}; !slices.Equal(overruns, want) {
			t.Errorf("overruns = %v, want %v", overruns, want)
		}
	})
}

func TestRunStopsImmediatelyOnCancel(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		t0 := time.Now()
		runs := 0
		done := make(chan struct{})
		go func() {
			Run(ctx, every(time.Hour), func(context.Context) error { runs++; return nil }, Hooks{})
			close(done)
		}()
		synctest.Wait()
		cancel()
		<-done
		if d := time.Since(t0); d != 0 {
			t.Errorf("clock advanced by %v", d)
		}
		if runs != 1 {
			t.Errorf("runs = %d, want 1", runs)
		}
	})
}

func TestRunWaitsForActiveTask(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		t0 := time.Now()
		var finished time.Duration
		task := func(context.Context) error {
			time.Sleep(5 * time.Second) // ignores ctx on purpose
			finished = time.Since(t0)
			return nil
		}
		done := make(chan struct{})
		go func() { Run(ctx, every(time.Hour), task, Hooks{}); close(done) }()
		time.Sleep(time.Second)
		cancel()
		<-done
		if finished != 5*time.Second {
			t.Errorf("task finished at %v, want 5s", finished)
		}
		if d := time.Since(t0); d != 5*time.Second {
			t.Errorf("Run returned at %v, want 5s", d)
		}
	})
}

func TestRunZeroNextDoesNotSpin(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		runs := 0
		done := make(chan struct{})
		go func() {
			Run(ctx, never{}, func(context.Context) error { runs++; return nil }, Hooks{})
			close(done)
		}()
		time.Sleep(24 * time.Hour)
		synctest.Wait()
		select {
		case <-done:
			t.Fatal("Run returned before cancel")
		default:
		}
		cancel()
		<-done
		if runs != 1 {
			t.Errorf("runs = %d, want 1", runs)
		}
	})
}

func TestCausesFromJoin(t *testing.T) {
	err := errors.Join(errors.New("first cause"), errors.New("second cause"))
	if got, want := Causes(err), []string{"first cause", "second cause"}; !slices.Equal(got, want) {
		t.Errorf("Causes = %q, want %q", got, want)
	}
	if got := Causes(errors.New("plain")); got != nil {
		t.Errorf("Causes(plain) = %q, want nil", got)
	}
}

func decodeLogs(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for line := range strings.Lines(buf.String()) {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("bad log line %q: %v", line, err)
		}
		out = append(out, m)
	}
	return out
}

func byLevel(recs []map[string]any, level string) []map[string]any {
	var out []map[string]any
	for _, r := range recs {
		if r["level"] == level {
			out = append(out, r)
		}
	}
	return out
}

var testEvents = Events{
	OverrunEvent: "balancer_monitor_tick_skipped_overrun",
	OverrunMsg:   "Balancer monitor tick skipped because previous run is still in progress.",
	FailureEvent: "monitor_tick_failed",
	FailureMsg:   "Monitor tick failed.",
}

func TestRunLoggedOverrunLog(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var buf bytes.Buffer
		log := slog.New(slog.NewJSONHandler(&buf, nil))
		ctx, cancel := context.WithCancel(t.Context())
		release := make(chan struct{})
		calls := 0
		task := func(context.Context) error {
			calls++
			if calls == 1 {
				<-release
			}
			return nil
		}
		done := make(chan struct{})
		go func() {
			RunLogged(ctx, every(100*time.Millisecond), task, log, testEvents,
				"subscriptionId", "sub-1", "targetAddress", "xray:8080")
			close(done)
		}()
		time.Sleep(150 * time.Millisecond)
		synctest.Wait()
		close(release)
		synctest.Wait()
		time.Sleep(100 * time.Millisecond) // loop keeps running: slot 200ms starts the task
		synctest.Wait()
		cancel()
		<-done

		recs := decodeLogs(t, &buf)
		if n := len(byLevel(recs, "ERROR")); n != 0 {
			t.Errorf("error logs = %d, want 0", n)
		}
		warns := byLevel(recs, "WARN")
		if len(warns) != 1 {
			t.Fatalf("warn logs = %d, want 1: %s", len(warns), buf.String())
		}
		w := warns[0]
		checks := map[string]any{
			"msg":            testEvents.OverrunMsg,
			"event":          testEvents.OverrunEvent,
			"subscriptionId": "sub-1",
			"targetAddress":  "xray:8080",
			"scheduledAt":    "2000-01-01T00:00:00.100Z",
		}
		for k, v := range checks {
			if w[k] != v {
				t.Errorf("%s = %v, want %v", k, w[k], v)
			}
		}
		if !strings.Contains(buf.String(), `"msg":"`+testEvents.OverrunMsg+`","event":`) {
			t.Errorf("event is not the first attribute: %s", buf.String())
		}
		if calls != 2 {
			t.Errorf("calls = %d, want 2", calls)
		}
	})
}

func TestRunLoggedFailureLogsCauses(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var buf bytes.Buffer
		log := slog.New(slog.NewJSONHandler(&buf, nil))
		ctx, cancel := context.WithCancel(t.Context())
		calls := 0
		task := func(context.Context) error {
			calls++
			if calls == 1 {
				return errors.Join(errors.New("first cause"), errors.New("second cause"))
			}
			return nil
		}
		done := make(chan struct{})
		go func() {
			RunLogged(ctx, every(100*time.Millisecond), task, log, testEvents, "subscriptionId", "sub-1")
			close(done)
		}()
		time.Sleep(150 * time.Millisecond) // second run at 100ms: loop survived the error
		synctest.Wait()
		cancel()
		<-done

		recs := decodeLogs(t, &buf)
		if n := len(byLevel(recs, "WARN")); n != 0 {
			t.Errorf("warn logs = %d, want 0", n)
		}
		errs := byLevel(recs, "ERROR")
		if len(errs) != 1 {
			t.Fatalf("error logs = %d, want 1: %s", len(errs), buf.String())
		}
		e := errs[0]
		if e["msg"] != testEvents.FailureMsg || e["event"] != testEvents.FailureEvent || e["subscriptionId"] != "sub-1" {
			t.Errorf("record = %v", e)
		}
		if e["error"] != "first cause\nsecond cause" {
			t.Errorf("error = %q", e["error"])
		}
		causes, _ := e["causes"].([]any)
		if len(causes) != 2 || causes[0] != "first cause" || causes[1] != "second cause" {
			t.Errorf("causes = %v", e["causes"])
		}
		if calls != 2 {
			t.Errorf("calls = %d, want 2", calls)
		}
	})
}

func TestRunLoggedOmitsEmptyCauses(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var buf bytes.Buffer
		log := slog.New(slog.NewJSONHandler(&buf, nil))
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() {
			RunLogged(ctx, every(time.Hour), func(context.Context) error { return errors.New("boom") }, log, testEvents)
			close(done)
		}()
		synctest.Wait()
		cancel()
		<-done
		errs := byLevel(decodeLogs(t, &buf), "ERROR")
		if len(errs) != 1 || errs[0]["error"] != "boom" {
			t.Fatalf("errors = %v", errs)
		}
		if _, ok := errs[0]["causes"]; ok {
			t.Errorf("causes present: %v", errs[0])
		}
	})
}

// staleOnce is a fake schedule whose first Next lies an hour in the past, as
// after a wall-clock jump or a suspend; later calls step by d.
type staleOnce struct {
	d    time.Duration
	used bool
}

func (s *staleOnce) Next(t time.Time) time.Time {
	if !s.used {
		s.used = true
		return t.Add(-time.Hour)
	}
	return t.Add(s.d)
}

// Final review M1: missed slots are not caught up in a burst; one overrun is
// reported and the loop continues from the next slot after now.
func TestRunDoesNotCatchUpMissedSlots(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		var mu sync.Mutex
		starts, overruns := 0, 0
		task := func(context.Context) error {
			mu.Lock()
			starts++
			mu.Unlock()
			return nil
		}
		hooks := Hooks{OnOverrun: func(time.Time) {
			mu.Lock()
			overruns++
			mu.Unlock()
		}}
		done := make(chan struct{})
		go func() { Run(ctx, &staleOnce{d: 15 * time.Second}, task, hooks); close(done) }()
		time.Sleep(20 * time.Second)
		synctest.Wait()
		cancel()
		<-done
		if starts != 2 || overruns != 1 {
			t.Errorf("starts = %d, overruns = %d; want 2 and 1", starts, overruns)
		}
	})
}
