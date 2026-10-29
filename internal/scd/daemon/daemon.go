// Package daemon runs scd as a long-lived process (03 §3): the sync loop,
// the per-subscription tunnel monitor, balancer monitor and balancer
// selection loops, the remote instance collection loops and the status
// server, until its context is cancelled.
//
// Shutdown is immediate and graceful (D7): on cancellation every cron loop
// stops at once instead of sleeping until its next slot, the ticks in
// progress finish (Xray mutations already run under context.WithoutCancel,
// Review Focus 1), the fire-and-forget remote pushes are awaited (bounded by
// their own timeoutMs), and only then is the status server shut down with
// httpserve.ShutdownTimeout. Ticks are scheduled only by the loops, so once
// they have all returned no new push can be added while waiting for them.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/a1ekseev/scd/internal/common/buildinfo"
	"github.com/a1ekseev/scd/internal/common/cron"
	"github.com/a1ekseev/scd/internal/common/cronloop"
	"github.com/a1ekseev/scd/internal/common/httpserve"
	"github.com/a1ekseev/scd/internal/common/inputsrc"
	"github.com/a1ekseev/scd/internal/common/sigctx"
	"github.com/a1ekseev/scd/internal/scd/balancer"
	"github.com/a1ekseev/scd/internal/scd/config"
	"github.com/a1ekseev/scd/internal/scd/monitor"
	"github.com/a1ekseev/scd/internal/scd/state"
	"github.com/a1ekseev/scd/internal/scd/status"
	"github.com/a1ekseev/scd/internal/scd/syncer"
	"github.com/a1ekseev/scd/internal/scd/xrayapi"
)

// Daemon runs one loaded config. Log is required; a nil Dial means
// xrayapi.Dial, a nil HTTPClient http.DefaultClient (remote instances) and a
// nil Now time.Now.
type Daemon struct {
	Config     config.Loaded
	Log        *slog.Logger
	Dial       xrayapi.Dialer
	HTTPClient *http.Client
	Now        func() time.Time
}

// Log events of the cron loops (03 §8.1; the balancer selection loop is new,
// D22).
var (
	syncEvents = cronloop.Events{
		OverrunEvent: "daemon_tick_skipped_overrun",
		OverrunMsg:   "Daemon tick skipped because previous run is still in progress.",
		FailureEvent: "sync_failed",
		FailureMsg:   "Daemon sync failed.",
	}
	monitorEvents = cronloop.Events{
		OverrunEvent: "monitor_tick_skipped_overrun",
		OverrunMsg:   "Monitor tick skipped because previous run is still in progress.",
		FailureEvent: "monitor_tick_failed",
		FailureMsg:   "Monitor tick failed.",
	}
	balancerMonitorEvents = cronloop.Events{
		OverrunEvent: "balancer_monitor_tick_skipped_overrun",
		OverrunMsg:   "Balancer monitor tick skipped because previous run is still in progress.",
		FailureEvent: "balancer_monitor_tick_failed",
		FailureMsg:   "Balancer monitor tick failed.",
	}
	selectionEvents = cronloop.Events{
		OverrunEvent: "balancer_selection_tick_skipped_overrun",
		OverrunMsg:   "Balancer selection tick skipped because previous run is still in progress.",
		FailureEvent: "balancer_selection_tick_failed",
		FailureMsg:   "Balancer selection tick failed.",
	}
	remoteEvents = cronloop.Events{
		OverrunEvent: "remote_status_tick_skipped_overrun",
		OverrunMsg:   "Remote status tick skipped because previous run is still in progress.",
		FailureEvent: "remote_status_tick_failed",
		FailureMsg:   "Remote status tick failed.",
	}
)

// loop is one cron loop of the daemon.
type loop struct {
	schedule string
	task     func(context.Context) error
	events   cronloop.Events
	// attrs are the context attributes of the loop's log records.
	attrs []any
}

func (d *Daemon) now() time.Time {
	if d.Now == nil {
		return time.Now()
	}
	return d.Now()
}

func (d *Daemon) dialer() xrayapi.Dialer {
	if d.Dial == nil {
		return xrayapi.Dial
	}
	return d.Dial
}

