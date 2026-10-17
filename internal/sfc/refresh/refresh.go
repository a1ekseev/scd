// Package refresh loads sfc subscriptions, filters their VLESS lines into
// outputs and keeps the served output state (04 §4.5–§4.7). The decoded
// source is cached on disk and used when the upstream fails.
package refresh

import (
	"context"
	"encoding/base64"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/a1ekseev/scd/internal/common/inputsrc"
	"github.com/a1ekseev/scd/internal/common/subscription"
	"github.com/a1ekseev/scd/internal/common/vless"
	"github.com/a1ekseev/scd/internal/sfc/cache"
	"github.com/a1ekseev/scd/internal/sfc/config"
)

// errEmptyOutput is the error of an output whose filter matched nothing.
const errEmptyOutput = "Filtered output is empty."

// OutputReport is the refresh result of one enabled output.
type OutputReport struct {
	ID             string `json:"id"`
	SubscriptionID string `json:"subscriptionId"`
	Name           string `json:"name,omitempty"`
	MatchedLines   int    `json:"matchedLines"`
	OK             bool   `json:"ok"`
	// UsedCachedValue reports a failed output that keeps serving its last
	// good payload.
	UsedCachedValue bool `json:"usedCachedValue"`
	// UsedSourceCache reports that the output was built from the disk
	// source cache because the input failed to load.
	UsedSourceCache bool   `json:"usedSourceCache,omitempty"`
	Error           string `json:"error,omitempty"`
}

// Report is the result of one refresh (04 §2.1).
type Report struct {
	RefreshedAt string         `json:"refreshedAt"`
	SourceCount int            `json:"sourceCount"`
	OutputCount int            `json:"outputCount"`
	Successful  int            `json:"successful"`
	Failed      int            `json:"failed"`
	Outputs     []OutputReport `json:"outputs"`
}

// Refresher refreshes the outputs of a config. Run must not be called
// concurrently.
type Refresher struct {
	Cfg   config.Loaded
	State *State
	// Log defaults to discarding.
	Log *slog.Logger
	// Now defaults to time.Now.
	Now func() time.Time
	// Input defaults to inputsrc.Load.
	Input func(ctx context.Context, input string, o inputsrc.Options) (inputsrc.Loaded, error)
}

// Run refreshes the enabled subscriptions one after another and returns the
// report with outputs sorted by id. Failures are reported per output, never
// returned. After ctx is cancelled no further subscription is refreshed and
// the remaining ones are left out of the report (their state is kept).
func (r *Refresher) Run(ctx context.Context) Report {
	reports := []OutputReport{}
	sources := 0
	for _, sub := range r.Cfg.Config.Subscriptions {
		if !sub.Enabled {
			continue
		}
		if ctx.Err() != nil {
			break
		}
		sources++
		reports = append(reports, r.refreshSubscription(ctx, sub)...)
	}
	slices.SortFunc(reports, func(a, b OutputReport) int { return strings.Compare(a.ID, b.ID) })

	rep := Report{
		RefreshedAt: r.now().UTC().Format("2006-01-02T15:04:05.000Z"),
		SourceCount: sources,
		OutputCount: len(reports),
		Outputs:     reports,
	}
	for _, o := range reports {
		if o.OK {
			rep.Successful++
		} else {
			rep.Failed++
		}
	}
	return rep
}

// refreshSubscription loads one subscription, writes the source cache before
// parsing and builds its outputs; when loading fails it falls back to the
// source cache, and without one every enabled output fails.
func (r *Refresher) refreshSubscription(ctx context.Context, sub config.Subscription) []OutputReport {
	log := r.log()
	in, err := r.input()(ctx, sub.Input, inputsrc.Options{
		Format:       sub.Format,
		FetchTimeout: time.Duration(sub.FetchTimeoutMs) * time.Millisecond,
		UserAgent:    sub.FetchUserAgent,
		Headers:      sub.FetchHeaders,
	})
	if err == nil {
		if p, werr := cache.Write(r.Cfg.Path, sub.ID, in.Content); werr != nil {
			log.Warn("Subscription source cache write failed; continuing with fresh input.",
				"event", "subscription_source_cache_write_failed", "subscriptionId", sub.ID, "error", werr.Error())
		} else {
			log.Info("Subscription source cache written.",
				"event", "subscription_source_cache_written", "subscriptionId", sub.ID, "path", p)
		}
		links := r.parse(sub.ID, in.Content)
		reports := r.buildOutputs(sub, links, false)
		log.Info("Subscription refreshed.", "event", "subscription_refreshed", "subscriptionId", sub.ID,
			"source", in.Source, "outputs", enabledOutputIDs(sub), "parsedEntries", len(links))
		return reports
	}

	cachePath, content, cerr := cache.Read(r.Cfg.Path, sub.ID)
	if cerr == nil {
		links := r.parse(sub.ID, content)
		reports := r.buildOutputs(sub, links, true)
		log.Warn("Subscription refresh used source cache after input load failed.",
			"event", "subscription_source_cache_used", "subscriptionId", sub.ID, "path", cachePath,
			"error", err.Error(), "parsedEntries", len(links))
		return reports
	}
	log.Error("Subscription source cache is not available after input load failed.",
		"event", "subscription_source_cache_missed", "subscriptionId", sub.ID, "error", err.Error(),
		"cachePath", cachePath, "cacheError", cerr.Error())
	log.Error("Subscription refresh failed.",
		"event", "subscription_refresh_failed", "subscriptionId", sub.ID, "error", err.Error())

	var reports []OutputReport
	for _, out := range sub.Outputs {
		if out.Enabled {
			reports = append(reports, r.fail(sub, out, err.Error(), false))
		}
	}
	return reports
}

