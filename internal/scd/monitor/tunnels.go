// Package monitor runs the scd health checks of 03 §4: the per-tunnel
// monitor tick (a probe through every tunnel's Xray SOCKS inbound, with
// retries, a repair after a final failure and a balancer rejoin after a
// success), the repair and rejoin RPC sequences of D2 under the target
// lock, and the balancer monitor tick with its fire-and-forget remote push.
//
// Monitor state is written under the state lock of state.Memory (short
// sections, no I/O); Xray mutations hold the per-target lock and run under
// context.WithoutCancel with the client's per-call timeout, so a shutdown
// never stops a repair or rejoin halfway (Review Focus 1, D7).
package monitor

import (
	"context"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/a1ekseev/scd/internal/scd/config"
	"github.com/a1ekseev/scd/internal/scd/socks"
	"github.com/a1ekseev/scd/internal/scd/state"
	"github.com/a1ekseev/scd/internal/scd/xrayapi"
)

// Deps are the collaborators of the monitor ticks. Memory, Log and Dial are
// required; nil Now, Probe, Direct and Sleep default to time.Now, socks.Do,
// PushDirect and a context-aware timer. Background, when set, tracks the
// remote push goroutines so that the daemon can wait for them on shutdown.
type Deps struct {
	Memory *state.Memory
	Log    *slog.Logger
	Dial   xrayapi.Dialer
	Now    func() time.Time
	// Probe performs one HTTP request through a SOCKS5 proxy.
	Probe func(ctx context.Context, r socks.Request) (socks.Response, error)
	// Direct performs a remote push without SOCKS: GET, the body is not
	// read; it returns the HTTP status code.
	Direct func(ctx context.Context, url string, timeout time.Duration) (int, error)
	// Sleep waits d between probe attempts or returns ctx's error.
	Sleep      func(ctx context.Context, d time.Duration) error
	Background *sync.WaitGroup
}

func (d Deps) now() time.Time {
	if d.Now == nil {
		return time.Now()
	}
	return d.Now()
}

func (d Deps) probe(ctx context.Context, r socks.Request) (socks.Response, error) {
	if d.Probe == nil {
		return socks.Do(ctx, r)
	}
	return d.Probe(ctx, r)
}

