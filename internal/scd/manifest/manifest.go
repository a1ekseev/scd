// Package manifest builds the outbound manifest of a subscription (01 §8–§10):
// every line is parsed, normalized and built into an Xray outbound, which
// conf.Build must accept (D19, D26); lines that fail are listed as skipped.
// It also applies the subscription filters and loads subscriptions.
package manifest

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/a1ekseev/scd/internal/common/candidate"
	"github.com/a1ekseev/scd/internal/common/jsonout"
	"github.com/a1ekseev/scd/internal/common/subscription"
	"github.com/a1ekseev/scd/internal/common/vless"
	"github.com/a1ekseev/scd/internal/scd/country"
	"github.com/a1ekseev/scd/internal/scd/normalize"
	"github.com/a1ekseev/scd/internal/scd/outbound"
)

// Version is the manifest format version.
const Version = 1

// reasonXrayRejected is the skip reason of a link whose outbound conf.Build
// rejects; the Xray error is the only detail (D19, D26, 11 §5).
const reasonXrayRejected = "Xray rejected the outbound configuration."

// Entry is one accepted link: its normalized form and JSON outbound.
type Entry struct {
	Kind string `json:"kind"`
	// Protocol is "wireguard" for a wireguard candidate, "" (omitted) for
	// VLESS.
	Protocol     string               `json:"protocol,omitempty"`
	Tag          string               `json:"tag"`
	Label        string               `json:"label"`
	Profile      string               `json:"profile"`
	Line         int                  `json:"line"`
	Country      *country.Info        `json:"country,omitempty"`
	City         string               `json:"city,omitempty"`
	Normalized   normalize.Normalized `json:"normalized"`
	JSONOutbound outbound.Spec        `json:"jsonOutbound"`
	// Warnings are the non-fatal notes of the normalizer.
	Warnings []normalize.Warning `json:"warnings,omitempty"`
	// Candidate is the parsed wireguard candidate (nil for VLESS). It holds
	// the private key and is never printed.
	Candidate *candidate.WireGuard `json:"-"`
	// interfaceKey is the candidate's interface public key (selection key).
	interfaceKey [32]byte
}

// Summary counts the lines of a manifest (01 §1.2 ManifestSummary).
// UnsupportedParam is kept for the JSON shape but is always 0: unknown
// parameters no longer skip a link (D28).
type Summary struct {
	TotalLines           int `json:"totalLines"`
	Parsed               int `json:"parsed"`
	Skipped              int `json:"skipped"`
	Filtered             int `json:"filtered"`
	FilteredByCountry    int `json:"filteredByCountry"`
	FilteredByLabelRegex int `json:"filteredByLabelRegex"`
	UnsupportedScheme    int `json:"unsupportedScheme"`
	UnsupportedParam     int `json:"unsupportedParam"`
	UnsupportedValue     int `json:"unsupportedValue"`
	UnsupportedCombo     int `json:"unsupportedCombo"`
	InvalidURI           int `json:"invalidUri"`
	MissingRequiredField int `json:"missingRequiredField"`
	DuplicateTag         int `json:"duplicateTag"`
	// PortRangeExhausted counts the entries left without a port by the
	// source's portRange (set by the plan, not by Build); also in Skipped.
	PortRangeExhausted int `json:"portRangeExhausted,omitzero"`
	// WireGuardInvalid counts the wireguard_invalid lines; also in Skipped.
	WireGuardInvalid int `json:"wireguardInvalid,omitzero"`
}

// Manifest is the outbound manifest of one subscription.
type Manifest struct {
	Version int `json:"version"`
	// GeneratedAt is UTC with milliseconds, e.g. "2026-10-17T08:32:15.445Z".
	GeneratedAt string `json:"generatedAt"`
	// SourceFile is the subscription id (sync) or the loaded source (generate).
	SourceFile string       `json:"sourceFile"`
	Entries    []Entry      `json:"entries"`
	Skipped    []vless.Skip `json:"skipped"`
	Summary    Summary      `json:"summary"`
}

// ParseLog is the parse report written next to a manifest.
type ParseLog struct {
	GeneratedAt string       `json:"generatedAt"`
	Summary     Summary      `json:"summary"`
	Skipped     []vless.Skip `json:"skipped"`
}

// ParseLog returns the parse report of m.
func (m Manifest) ParseLog() ParseLog {
	return ParseLog{GeneratedAt: m.GeneratedAt, Summary: m.Summary, Skipped: m.Skipped}
}

