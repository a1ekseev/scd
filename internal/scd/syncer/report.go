package syncer

import (
	"time"

	"github.com/a1ekseev/scd/internal/common/vless"
	"github.com/a1ekseev/scd/internal/scd/apply"
)

// TargetReport is the sync result of one subscription target (TS
// TargetSyncReport, 03 §2.4). Field order is the JSON key order.
type TargetReport struct {
	SubscriptionID string `json:"subscriptionId"`
	TargetAddress  string `json:"targetAddress"`
	SourceID       string `json:"sourceId"`
	// UnchangedKinds are the kinds skipped because their hash is unchanged.
	UnchangedKinds []apply.Kind `json:"unchangedKinds"`
	// Failed is the sum of Resources[].Failed.
	Failed    int            `json:"failed"`
	Resources []apply.Report `json:"resources"`
	// Skipped are the manifest's skipped lines, once (not once per kind).
	Skipped []vless.Skip `json:"skipped"`
}

// Report is the result of one sync run (TS SyncReport, 03 §2.4), printed by
// `scd sync`. Field order is the JSON key order.
type Report struct {
	// AppliedAt is taken when the report is aggregated (the end of the run).
	AppliedAt  string `json:"appliedAt"`
	DurationMs int64  `json:"durationMs"`
	Added      int    `json:"added"`
	Replaced   int    `json:"replaced"`
	Removed    int    `json:"removed"`
	// Unchanged counts the unchanged kinds over all targets.
	Unchanged int            `json:"unchanged"`
	Failed    int            `json:"failed"`
	Targets   []TargetReport `json:"targets"`
	// Skipped holds every loaded subscription's skipped lines once.
	Skipped []vless.Skip `json:"skipped"`
}

// timestamp formats t like JavaScript's Date.toISOString (03 §1).
func timestamp(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000Z")
}

// emptyReport is the report of an overlapping run (03 §2.4).
func emptyReport(now time.Time) Report {
	return Report{AppliedAt: timestamp(now), Targets: []TargetReport{}, Skipped: []vless.Skip{}}
}

// newTargetReport builds a target report; nil lists become [].
func newTargetReport(subID, address, sourceID string, unchanged []apply.Kind, resources []apply.Report, skipped []vless.Skip) TargetReport {
	if unchanged == nil {
		unchanged = []apply.Kind{}
	}
	if resources == nil {
		resources = []apply.Report{}
	}
	if skipped == nil {
		skipped = []vless.Skip{}
	}
	r := TargetReport{
		SubscriptionID: subID, TargetAddress: address, SourceID: sourceID,
		UnchangedKinds: unchanged, Resources: resources, Skipped: skipped,
	}
	for _, res := range resources {
		r.Failed += res.Failed
	}
	return r
}

// aggregate sums the target reports into the run report.
func aggregate(targets []TargetReport, skipped []vless.Skip, startedAt, now time.Time) Report {
	r := emptyReport(now)
	r.DurationMs = now.Sub(startedAt).Milliseconds()
	if targets != nil {
		r.Targets = targets
	}
	if skipped != nil {
		r.Skipped = skipped
	}
	for _, t := range targets {
		r.Unchanged += len(t.UnchangedKinds)
		for _, res := range t.Resources {
			r.Added += res.Added
			r.Replaced += res.Replaced
			r.Removed += res.Removed
			r.Failed += res.Failed
		}
	}
	return r
}
