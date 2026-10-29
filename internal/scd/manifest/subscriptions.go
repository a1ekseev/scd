package manifest

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/a1ekseev/scd/internal/common/candidate"
	"github.com/a1ekseev/scd/internal/common/inputsrc"
	"github.com/a1ekseev/scd/internal/common/securefile"
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
	// Code is CodeWireGuardInputRefused (CodeMasqueInputRefused) for a
	// refused wireguard (masque) input, else "".
	Code string
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
		var (
			l   inputsrc.Loaded
			err error
		)
		if config.CandidateType(s.Type) {
			l, err = LoadPrivate(s.Input, s.MaxInputBytes)
		} else {
			l, err = load(ctx, s.Input, LoadOptions(s))
		}
		if err != nil {
			if ctx.Err() != nil {
				break // cut short by the cancellation: not a failure
			}
			f := FailedSource{Subscription: sub, Config: s, Error: err.Error()}
			if errors.Is(err, securefile.ErrRefused) {
				f.Code = s.Type + "_input_refused"
			}
			failed = append(failed, f)
			continue
		}
		loaded = append(loaded, LoadedSource{Subscription: sub, Config: s, Source: l.Source, Content: l.Content, Encoding: l.Encoding})
	}
	return loaded, failed
}

// LoadPrivate reads the candidate file of a wireguard or masque source with
// securefile.ReadPrivate: an absolute path to a regular file that group and
// others cannot access, at most maxBytes long.
func LoadPrivate(path string, maxBytes int64) (inputsrc.Loaded, error) {
	if !filepath.IsAbs(path) {
		return inputsrc.Loaded{}, fmt.Errorf("input %s must be an absolute file path", path)
	}
	b, err := securefile.ReadPrivate(path, maxBytes)
	if err != nil {
		return inputsrc.Loaded{}, err
	}
	return inputsrc.Loaded{Source: path, Content: string(b), Encoding: "plain"}, nil
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
// the trimmed content under the source's policy (a wireguard or masque
// source: its candidates, BuildCandidates), with the source id as source
// file, filtered and validated. An empty wireguard or masque file is valid:
// its source has no live candidates. The manifest is returned with the
// validation error too.
func FromSource(s LoadedSource, now time.Time) (Manifest, error) {
	id := s.Config.ID
	var m Manifest
	if config.CandidateType(s.Config.Type) {
		ds := ""
		if wg := s.Config.WireGuard; wg != nil {
			ds = wg.EndpointDomainStrategy
		}
		m = BuildCandidates(candidate.Kind(s.Config.Type), strings.TrimSpace(s.Content), id, id, ds, now)
		m = applyCandidateFilters(m, s.Config.Filters)
		if m.Summary.TotalLines == 0 {
			return m, nil // an empty candidate file: no live candidates, not a failure
		}
		return m, Validate(m, id)
	}
	m = Build(strings.TrimSpace(s.Content), id, PolicyFor(s.Config), now)
	m = ApplyFilters(m, s.Config.Filters)
	return m, Validate(m, id)
}
