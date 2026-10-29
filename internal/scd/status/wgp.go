package status

import (
	"cmp"
	"errors"
	"io/fs"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/a1ekseev/scd/internal/common/wgpstatus"
	"github.com/a1ekseev/scd/internal/scd/config"
	"github.com/a1ekseev/scd/internal/scd/state"
)

// wgpMaxBytes limits the size of a status.json read for the snapshot.
const wgpMaxBytes = 64 << 10

// wgpExpirySoon is how close ExpiresAt must be to degrade a provider card.
// It lies below the renewal window of a provider (renewBeforeMinutes, 6 h
// in the example), so a certificate that is renewed on time never shows as
// degraded; one this close to its expiry was not renewed. scd does not know
// renewBeforeMinutes: with one under 120 a healthy card shows degraded.
const wgpExpirySoon = 2 * time.Hour

// WGPSource is a source that reads the directory of a wgp block.
type WGPSource struct {
	SubscriptionID string `json:"subscriptionId"`
	SourceID       string `json:"sourceId"`
}

// WGPProvider is the common part of one provider in status.json, with
// timestamps in the status API form.
type WGPProvider struct {
	ID            string `json:"id"`
	Output        string `json:"output"`
	Type          string `json:"type"`
	State         string `json:"state"`
	Error         string `json:"error,omitempty"`
	LastTickAt    string `json:"lastTickAt,omitempty"`
	LastSuccessAt string `json:"lastSuccessAt,omitempty"`
	Issued        int    `json:"issued"`
	Limit         int    `json:"limit,omitempty"`
	ExpiresAt     string `json:"expiresAt,omitempty"`
}

// WGPBlock is the status.json of one input directory of the enabled
// wireguard and masque sources. Error is set, without the path, when the
// file exists but cannot be read or parsed; then Providers is empty.
type WGPBlock struct {
	// Sources are the sources reading this directory, in id order.
	Sources   []WGPSource   `json:"sources"`
	UpdatedAt string        `json:"updatedAt,omitempty"`
	Error     string        `json:"error,omitempty"`
	Providers []WGPProvider `json:"providers,omitempty"`
}

// buildWGP reads status.json from the input directory of every enabled
// wireguard or masque source of an enabled subscription (one block per
// directory, sorted by its first source); nil when there is none. A missing
// file gives no block.
func buildWGP(cfg config.AppConfig) []WGPBlock {
	byDir := map[string][]WGPSource{}
	for _, sub := range cfg.Subscriptions {
		if !sub.Enabled {
			continue
		}
		for _, src := range sub.Sources {
			if src.Enabled && config.CandidateType(src.Type) {
				dir := filepath.Dir(src.Input)
				byDir[dir] = append(byDir[dir], WGPSource{SubscriptionID: sub.ID, SourceID: src.ID})
			}
		}
	}
	var blocks []WGPBlock
	for dir, sources := range byDir {
		slices.SortFunc(sources, compareWGPSource)
		b, ok := readWGP(dir)
		if !ok {
			continue
		}
		b.Sources = sources
		blocks = append(blocks, b)
	}
	slices.SortFunc(blocks, func(a, b WGPBlock) int { return compareWGPSource(a.Sources[0], b.Sources[0]) })
	return blocks
}

func compareWGPSource(a, b WGPSource) int {
	return cmp.Or(strings.Compare(a.SubscriptionID, b.SubscriptionID), strings.Compare(a.SourceID, b.SourceID))
}

// readWGP reads dir/status.json; false when it does not exist.
func readWGP(dir string) (WGPBlock, bool) {
	s, err := wgpstatus.Read(dir, wgpMaxBytes)
	if errors.Is(err, fs.ErrNotExist) {
		return WGPBlock{}, false
	}
	if err != nil {
		// The API must not reveal the host path; status.json holds no secrets.
		return WGPBlock{Error: strings.ReplaceAll(err.Error(), filepath.Join(dir, wgpstatus.Name), wgpstatus.Name)}, true
	}
	b := WGPBlock{UpdatedAt: optTime(s.UpdatedAt)}
	for _, p := range s.Providers {
		b.Providers = append(b.Providers, WGPProvider{
			ID: p.ID, Output: p.Output, Type: p.Type, State: p.State, Error: p.Error,
			LastTickAt: optTime(p.LastTickAt), LastSuccessAt: optTime(p.LastSuccessAt),
			Issued: p.Issued, Limit: p.Limit, ExpiresAt: optTime(p.ExpiresAt),
		})
	}
	return b, true
}

// optTime formats t, "" when zero.
func optTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return formatTime(t)
}

// wgpCardView is one card of the wgp section: a provider, or a block whose
// status.json could not be read.
type wgpCardView struct {
	// Class is healthy, or degraded for needs_login, error, an unknown
	// state, an expiry within wgpExpirySoon or a read error.
	Class, Kicker, Title string
	Metrics              []metricView
}

// wgpCards are the wgp cards of snapshot s; ExpiresAt is compared with its
// capture time.
func wgpCards(s *InstanceSnapshot) []wgpCardView {
	captured, _ := time.Parse(time.RFC3339, s.CapturedAt)
	var cards []wgpCardView
	for _, b := range s.WGP {
		if b.Error != "" {
			names := make([]string, 0, len(b.Sources))
			for _, src := range b.Sources {
				names = append(names, src.SubscriptionID+"/"+src.SourceID)
			}
			cards = append(cards, wgpCardView{
				Class: state.TunnelDegraded, Kicker: "wgp", Title: strings.Join(names, ", "),
				Metrics: []metricView{{Label: "Error", Value: b.Error}},
			})
			continue
		}
		for _, p := range b.Providers {
			c := wgpCardView{Class: state.TunnelHealthy, Kicker: "wgp · " + p.Type, Title: p.ID + " · " + p.Output}
			status := metricView{Label: "Status", Value: p.State, Class: " metric-state-healthy"}
			if p.State != wgpstatus.StateOK {
				c.Class, status.Class = state.TunnelDegraded, " metric-state-degraded"
			}
			issued := strconv.Itoa(p.Issued)
			if p.Limit > 0 {
				issued += " of " + strconv.Itoa(p.Limit)
			}
			c.Metrics = append(c.Metrics, status, metricView{Label: "Issued", Value: issued})
			if p.ExpiresAt != "" {
				exp := metricView{Label: "Expires", Value: p.ExpiresAt}
				if at, err := time.Parse(time.RFC3339, p.ExpiresAt); err == nil && !captured.IsZero() && at.Sub(captured) < wgpExpirySoon {
					c.Class, exp.Class = state.TunnelDegraded, " metric-state-degraded"
				}
				c.Metrics = append(c.Metrics, exp)
			}
			c.Metrics = append(c.Metrics, metricView{Label: "Last Success", Value: cmp.Or(p.LastSuccessAt, "-")})
			if p.Error != "" {
				c.Metrics = append(c.Metrics, metricView{Label: "Error", Value: p.Error})
			}
			cards = append(cards, c)
		}
	}
	return cards
}
