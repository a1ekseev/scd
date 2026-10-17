package monitor

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/a1ekseev/scd/internal/scd/config"
	"github.com/a1ekseev/scd/internal/scd/socks"
	"github.com/a1ekseev/scd/internal/scd/state"
)

// pingPayload is what a remote push reports; pingMs is nil for down.
type pingPayload struct {
	status, msg string
	pingMs      *int
}

// scheduleRemotePing starts the remote push of a balancer check (03 §4.6)
// unless remotePing is off or a previous push is still pending (then it
// logs balancer_monitor_remote_ping_skipped_overrun). The pending
// transition is a check-and-set under the state lock. The push runs in a
// goroutine counted in d.Background and is not awaited by the tick; it is
// detached from ctx cancellation (each request has remotePing.timeoutMs), so
// a shutdown waits for it at most that long instead of recording a failure.
func (d Deps) scheduleRemotePing(ctx context.Context, sub config.SubscriptionConfig, key string, p pingPayload) {
	bm := sub.Target.BalancerMonitor
	if !bm.RemotePing.Enabled || bm.RemotePing.URL == "" || bm.Socks5 == nil {
		return
	}
	startedAt := d.now()
	skipped := false
	if !d.Memory.Update(key, func(tg *state.Target) {
		b := &tg.Balancer
		if b.RemotePingState == state.RemotePingPending {
			skipped = true
			return
		}
		b.RemotePingState = state.RemotePingPending
		b.RemotePingLastRunAt = &startedAt
		b.RemotePingLastError = ""
		b.RemotePingLastReportedStatus = p.status
		b.RemotePingLastReportedMsg = p.msg
		b.RemotePingLastReportedPingMs = nil
		if p.pingMs != nil {
			b.RemotePingLastReportedPingMs = new(*p.pingMs)
		}
	}) {
		return
	}
	if skipped {
		d.Log.LogAttrs(ctx, slog.LevelWarn, "Balancer monitor remote ping skipped because previous push is still in progress.",
			slog.String("event", "balancer_monitor_remote_ping_skipped_overrun"),
			slog.String("subscriptionId", sub.ID),
			slog.String("targetAddress", sub.Target.Address))
		return
	}

	d.logPing(ctx, slog.LevelInfo, "Balancer monitor remote ping started.", "balancer_monitor_remote_ping_started",
		sub, p, slog.String("status", p.status))

	pctx := context.WithoutCancel(ctx)
	run := func() { d.pushRemotePing(pctx, sub, key, p) }
	if d.Background != nil {
		d.Background.Go(run)
	} else {
		go run()
	}
}

// pingAttrs are the trailing attributes of every remote ping log: never the
// push URL (its query usually holds a token), only its host.
func pingAttrs(bm config.TargetBalancerMonitorConfig, p pingPayload) []slog.Attr {
	attrs := []slog.Attr{
		slog.String("reportedStatus", p.status),
		slog.Bool("viaSocks", bm.RemotePing.ViaSocks),
		slog.String("remoteHost", remoteHost(bm.RemotePing.URL)),
	}
	if bm.RemotePing.ViaSocks {
		attrs = append(attrs, slog.String("proxyHost", bm.Socks5.Host), slog.Int("proxyPort", bm.Socks5.Port))
	}
	return attrs
}