func (d Deps) sleep(ctx context.Context, delay time.Duration) error {
	if d.Sleep != nil {
		return d.Sleep(ctx, delay)
	}
	if delay <= 0 {
		return nil
	}
	t := time.NewTimer(delay)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// textError is an error whose text is part of the TS contract (capitalised,
// trailing period): it reaches lastError, the status API and the remote push.
type textError string

func (e textError) Error() string { return string(e) }

// TaskError is returned by TunnelTick when tunnel workers failed. Unwrap
// gives the causes (for errors.Is/As and the causes field of
// monitor_tick_failed).
type TaskError struct {
	Errs []error
}

// Error implements error.
func (e *TaskError) Error() string { return "concurrent task execution failed" }

// Unwrap returns the worker errors.
func (e *TaskError) Unwrap() []error { return e.Errs }

// ResolveProxyHost returns the host to reach a tunnel's SOCKS inbound: the
// inbound listen address, or the host of the target's API address when the
// inbound listens on all interfaces (0.0.0.0 or ::). Unlike the TS
// split(':')[0] it handles IPv6 target addresses ([fd00::5]:8080).
func ResolveProxyHost(listen, targetAddress string) string {
	if listen != "0.0.0.0" && listen != "::" {
		return listen
	}
	host, _, err := net.SplitHostPort(targetAddress)
	if err != nil {
		return targetAddress
	}
	return host
}

// probeSpec is one health check with its retry policy and retry log.
type probeSpec struct {
	host    string
	port    int
	request *config.TargetMonitorRequestConfig
	retry   config.RetryConfig
	event   string
	message string
	attrs   []slog.Attr
}

// probeWithRetry runs the check up to max(1, attempts) times (03 §4.1): an
// error or a status other than expectedStatus is a failed attempt; between
// attempts it logs the retry event at debug and sleeps max(0, delayMs). It
// returns the last error after the final attempt, or ctx's error as soon as
// ctx is done (then nothing must be recorded).
func (d Deps) probeWithRetry(ctx context.Context, s probeSpec) (socks.Response, error) {
	attempts := max(1, s.retry.Attempts)
	delayMs := max(0, s.retry.DelayMs)
	req := socks.Request{
		ProxyHost: s.host,
		ProxyPort: s.port,
		URL:       s.request.URL,
		Method:    s.request.Method,
		Timeout:   time.Duration(s.request.TimeoutMs) * time.Millisecond,
	}
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		resp, err := d.probe(ctx, req)
		if ctx.Err() != nil {
			return socks.Response{}, ctx.Err()
		}
		if err == nil {
			if resp.StatusCode == s.request.ExpectedStatus {
				return resp, nil
			}
			err = textError("Expected HTTP " + strconv.Itoa(s.request.ExpectedStatus) + ", got " + strconv.Itoa(resp.StatusCode) + ".")
		}
		lastErr = err
		if attempt == attempts {
			break
		}
		attrs := append([]slog.Attr{slog.String("event", s.event)}, s.attrs...)
		attrs = append(attrs, slog.Int("attempt", attempt), slog.Int("attempts", attempts), slog.Int("delayMs", delayMs))
		if resp.StatusCode != 0 {
			attrs = append(attrs, slog.Int("statusCode", resp.StatusCode))
		}
		attrs = append(attrs, slog.String("error", err.Error()))
		d.Log.LogAttrs(ctx, slog.LevelDebug, s.message, attrs...)
		if err := d.sleep(ctx, time.Duration(delayMs)*time.Millisecond); err != nil {
			return socks.Response{}, err
		}
	}
	return socks.Response{}, lastErr
}

// updateTunnel applies fn to the monitor and mapping of tunnel id under the
// state lock; a tunnel that is gone (topology replaced) is ignored.
func updateTunnel(m *state.Memory, key, id string, fn func(tn *state.Tunnel)) {
	m.Update(key, func(tg *state.Target) {
		for _, tn := range tg.Tunnels {
			if tn.Mapping.BaseTunnelID == id {
				fn(tn)
				return
			}
		}
	})
}

// tunnelRef is the part of a tunnel the tick needs, copied at its start.
type tunnelRef struct {
	id       string
	sourceID string
	listen   string
	port     int
}

// TunnelTick probes every tunnel of the subscription's target (03 §4.1):
// at most monitor.maxParallel probes at once, retries inside the worker
// slot, a repair after the final failure and a rejoin after a success. It
// is a no-op without a monitor request or before a sync reached the target
// (SyncSeen). A worker error (a repair that could not start) ends that worker;
// the others go on, and the tick returns a *TaskError.
func TunnelTick(ctx context.Context, d Deps, sub config.SubscriptionConfig) error {
	t := sub.Target
	if !t.Monitor.Enabled || t.Monitor.Request == nil {
		return nil
	}
	key := state.Key(sub.ID, t.Address)
	var (
		refs   []tunnelRef
		synced bool
	)
	d.Memory.View(key, func(tg *state.Target) {
		synced = tg.SyncSeen
		for _, tn := range tg.Tunnels {
			refs = append(refs, tunnelRef{tn.Mapping.BaseTunnelID, tn.Mapping.SourceID, tn.Mapping.Listen, tn.Mapping.Port})
		}
	})
	if !synced {
		return nil
	}
	return runLimited(len(refs), t.Monitor.MaxParallel, func(i int) error {
		return checkTunnel(ctx, d, sub, key, refs[i])
	})
}

