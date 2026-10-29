// Package daemon runs the wgp providers on their schedules: every tick of a
// provider publishes its output and rewrites status.json with the status of
// all providers.
package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/a1ekseev/scd/internal/common/cron"
	"github.com/a1ekseev/scd/internal/common/cronloop"
	"github.com/a1ekseev/scd/internal/common/wgpstatus"
	"github.com/a1ekseev/scd/internal/wgp/config"
	"github.com/a1ekseev/scd/internal/wgp/output"
	"github.com/a1ekseev/scd/internal/wgp/provider"
)

// tickEvents are the cron loop log events of a provider. A tick logs its
// own failure (provider_tick_failed, edge-triggered), so the loop never
// receives one.
var tickEvents = cronloop.Events{
	OverrunEvent: "provider_tick_skipped_overrun",
	OverrunMsg:   "Provider tick skipped because the previous one is still in progress.",
}

// reserved are the event keys the core sets; a provider event cannot
// override them.
var reserved = []string{"event", "providerId", "providerType", "outputId", "slot"}

// job is one enabled provider with its output and schedule.
type job struct {
	p     config.Provider
	out   output.Output
	sched cron.Schedule
}

// Daemon holds the jobs and the last status of every provider.
type Daemon struct {
	jobs    []job
	dir     string
	records func(id string) provider.Store
	log     *slog.Logger

	mu sync.Mutex
	// status is keyed by provider id; only providers that ticked have one.
	status map[string]wgpstatus.Provider
	// lastWarnings is the joined warnings of the last tick per provider id.
	lastWarnings map[string]string
	// lastWriteErr is the last status.json write error, "" after a success.
	lastWriteErr string
}

// New returns the daemon of the enabled providers of cfg; records returns
// the state record of a provider id.
func New(cfg config.Config, records func(id string) provider.Store, log *slog.Logger) (*Daemon, error) {
	d := &Daemon{records: records, log: log, status: map[string]wgpstatus.Provider{}, lastWarnings: map[string]string{}}
	for _, o := range cfg.Outputs {
		p, ok := cfg.Provider(o.Provider)
		if !ok || !p.Enabled {
			continue
		}
		sched, err := cron.Parse(p.Schedule)
		if err != nil {
			return nil, fmt.Errorf("provider %q: %w", p.ID, err)
		}
		d.dir = filepath.Dir(o.Path)
		d.jobs = append(d.jobs, job{p: p, out: output.Output{ID: o.ID, Path: o.Path, Kind: p.Impl.Kind()}, sched: sched})
	}
	return d, nil
}

// Run runs one cron loop per provider until ctx is done: the first tick at
// once, an overlapping tick skipped. It returns when every loop and its
// active tick have returned.
func (d *Daemon) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for _, j := range d.jobs {
		wg.Go(func() {
			cronloop.RunLogged(ctx, j.sched, func(ctx context.Context) error { d.tick(ctx, j); return nil },
				d.log, tickEvents, j.ids()...)
		})
	}
	wg.Wait()
}

// ids are the correlation attributes of every provider event.
func (j job) ids() []any {
	return []any{"providerId", j.p.ID, "providerType", j.p.Type, "outputId", j.out.ID}
}