// pushRemotePing sends one push and records its outcome: a 2xx status is
// ok, any other status or an error is failed. The balancer state is never
// changed by a push.
func (d Deps) pushRemotePing(ctx context.Context, sub config.SubscriptionConfig, key string, p pingPayload) {
	bm := sub.Target.BalancerMonitor
	code, pushURL, err := d.sendRemotePing(ctx, bm, p)
	if err != nil {
		msg := pingErrorText(err, bm.RemotePing.URL, pushURL)
		failedAt := d.now()
		d.Memory.Update(key, func(tg *state.Target) {
			b := &tg.Balancer
			b.RemotePingState = state.RemotePingFailed
			b.RemotePingLastFailureAt = &failedAt
			b.RemotePingLastStatusCode = nil
			b.RemotePingLastError = msg
		})
		d.logPing(ctx, slog.LevelWarn, "Balancer monitor remote ping failed.", "balancer_monitor_remote_ping_failed",
			sub, p, slog.String("error", msg))
		return
	}
	if code < 200 || code >= 300 {
		msg := "Remote ping returned HTTP " + strconv.Itoa(code) + "."
		failedAt := d.now()
		d.Memory.Update(key, func(tg *state.Target) {
			b := &tg.Balancer
			b.RemotePingState = state.RemotePingFailed
			b.RemotePingLastFailureAt = &failedAt
			b.RemotePingLastStatusCode = new(code)
			b.RemotePingLastError = msg
		})
		d.logPing(ctx, slog.LevelWarn, "Balancer monitor remote ping failed.", "balancer_monitor_remote_ping_failed",
			sub, p, slog.Int("statusCode", code), slog.String("error", msg))
		return
	}

	doneAt := d.now()
	d.Memory.Update(key, func(tg *state.Target) {
		b := &tg.Balancer
		b.RemotePingState = state.RemotePingOK
		b.RemotePingLastSuccessAt = &doneAt
		b.RemotePingLastStatusCode = new(code)
		b.RemotePingLastError = ""
	})
	d.logPing(ctx, slog.LevelInfo, "Balancer monitor remote ping succeeded.", "balancer_monitor_remote_ping_succeeded",
		sub, p, slog.Int("statusCode", code))
}

// logPing logs a remote ping event: event, subscriptionId, targetAddress,
// the event's own attributes, then pingAttrs.
func (d Deps) logPing(ctx context.Context, level slog.Level, msg, event string, sub config.SubscriptionConfig, p pingPayload, own ...slog.Attr) {
	attrs := []slog.Attr{
		slog.String("event", event),
		slog.String("subscriptionId", sub.ID),
		slog.String("targetAddress", sub.Target.Address),
	}
	attrs = append(attrs, own...)
	attrs = append(attrs, pingAttrs(sub.Target.BalancerMonitor, p)...)
	d.Log.LogAttrs(ctx, level, msg, attrs...)
}

// sendRemotePing builds the push URL and sends it through the balancer
// SOCKS5 (viaSocks) or directly. It returns the status code and the URL.
func (d Deps) sendRemotePing(ctx context.Context, bm config.TargetBalancerMonitorConfig, p pingPayload) (int, string, error) {
	pushURL, err := RemotePingURL(bm.RemotePing.URL, p.status, p.msg, p.pingMs)
	if err != nil {
		return 0, "", err
	}
	timeout := time.Duration(bm.RemotePing.TimeoutMs) * time.Millisecond
	if bm.RemotePing.ViaSocks {
		resp, err := d.probe(ctx, socks.Request{
			ProxyHost: bm.Socks5.Host,
			ProxyPort: bm.Socks5.Port,
			URL:       pushURL,
			Method:    http.MethodGet,
			Timeout:   timeout,
		})
		return resp.StatusCode, pushURL, err
	}
	direct := d.Direct
	if direct == nil {
		direct = PushDirect
	}
	code, err := direct(ctx, pushURL, timeout)
	return code, pushURL, err
}

// pingErrorText is the error text of a failed push without the push URL:
// a *url.Error is reduced to its cause and any remaining copy of the URLs
// is replaced by "<redacted>".
func pingErrorText(err error, urls ...string) string {
	if ue, ok := errors.AsType[*url.Error](err); ok {
		err = ue.Err
	}
	msg := err.Error()
	for _, u := range urls {
		if u != "" {
			msg = strings.ReplaceAll(msg, u, "<redacted>")
		}
	}
	return msg
}

// directClient sends direct pushes; it follows redirects like fetch.
var directClient = &http.Client{}

// PushDirect sends a remote push without SOCKS (TS pushRemotePingDirect):
// GET rawURL within timeout, the status code is returned once the headers
// arrive and the body is closed without being read. A timeout fails with
// "Remote ping timed out after <ms>ms."; other errors carry no URL.
func PushDirect(ctx context.Context, rawURL string, timeout time.Duration) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return 0, errors.New(pingErrorText(err, rawURL))
	}
	resp, err := directClient.Do(req)
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return 0, textError("Remote ping timed out after " + strconv.FormatInt(timeout.Milliseconds(), 10) + "ms.")
		}
		return 0, errors.New(pingErrorText(err, rawURL))
	}
	_ = resp.Body.Close()
	return resp.StatusCode, nil
}