// checkTunnel is the worker of TunnelTick for one tunnel.
func checkTunnel(ctx context.Context, d Deps, sub config.SubscriptionConfig, key string, ref tunnelRef) error {
	t := sub.Target
	src := sourceOf(sub, ref.sourceID)
	request, retry := sourceProbe(t.Monitor, src)
	resp, err := d.probeWithRetry(ctx, probeSpec{
		host:    ResolveProxyHost(ref.listen, t.Address),
		port:    ref.port,
		request: request,
		retry:   retry,
		event:   "monitor_check_retry",
		message: "Monitor check attempt failed; retrying.",
		attrs: []slog.Attr{
			slog.String("subscriptionId", sub.ID),
			slog.String("targetAddress", t.Address),
			slog.String("tunnelId", ref.id),
		},
	})
	if ctx.Err() != nil {
		// Shutdown: neither a degraded state nor a repair (D7).
		return ctx.Err()
	}
	if err != nil {
		failedAt := d.now()
		inRow, flaps := 0, 0
		updateTunnel(d.Memory, key, ref.id, func(tn *state.Tunnel) {
			m := &tn.Monitor
			m.State = state.TunnelDegraded
			m.LastCheckedAt, m.LastFailureAt = new(failedAt), new(failedAt)
			m.LastStatusCode, m.LastLatencyMs = nil, nil
			m.LastError = err.Error()
			m.ConsecutiveFailures++
			m.FailuresInRow++
			if m.FailingSince == nil {
				m.FailingSince = new(failedAt)
			}
			m.Flap.LastFailAt = failedAt
			if !m.Flap.Since.IsZero() && m.FailuresInRow == downAfter {
				// The flapping tunnel stays down: the episode ends.
				flaps, m.Flap = m.Flap.Count, state.FlapState{}
			}
			inRow = m.FailuresInRow
		})
		if flaps > 0 {
			d.Log.LogAttrs(ctx, slog.LevelWarn, "Flapping tunnel stays down.",
				slog.String("event", "tunnel_down"),
				slog.String("subscriptionId", sub.ID),
				slog.String("sourceId", ref.sourceID),
				slog.String("targetAddress", t.Address),
				slog.String("tunnelId", ref.id),
				slog.Int("failuresInRow", inRow),
				slog.Int("flaps", flaps),
				slog.String("probeError", err.Error()))
		}
		if src != nil && src.Selection != nil && inRow >= src.Selection.ReplaceAfterFailures {
			if done, err := ReplaceFailed(ctx, d, sub, *src, ref.id); done || err != nil {
				return err
			}
		}
		return Repair(ctx, d, sub, ref.id)
	}

	checkedAt := d.now()
	var (
		since       *time.Time
		withdrawn   bool
		mon         state.TunnelMonitor
		stable      bool
		stableFlaps int
		flappedFor  time.Duration
	)
	updateTunnel(d.Memory, key, ref.id, func(tn *state.Tunnel) {
		m := &tn.Monitor
		since, withdrawn = m.FailingSince, shouldRejoin(tn.Mapping)
		m.FailingSince, m.LastRepairError = nil, ""
		m.State = state.TunnelHealthy
		m.LastCheckedAt, m.LastSuccessAt = new(checkedAt), new(checkedAt)
		m.LastStatusCode, m.LastLatencyMs = new(resp.StatusCode), new(resp.LatencyMs)
		m.LastSuccessStatusCode, m.LastSuccessLatencyMs = new(resp.StatusCode), new(resp.LatencyMs)
		m.LastError = ""
		m.ConsecutiveFailures = 0
		m.FailuresInRow = 0
		if f := m.Flap; !f.Since.IsZero() && checkedAt.Sub(f.LastFailAt) >= stableAfter {
			stable, stableFlaps, flappedFor = true, f.Count, f.LastFailAt.Sub(f.Since)
			m.Flap = state.FlapState{}
		}
		mon = *m
	})
	var noLive *time.Time
	d.Memory.Update(key, func(tg *state.Target) {
		if st := tg.Sources[ref.sourceID]; st != nil {
			delete(st.NoReplacement, ref.id)
			noLive, st.NoLiveSince = st.NoLiveSince, nil
		}
	})
	if noLive != nil {
		d.Log.LogAttrs(ctx, slog.LevelInfo, "Source has a working tunnel again.",
			slog.String("event", "source_recovered"),
			slog.String("subscriptionId", sub.ID),
			slog.String("sourceId", ref.sourceID),
			slog.String("targetAddress", t.Address),
			slog.Int64("failedForMs", checkedAt.Sub(*noLive).Milliseconds()))
	}
	if stable {
		d.Log.LogAttrs(ctx, slog.LevelInfo, "Tunnel is stable again after flapping.",
			slog.String("event", "tunnel_stable"),
			slog.String("subscriptionId", sub.ID),
			slog.String("sourceId", ref.sourceID),
			slog.String("targetAddress", t.Address),
			slog.String("tunnelId", ref.id),
			slog.Int("flaps", stableFlaps),
			slog.Int64("flappedForMs", flappedFor.Milliseconds()))
	}
	// One info line per recovery: tunnel_rejoined_balancer (with
	// failedForMs) for a tunnel that goes back into the balancer, else
	// tunnel_recovered; debug inside a flap episode.
	var failedFor *int64
	if since != nil {
		failedFor = new(checkedAt.Sub(*since).Milliseconds())
	}
	if failedFor != nil && !withdrawn {
		d.Log.LogAttrs(ctx, flapLevel(mon, slog.LevelInfo), "Tunnel recovered after a successful monitor probe.",
			slog.String("event", "tunnel_recovered"),
			slog.String("subscriptionId", sub.ID),
			slog.String("sourceId", ref.sourceID),
			slog.String("targetAddress", t.Address),
			slog.String("tunnelId", ref.id),
			slog.Int64("failedForMs", *failedFor))
	}
	if err := rejoin(ctx, d, sub, ref.id, failedFor); err != nil {
		repeat := false
		updateTunnel(d.Memory, key, ref.id, func(tn *state.Tunnel) {
			tn.Monitor.LastError = rejoinFailedPrefix + err.Error()
			repeat = noteRejoinError(&tn.Monitor, err)
		})
		d.logRejoinFailed(ctx, sub, ref.sourceID, ref.id, err, repeat)
	}
	return nil
}