// skip records s in m.
func (m *Manifest) skip(s vless.Skip) {
	m.Skipped = append(m.Skipped, s)
	m.Summary.Skipped++
	switch s.Code {
	case vless.CodeUnsupportedScheme:
		m.Summary.UnsupportedScheme++
	case vless.CodeUnsupportedValue:
		m.Summary.UnsupportedValue++
	case vless.CodeUnsupportedCombo:
		m.Summary.UnsupportedCombo++
	case vless.CodeInvalidURI:
		m.Summary.InvalidURI++
	case vless.CodeMissingRequiredField:
		m.Summary.MissingRequiredField++
	case vless.CodeDuplicateTag:
		m.Summary.DuplicateTag++
	case CodeWireGuardInvalid:
		m.Summary.WireGuardInvalid++
	}
}

// Build builds the manifest of the subscription text (01 §8). Each line is
// parsed and normalized under p, and its outbound must pass conf.Build;
// the first line of a tag wins, later ones are skipped as duplicate_tag.
// Entries keep the line order.
func Build(text, sourceFile string, p normalize.Policy, now time.Time) Manifest {
	lines := subscription.ScanLines(text)
	m := Manifest{
		Version:     Version,
		GeneratedAt: jsonout.Timestamp(now),
		SourceFile:  sourceFile,
		Entries:     []Entry{},
		Skipped:     []vless.Skip{},
		Summary:     Summary{TotalLines: len(lines)},
	}
	seen := map[string]bool{}
	for _, line := range lines {
		l, s := vless.Parse(line)
		if s != nil {
			m.skip(redactCandidate(*s, line.Trimmed))
			continue
		}
		n, warnings, s := normalize.Normalize(l, p)
		if s != nil {
			m.skip(*s)
			continue
		}
		label := n.Label
		if seen[n.Tag] {
			m.skip(vless.Skip{
				Line: n.Line, Raw: n.Raw, Label: &label, Code: vless.CodeDuplicateTag,
				Reason: `Duplicate generated outbound tag "` + n.Tag + `".`,
			})
			continue
		}
		ob := outbound.Build(n)
		if _, err := outbound.ToProto(ob); err != nil {
			m.skip(vless.Skip{
				Line: n.Line, Raw: n.Raw, Label: &label, Code: vless.CodeUnsupportedValue,
				Reason: reasonXrayRejected, Details: []string{err.Error()},
			})
			continue
		}
		seen[n.Tag] = true
		m.Entries = append(m.Entries, Entry{
			Kind: n.Kind, Tag: n.Tag, Label: n.Label, Profile: n.Profile, Line: n.Line,
			Country: n.Country, City: n.City, Normalized: n, JSONOutbound: ob, Warnings: warnings,
		})
		m.Summary.Parsed++
	}
	return m
}

// redactCandidate drops everything but the scheme from the skip of a
// candidate line (wireguard://, masque://) in a VLESS source: such a line
// holds a private key. Other skips are returned as they are.
func redactCandidate(s vless.Skip, line string) vless.Skip {
	scheme, _, _ := strings.Cut(line, "://")
	for _, k := range []candidate.Kind{candidate.KindWireGuard, candidate.KindMasque} {
		if strings.EqualFold(scheme, string(k)) {
			return vless.Skip{Line: s.Line, Raw: string(k) + "://<redacted>", Code: s.Code, Reason: s.Reason}
		}
	}
	return s
}

// ErrNoValidEntries is matched (errors.Is) by the error of Validate.
var ErrNoValidEntries = errors.New("no valid outbound entries")

type noValidEntriesError struct {
	source string
	s      Summary
}

func (e noValidEntriesError) Error() string {
	s := e.s
	return fmt.Sprintf("Subscription source \"%s\" produced no valid outbound entries. parsed=%d skipped=%d filtered=%d "+
		"filteredByCountry=%d filteredByLabelRegex=%d unsupportedScheme=%d unsupportedParam=%d unsupportedValue=%d "+
		"unsupportedCombo=%d invalidUri=%d missingRequiredField=%d duplicateTag=%d",
		e.source, s.Parsed, s.Skipped, s.Filtered, s.FilteredByCountry, s.FilteredByLabelRegex, s.UnsupportedScheme,
		s.UnsupportedParam, s.UnsupportedValue, s.UnsupportedCombo, s.InvalidURI, s.MissingRequiredField, s.DuplicateTag) +
		wireGuardInvalidSuffix(s)
}

// wireGuardInvalidSuffix is " wireguardInvalid=<n>" when n > 0 (never for
// VLESS, whose text is unchanged).
func wireGuardInvalidSuffix(s Summary) string {
	if s.WireGuardInvalid == 0 {
		return ""
	}
	return fmt.Sprintf(" wireguardInvalid=%d", s.WireGuardInvalid)
}

func (noValidEntriesError) Unwrap() error { return ErrNoValidEntries }

// Validate returns an ErrNoValidEntries error (01 §10 text with every
// counter) when m has no entries.
func Validate(m Manifest, sourceID string) error {
	if m.Summary.Parsed == 0 || len(m.Entries) == 0 {
		return noValidEntriesError{source: sourceID, s: m.Summary}
	}
	return nil
}
