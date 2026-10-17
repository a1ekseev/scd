// Package apply holds the Xray apply algorithms of scd (02 §2, D2): the full
// replacement of the managed outbounds, inbounds and routing rules of a
// target, with fixed resources preserved and a best-effort rollback for
// outbounds only, and the ApplyReport they produce (03 §2.4).
package apply

import (
	"time"

	"github.com/a1ekseev/scd/internal/common/jsonout"
	"github.com/a1ekseev/scd/internal/common/vless"
)

// Kind is a resource kind of an apply.
type Kind string

// Resource kinds.
const (
	KindOutbound Kind = "outbound"
	KindInbound  Kind = "inbound"
	KindRouting  Kind = "routing"
)

// Order is the fixed apply order of the kinds (02 §2.1).
var Order = []Kind{KindOutbound, KindInbound, KindRouting}

// Item statuses.
const (
	StatusRemoved  = "removed"
	StatusAdded    = "added"
	StatusReplaced = "replaced"
	StatusFailed   = "failed"
)

// Item is one step of an apply: a resource id and what happened to it.
type Item struct {
	ID      string `json:"id"`
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
}

// Report is the ApplyReport of one kind on one target. The field order is
// the JSON key order of 03 §2.4.
type Report struct {
	Kind           Kind         `json:"kind"`
	SourceID       string       `json:"sourceId"`
	SubscriptionID string       `json:"subscriptionId"`
	TargetAddress  string       `json:"targetAddress"`
	AppliedAt      string       `json:"appliedAt"`
	DurationMs     int64        `json:"durationMs"`
	Added          int          `json:"added"`
	Replaced       int          `json:"replaced"`
	Removed        int          `json:"removed"`
	Failed         int          `json:"failed"`
	DeletedIDs     []string     `json:"deletedIds"`
	AppliedIDs     []string     `json:"appliedIds"`
	Items          []Item       `json:"items"`
	Skipped        []vless.Skip `json:"skipped"`
}

// Meta identifies an apply in its report. Now is the clock (time.Now when
// nil).
type Meta struct {
	SourceID       string
	SubscriptionID string
	TargetAddress  string
	Now            func() time.Time
}

func (m Meta) now() time.Time {
	if m.Now == nil {
		return time.Now()
	}
	return m.Now()
}

// newReport returns an empty report of kind started at startedAt, with
// non-nil lists so that they serialize as [].
func newReport(kind Kind, m Meta, startedAt time.Time, skipped []vless.Skip) Report {
	if skipped == nil {
		skipped = []vless.Skip{}
	}
	return Report{
		Kind:           kind,
		SourceID:       m.SourceID,
		SubscriptionID: m.SubscriptionID,
		TargetAddress:  m.TargetAddress,
		AppliedAt:      jsonout.Timestamp(startedAt),
		DeletedIDs:     []string{},
		AppliedIDs:     []string{},
		Items:          []Item{},
		Skipped:        skipped,
	}
}

// FailedReport is the report of a kind whose apply returned an error
// (02 §2.1 p.5): one failed item with the kind as id and the error text,
// durationMs 0.
func FailedReport(kind Kind, m Meta, skipped []vless.Skip, err error) Report {
	r := newReport(kind, m, m.now(), skipped)
	r.Failed = 1
	r.Items = append(r.Items, Item{ID: string(kind), Status: StatusFailed, Message: err.Error()})
	return r
}