// sourceOf is the source id of sub, nil when it is not configured.
func sourceOf(sub config.SubscriptionConfig, id string) *config.SourceConfig {
	for i := range sub.Sources {
		if sub.Sources[i].ID == id {
			return &sub.Sources[i]
		}
	}
	return nil
}

// sourceProbe is the probe request and retry of the tunnels of src:
// target.monitor with sources[].monitor's timeoutMs, retry.attempts and
// retry.delayMs where they are set.
func sourceProbe(m config.TargetMonitorConfig, src *config.SourceConfig) (*config.TargetMonitorRequestConfig, config.RetryConfig) {
	request, retry := m.Request, m.Retry
	if src == nil || src.Monitor == nil {
		return request, retry
	}
	if o := src.Monitor.Request; o != nil {
		r := *request
		r.TimeoutMs = o.TimeoutMs
		request = &r
	}
	if o := src.Monitor.Retry; o != nil {
		if o.Attempts != 0 {
			retry.Attempts = o.Attempts
		}
		if o.DelayMs != nil {
			retry.DelayMs = *o.DelayMs
		}
	}
	return request, retry
}

// runLimited runs worker for 0..n-1 on max(1, min(maxParallel, n))
// goroutines that share one index (03 §4.1 runWithConcurrencyLimit). A
// failing worker stops its goroutine; the rest drain the queue.
func runLimited(n, maxParallel int, worker func(i int) error) error {
	if n == 0 {
		return nil
	}
	var (
		next atomic.Int64
		mu   sync.Mutex
		errs []error
		wg   sync.WaitGroup
	)
	for range max(1, min(maxParallel, n)) {
		wg.Go(func() {
			for {
				i := int(next.Add(1) - 1)
				if i >= n {
					return
				}
				if err := worker(i); err != nil {
					mu.Lock()
					errs = append(errs, err)
					mu.Unlock()
					return
				}
			}
		})
	}
	wg.Wait()
	if len(errs) > 0 {
		return &TaskError{Errs: errs}
	}
	return nil
}
