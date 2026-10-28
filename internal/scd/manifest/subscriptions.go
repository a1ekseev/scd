package manifest

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/a1ekseev/scd/internal/common/inputsrc"
	"github.com/a1ekseev/scd/internal/scd/config"
	"github.com/a1ekseev/scd/internal/scd/normalize"
)

// LoadedSource is a source whose input was loaded and decoded.
type LoadedSource struct {
	Subscription config.SubscriptionConfig
	Config       config.SourceConfig
	// Source is what the loader reports: "stdin", the URL or an absolute path.
	Source   string
	Content  string
	Encoding string
}

// FailedSource is a source whose input could not be loaded.
type FailedSource struct {
	Subscription config.SubscriptionConfig
	Config       config.SourceConfig
	// Error is the loader's error text. It never holds fetchHeaders values but
	// it does hold the input URL; redact it with inputsrc.RedactInputIn before
	// logging or printing (D33).
	Error string
}

// LoadSources loads the enabled sources of sub one after another, ordered by
// id (strings.Compare, D23), each with its maxInputBytes limit. A failed
// load does not stop the others; a cancelled ctx does: the remaining
// sources, and a load that failed because of the cancellation, are not
// reported. load is inputsrc.Load in production.
func LoadSources(
	ctx context.Context,
	sub config.SubscriptionConfig,
	load func(ctx context.Context, input string, o inputsrc.Options) (inputsrc.Loaded, error),
) ([]LoadedSource, []FailedSource) {
	enabled := sub.EnabledSources()
	slices.SortStableFunc(enabled, func(a, b config.SourceConfig) int { return strings.Compare(a.ID, b.ID) })
	var (
		loaded []LoadedSource
		failed []FailedSource
	)
	for _, s := range enabled {
		if ctx.Err() != nil {
			break
		}
		l, err := load(ctx, s.Input, LoadOptions(s))
		if err != nil {
			if ctx.Err() != nil {
				break // cut short by the cancellation: not a failure
			}
			failed = append(failed, FailedSource{Subscription: sub, Config: s, Error: err.Error()})
			continue
		}
		loaded = append(loaded, LoadedSource{Subscription: sub, Config: s, Source: l.Source, Content: l.Content, Encoding: l.Encoding})
	}
	return loaded, failed
}

// LoadOptions returns the inputsrc options of source s.
func LoadOptions(s config.SourceConfig) inputsrc.Options {
	return inputsrc.Options{
		Format:       s.Format,
		FetchTimeout: time.Duration(s.FetchTimeoutMs) * time.Millisecond,
		UserAgent:    s.FetchUserAgent,
		Headers:      s.FetchHeaders,
		MaxBytes:     s.MaxInputBytes,
	}
}

// PolicyFor returns the normalization policy of a source (D15): its
// allowInsecure policy and its id as the tag prefix.
func PolicyFor(s config.SourceConfig) normalize.Policy {
	return normalize.Policy{AllowInsecure: s.AllowInsecure == "allow", TagPrefix: s.ID}
}

// FromSource builds the manifest of a loaded source as sync does (01 §10):
// the trimmed content under the source's policy, with the source id as
// source file, filtered and validated. The manifest is returned with the
// validation error too.
func FromSource(s LoadedSource, now time.Time) (Manifest, error) {
	id := s.Config.ID
	m := Build(strings.TrimSpace(s.Content), id, PolicyFor(s.Config), now)
	m = ApplyFilters(m, s.Config.Filters)
	return m, Validate(m, id)
}