// loops returns the cron loops of the config in start order (03 §3.1): the
// sync loop; for every subscription, enabled or not (as in TS), the tunnel
// monitor, balancer monitor and balancer selection loops that are enabled
// and have a schedule; a loop per remote instance.
func (d *Daemon) loops(mem *state.Memory, agg *status.Aggregator, bg *sync.WaitGroup) []loop {
	cfg := d.Config.Config
	dial := d.dialer()
	s := &syncer.Syncer{Config: d.Config, Memory: mem, Log: d.Log, Dial: dial, Now: d.Now}
	md := monitor.Deps{Memory: mem, Log: d.Log, Dial: dial, Now: d.Now, Background: bg}
	bd := balancer.Deps{Memory: mem, Log: d.Log, Dial: dial, Now: d.Now}

	out := []loop{{
		schedule: cfg.Runtime.Schedule,
		events:   syncEvents,
		task: func(ctx context.Context) error {
			d.Log.LogAttrs(ctx, slog.LevelDebug, "Daemon tick started.", slog.String("event", "daemon_tick"))
			_, err := s.Run(ctx)
			return err
		},
	}}
	for _, sub := range cfg.Subscriptions {
		t := sub.Target
		attrs := []any{"subscriptionId", sub.ID, "targetAddress", t.Address}
		if t.Monitor.Enabled && t.Monitor.Schedule != "" {
			out = append(out, loop{t.Monitor.Schedule, func(ctx context.Context) error {
				return monitor.TunnelTick(ctx, md, sub)
			}, monitorEvents, attrs})
		}
		if t.BalancerMonitor.Enabled && t.BalancerMonitor.Schedule != "" {
			out = append(out, loop{t.BalancerMonitor.Schedule, func(ctx context.Context) error {
				return monitor.BalancerTick(ctx, md, sub)
			}, balancerMonitorEvents, attrs})
		}
		if t.BalancerSelection.Enabled && t.BalancerSelection.Schedule != "" {
			out = append(out, loop{t.BalancerSelection.Schedule, func(ctx context.Context) error {
				return balancer.Tick(ctx, bd, sub)
			}, selectionEvents, attrs})
		}
	}
	for _, r := range cfg.StatusServer.RemoteInstances {
		out = append(out, loop{r.Schedule, func(ctx context.Context) error {
			agg.Collect(ctx, r, d.HTTPClient, d.Log, d.now)
			return nil
		}, remoteEvents, []any{"remoteInstanceId", r.ID, "apiUrl", inputsrc.RedactURL(r.APIURL)}})
	}
	return out
}

// Run runs the daemon until ctx is done and returns nil after a clean stop.
// It fails before starting anything when the config is not in daemon mode,
// a schedule does not parse, or the status server cannot listen; it also
// returns the error of a status server that stops serving (the loops are
// stopped then).
func (d *Daemon) Run(ctx context.Context) error {
	cfg := d.Config.Config
	if cfg.Runtime.Mode != "daemon" {
		return fmt.Errorf("config at %s is not in daemon mode", d.Config.Path)
	}
	mem := state.New()
	agg := status.NewAggregator(cfg.StatusServer.RemoteInstances)
	var background sync.WaitGroup
	loops := d.loops(mem, agg, &background)
	schedules := make([]cron.Schedule, len(loops))
	for i, l := range loops {
		s, err := cron.Parse(l.schedule)
		if err != nil {
			return fmt.Errorf("schedule %q: %w", l.schedule, err)
		}
		schedules[i] = s
	}

	d.Log.LogAttrs(ctx, slog.LevelInfo, "Config loaded.", slog.String("event", "config_loaded"), slog.String("configPath", d.Config.Path),
		slog.String("version", buildinfo.Version))

	var ln net.Listener
	if ss := cfg.StatusServer; ss.Enabled && ss.Listen != "" {
		var err error
		ln, err = httpserve.Listen(ss.Listen) //nolint:contextcheck // binding a socket needs no cancellation
		if err != nil {
			return err
		}
		d.Log.LogAttrs(ctx, slog.LevelInfo, "Status server started.", slog.String("event", "status_server_started"), slog.String("listen", ss.Listen))
	}

	logged := make(chan struct{})
	stopLog := context.AfterFunc(ctx, func() {
		defer close(logged)
		d.Log.LogAttrs(context.WithoutCancel(ctx), slog.LevelInfo, "Daemon stopping.",
			slog.String("event", "daemon_stopping"), slog.String("signal", sigctx.Reason(ctx)))
	})
	defer func() {
		if !stopLog() {
			<-logged
		}
	}()

	loopCtx, stopLoops := context.WithCancel(ctx)
	defer stopLoops()
	// The server outlives ctx: it is shut down after the loops and the
	// remote pushes have finished.
	serverCtx, stopServer := context.WithCancel(context.WithoutCancel(ctx))
	defer stopServer()
	serverErr := make(chan error, 1)
	if ln != nil {
		h := status.NewHandler(status.Deps{Config: d.Config, Memory: mem, Aggregator: agg, Dial: d.dialer(), Now: d.Now, Log: d.Log})
		go func() {
			err := httpserve.Run(serverCtx, ln, h, httpserve.ShutdownTimeout)
			if err != nil {
				stopLoops()
			}
			serverErr <- err
		}()
	} else {
		serverErr <- nil
	}

	var wg sync.WaitGroup
	for i, l := range loops {
		wg.Go(func() {
			cronloop.RunLogged(loopCtx, schedules[i], quietOnCancel(l.task), d.Log, l.events, l.attrs...)
		})
	}
	wg.Wait()
	background.Wait()
	stopServer()
	return <-serverErr
}

// quietOnCancel drops the error of a tick that only reports its own
// cancellation (the monitor and selection ticks return ctx.Err() when
// stopped, recording nothing), so a shutdown does not log tick failures.
func quietOnCancel(task func(context.Context) error) func(context.Context) error {
	return func(ctx context.Context) error {
		err := task(ctx)
		if err != nil && ctx.Err() != nil && onlyCancelled(err) {
			return nil
		}
		return err
	}
}

// onlyCancelled reports whether err is context.Canceled or a multi-error
// (monitor.TaskError) all of whose causes are.
func onlyCancelled(err error) bool {
	var multi interface{ Unwrap() []error }
	if errors.As(err, &multi) {
		errs := multi.Unwrap()
		for _, e := range errs {
			if !onlyCancelled(e) {
				return false
			}
		}
		return len(errs) > 0
	}
	return errors.Is(err, context.Canceled)
}