// tick runs one provider tick, logs its events, publishes its lines and
// writes status.json. A provider or publish error goes into the provider
// status (state error, or needs_login when the provider says so) and is
// logged here as provider_tick_failed; the previous output file stays.
//
// Warnings, errors and the status write are edge-triggered: a repeat of the
// last state is logged at debug, a change at its own level, and the way
// back to ok as provider_recovered or status_write_recovered.
func (d *Daemon) tick(ctx context.Context, j job) {
	if ctx.Err() != nil {
		return // the first tick of a loop started during shutdown
	}
	start := time.Now().UTC()
	res, err := j.p.Impl.Tick(ctx, d.records(j.p.ID))
	for _, e := range res.Events {
		d.log.LogAttrs(ctx, e.Level, e.Msg, eventAttrs(j, e)...)
	}
	if err != nil && ctx.Err() != nil {
		return // shutdown, not a provider failure
	}
	d.warnings(j, res.Warnings)
	if err == nil {
		var changed bool
		if len(res.Lines) == 0 {
			// No error and no lines: no live candidate, so the file empties
			// and scd drops the tunnels. A failed tick keeps the file.
			changed, err = output.PublishEmpty(j.out)
		} else {
			changed, err = output.Publish(j.out, res.Lines)
		}
		if changed {
			d.log.Info("Output published.", append(j.ids(), "event", "output_published", "candidates", len(res.Lines))...)
		}
	}
	s := res.Status
	s.ID, s.Output, s.Type, s.LastTickAt = j.p.ID, j.out.ID, j.p.Type, start
	switch {
	case err != nil:
		if s.State != wgpstatus.StateNeedsLogin { // needs_login is a failed tick the owner must act on
			s.State = wgpstatus.StateError
		}
		s.Error = err.Error()
	case s.State == "":
		s.State = wgpstatus.StateOK
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	prev, seen := d.status[j.p.ID]
	d.logState(ctx, j, prev, seen, s, err)
	if s.State == wgpstatus.StateOK {
		s.LastSuccessAt = start
	} else {
		s.LastSuccessAt = prev.LastSuccessAt
	}
	d.status[j.p.ID] = s
	d.writeStatus()
}

// eventAttrs are the core ids followed by the attributes of e without the
// reserved keys.
func eventAttrs(j job, e provider.Event) []slog.Attr {
	attrs := []slog.Attr{
		slog.String("event", e.Name),
		slog.String("providerId", j.p.ID),
		slog.String("providerType", j.p.Type),
		slog.String("outputId", j.out.ID),
	}
	if e.Slot > 0 {
		attrs = append(attrs, slog.Int("slot", e.Slot))
	}
	for _, a := range e.Attrs {
		if !slices.Contains(reserved, a.Key) {
			attrs = append(attrs, a)
		}
	}
	return attrs
}

// warnings logs the warnings of a tick: at warn when they differ from the
// last tick, else at debug.
func (d *Daemon) warnings(j job, warnings []string) {
	fp := strings.Join(warnings, "\n")
	d.mu.Lock()
	changed := fp != d.lastWarnings[j.p.ID]
	d.lastWarnings[j.p.ID] = fp
	d.mu.Unlock()
	logf := d.log.Debug
	if changed {
		logf = d.log.Warn
	}
	for _, w := range warnings {
		logf("Provider warning.", append(j.ids(), "event", "provider_warning", "warning", w)...)
	}
}

// logState logs the provider state of a tick against the previous one:
// provider_tick_failed for a failed tick, provider_error for a tick that
// reports an error, each at debug when (state, error) did not change, and
// provider_recovered when the state is ok again. d.mu must be held.
func (d *Daemon) logState(ctx context.Context, j job, prev wgpstatus.Provider, seen bool, s wgpstatus.Provider, err error) {
	same := seen && prev.State == s.State && prev.Error == s.Error
	level := func(l slog.Level) slog.Level {
		if same {
			return slog.LevelDebug
		}
		return l
	}
	switch {
	case err != nil:
		args := append(j.ids(), "event", "provider_tick_failed", "state", s.State, "error", s.Error)
		if causes := cronloop.Causes(err); causes != nil {
			args = append(args, "causes", causes)
		}
		d.log.Log(ctx, level(slog.LevelError), "Provider tick failed.", args...)
	case s.State == wgpstatus.StateError:
		// The provider published what it could and reports the rest.
		d.log.Log(ctx, level(slog.LevelError), "Provider reported an error.", append(j.ids(), "event", "provider_error", "error", s.Error)...)
	case s.State == wgpstatus.StateOK && (prev.State == wgpstatus.StateError || prev.State == wgpstatus.StateNeedsLogin):
		d.log.Info("Provider recovered.", append(j.ids(), "event", "provider_recovered", "previousState", prev.State)...)
	}
}

// writeStatus writes status.json; a failure is logged at error when its
// text is new, else at debug, and the first success after it as
// status_write_recovered. d.mu must be held.
func (d *Daemon) writeStatus() {
	werr := wgpstatus.Write(d.dir, d.snapshot())
	switch {
	case werr != nil:
		logf := d.log.Error
		if werr.Error() == d.lastWriteErr {
			logf = d.log.Debug
		}
		d.lastWriteErr = werr.Error()
		logf("Status file write failed.", "event", "status_write_failed", "error", werr.Error())
	case d.lastWriteErr != "":
		d.lastWriteErr = ""
		d.log.Info("Status file write recovered.", "event", "status_write_recovered")
	}
}

// Status returns the current status.json document.
func (d *Daemon) Status() wgpstatus.Status {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.snapshot()
}

// snapshot builds the status of the providers that ticked, in config order.
// d.mu must be held.
func (d *Daemon) snapshot() wgpstatus.Status {
	s := wgpstatus.Status{Version: wgpstatus.Version, UpdatedAt: time.Now().UTC(), Providers: []wgpstatus.Provider{}}
	for _, j := range d.jobs {
		if p, ok := d.status[j.p.ID]; ok {
			s.Providers = append(s.Providers, p)
		}
	}
	return s
}