// parse returns the VLESS links of content. Lines that are not usable links
// are dropped silently (as in TS); links with unknown parameters are kept
// (D28) and logged at debug level.
func (r *Refresher) parse(subscriptionID, content string) []vless.Link {
	var links []vless.Link
	for _, line := range subscription.ScanLines(strings.TrimSpace(content)) {
		link, skip := vless.Parse(line)
		if skip != nil {
			continue
		}
		if len(link.Ignored) > 0 {
			r.log().Debug("Subscription line parameters ignored.", "event", "subscription_line_param_ignored",
				"subscriptionId", subscriptionID, "line", link.Line, "params", link.Ignored)
		}
		links = append(links, link)
	}
	return links
}

// buildOutputs filters links into every enabled output of sub.
func (r *Refresher) buildOutputs(sub config.Subscription, links []vless.Link, fromCache bool) []OutputReport {
	var reports []OutputReport
	for _, out := range sub.Outputs {
		if !out.Enabled {
			continue
		}
		var raws []string
		for _, l := range links {
			if out.Filter.Match(l.Label) {
				raws = append(raws, l.Raw)
			}
		}
		if len(raws) == 0 {
			reports = append(reports, r.fail(sub, out, errEmptyOutput, fromCache))
			continue
		}
		reports = append(reports, r.succeed(sub, out, raws, fromCache))
	}
	return reports
}

// succeed stores a new payload for out.
func (r *Refresher) succeed(sub config.Subscription, out config.Output, raws []string, fromCache bool) OutputReport {
	prev := r.State.get(out.ID)
	st := configured(&prev, sub, out)
	now := r.now()
	st.LastGoodPlain = strings.Join(raws, "\n")
	st.LastGoodBase64 = base64.StdEncoding.EncodeToString([]byte(st.LastGoodPlain))
	st.LastGoodLineCount = len(raws)
	st.LastRefreshAt, st.LastSuccessAt = now, now
	st.LastError = ""
	r.State.put(st)
	return OutputReport{
		ID: out.ID, SubscriptionID: sub.ID, Name: out.Name,
		MatchedLines: len(raws), OK: true, UsedSourceCache: fromCache,
	}
}

// fail records a failed refresh of out; the last good payload is kept.
func (r *Refresher) fail(sub config.Subscription, out config.Output, msg string, fromCache bool) OutputReport {
	prev := r.State.get(out.ID)
	st := configured(&prev, sub, out)
	now := r.now()
	st.LastRefreshAt, st.LastFailureAt = now, now
	st.LastError = msg
	r.State.put(st)
	return OutputReport{
		ID: out.ID, SubscriptionID: sub.ID, Name: out.Name,
		UsedCachedValue: prev.LastGoodBase64 != "", UsedSourceCache: fromCache, Error: msg,
	}
}

func enabledOutputIDs(sub config.Subscription) []string {
	ids := []string{}
	for _, out := range sub.Outputs {
		if out.Enabled {
			ids = append(ids, out.ID)
		}
	}
	return ids
}

func (r *Refresher) log() *slog.Logger {
	if r.Log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return r.Log
}

func (r *Refresher) now() time.Time {
	if r.Now == nil {
		return time.Now()
	}
	return r.Now()
}

func (r *Refresher) input() func(context.Context, string, inputsrc.Options) (inputsrc.Loaded, error) {
	if r.Input == nil {
		return inputsrc.Load
	}
	return r.Input
}
