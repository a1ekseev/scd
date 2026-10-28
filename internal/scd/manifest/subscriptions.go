package manifest

import (
	"cmp"
	"context"
	"slices"
	"strings"
	"time"

	"github.com/a1ekseev/scd/internal/common/inputsrc"
	"github.com/a1ekseev/scd/internal/scd/config"
	"github.com/a1ekseev/scd/internal/scd/normalize"
)

// LoadedSubscription is a subscription whose input was loaded and decoded.
type LoadedSubscription struct {
	Config config.SubscriptionConfig
	// Source is what the loader reports: "stdin", the URL or an absolute path.
	Source   string
	Content  string
	Encoding string
}

// FailedSubscription is a subscription whose input could not be loaded.
type FailedSubscription struct {
	Config config.SubscriptionConfig
	// Error is the loader's error text. It never holds fetchHeaders values but
	// it does hold the input URL; redact it with inputsrc.RedactInputIn before
	// logging or printing (D33).
	Error string
}

// LoadSubscriptions loads the enabled subscriptions one after another,
// ordered by id and then input (strings.Compare, D23). A failed load does not
// stop the others; a cancelled ctx does: the remaining subscriptions, and a
// load that failed because of the cancellation, are not reported. load is inputsrc.Load in production.
func LoadSubscriptions(
	ctx context.Context,
	subs []config.SubscriptionConfig,
	load func(ctx context.Context, input string, o inputsrc.Options) (inputsrc.Loaded, error),
) ([]LoadedSubscription, []FailedSubscription) {
	var enabled []config.SubscriptionConfig
	for _, s := range subs {
		if s.Enabled {
			enabled = append(enabled, s)
		}
	}
	slices.SortStableFunc(enabled, func(a, b config.SubscriptionConfig) int {
		return cmp.Or(strings.Compare(a.ID, b.ID), strings.Compare(a.Input, b.Input))
	})
	var (
		loaded []LoadedSubscription
		failed []FailedSubscription
	)
	for _, s := range enabled {
		if ctx.Err() != nil {
			break
		}
		l, err := load(ctx, s.Input, inputsrc.Options{
			Format:       s.Format,
			FetchTimeout: time.Duration(s.FetchTimeoutMs) * time.Millisecond,
			UserAgent:    s.FetchUserAgent,
			Headers:      s.FetchHeaders,
		})
		if err != nil {
			if ctx.Err() != nil {
				break // cut short by the cancellation: not a failure
			}
			failed = append(failed, FailedSubscription{Config: s, Error: err.Error()})
			continue
		}
		loaded = append(loaded, LoadedSubscription{Config: s, Source: l.Source, Content: l.Content, Encoding: l.Encoding})
	}
	return loaded, failed
}

// PolicyFor returns the normalization policy of a subscription (D15).
func PolicyFor(c config.SubscriptionConfig) normalize.Policy {
	return normalize.Policy{AllowInsecure: c.AllowInsecure == "allow", TagPrefix: normalize.DefaultTagPrefix}
}

// FromSubscription builds the manifest of a loaded subscription as sync
// does (01 §10): the trimmed content under the subscription's policy, with
// the subscription id as source, filtered and validated. The manifest is
// returned with the validation error too.
func FromSubscription(s LoadedSubscription, now time.Time) (Manifest, error) {
	id := s.Config.ID
	m := Build(strings.TrimSpace(s.Content), id, PolicyFor(s.Config), now)
	m = ApplyFilters(m, s.Config.Filters)
	return m, Validate(m, id)
}
