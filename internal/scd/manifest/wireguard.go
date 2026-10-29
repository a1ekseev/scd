package manifest

import (
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/a1ekseev/scd/internal/common/candidate"
	"github.com/a1ekseev/scd/internal/common/jsonout"
	"github.com/a1ekseev/scd/internal/common/subscription"
	"github.com/a1ekseev/scd/internal/common/vless"
	"github.com/a1ekseev/scd/internal/scd/config"
	"github.com/a1ekseev/scd/internal/scd/country"
	"github.com/a1ekseev/scd/internal/scd/normalize"
	"github.com/a1ekseev/scd/internal/scd/outbound"
	"github.com/a1ekseev/scd/internal/scd/selection"
)

// Skip and failure codes of a wireguard source.
const (
	// CodeWireGuardInvalid: the line breaks the candidate format.
	CodeWireGuardInvalid = "wireguard_invalid"
	// CodeWireGuardInputRefused: the input file is not a regular file or
	// group and others have access to it.
	CodeWireGuardInputRefused = "wireguard_input_refused"
)

// ProtocolWireGuard is Entry.Protocol of a wireguard candidate.
const ProtocolWireGuard = "wireguard"

// BuildWireGuard builds the manifest of a wireguard candidate file (one
// "wireguard://" line per candidate) with tags of source tagPrefix and the
// endpoint domain strategy. The skipped lines have no raw text (they may
// hold a key); an accepted line keeps candidate.Redacted as Raw. Entries
// keep the line order; the first line of a tag wins.
func BuildWireGuard(text, sourceFile, tagPrefix, domainStrategy string, now time.Time) Manifest {
	lines := subscription.ScanLines(text)
	m := Manifest{
		Version: Version, GeneratedAt: jsonout.Timestamp(now), SourceFile: sourceFile,
		Entries: []Entry{}, Skipped: []vless.Skip{}, Summary: Summary{TotalLines: len(lines)},
	}
	seen := map[string]bool{}
	for _, l := range lines {
		if !strings.HasPrefix(l.Trimmed, "wireguard://") {
			m.skip(vless.Skip{Line: l.Number, Code: vless.CodeUnsupportedScheme, Reason: "The line is not a wireguard:// candidate."})
			continue
		}
		c, err := candidate.ParseWireGuard(l.Trimmed)
		if err != nil {
			m.skip(vless.Skip{
				Line: l.Number, Code: CodeWireGuardInvalid, Reason: "The wireguard candidate line is invalid.",
				Details: []string{err.Error()},
			})
			continue
		}
		e := wireGuardEntry(c, l.Number, tagPrefix, domainStrategy)
		label := e.Label
		if seen[e.Tag] {
			m.skip(vless.Skip{
				Line: l.Number, Raw: e.Normalized.Raw, Label: &label, Code: vless.CodeDuplicateTag,
				Reason: `Duplicate generated outbound tag "` + e.Tag + `".`,
			})
			continue
		}
		if _, err := outbound.ToProto(e.JSONOutbound); err != nil {
			m.skip(vless.Skip{
				Line: l.Number, Raw: e.Normalized.Raw, Label: &label, Code: vless.CodeUnsupportedValue,
				Reason: reasonXrayRejected, Details: []string{err.Error()},
			})
			continue
		}
		seen[e.Tag] = true
		m.Entries = append(m.Entries, e)
		m.Summary.Parsed++
	}
	return m
}

// wireGuardEntry is the entry of candidate c. Its label is the candidate
// label, else the endpoint; its country is the exit metadata, else the
// label flag.
func wireGuardEntry(c candidate.WireGuard, line int, tagPrefix, domainStrategy string) Entry {
	host, port, _ := net.SplitHostPort(c.Endpoint) // valid: ParseWireGuard checked it
	portN, _ := strconv.Atoi(port)
	label := c.Label
	if label == "" {
		label = c.Endpoint
	}
	tag := normalize.BuildWireGuardTag(tagPrefix, c.Label, host, c.PublicHash())
	var info *country.Info
	if i, ok := country.FromISO2(c.Meta.Exit); ok {
		info = &i
	} else if i, ok := country.FromLabel(label); ok {
		info = &i
	}
	city, _ := normalize.InferCity(c.Label)
	n := normalize.Normalized{
		Kind: "outbound", Protocol: ProtocolWireGuard, Profile: "wg", Tag: tag, Line: line, Raw: c.Redacted(),
		Label: label, Address: host, Port: portN, ALPN: []string{}, Country: info, City: city, Query: normalize.Params{},
	}
	return Entry{
		Kind: n.Kind, Protocol: ProtocolWireGuard, Tag: tag, Label: label, Profile: n.Profile, Line: line,
		Country: info, City: city, Normalized: n, JSONOutbound: outbound.BuildWireGuard(c, tag, domainStrategy), Candidate: &c,
		interfaceKey: c.InterfacePublicKey(),
	}
}

// SelectionCandidate is what selection needs of a wireguard entry.
func (e Entry) SelectionCandidate() selection.Candidate {
	c := selection.Candidate{Tag: e.Tag, Label: e.Label}
	if w := e.Candidate; w != nil {
		c.Label = w.Label
		c.Entry, c.Exit, c.Load, c.Score, c.Key, c.Endpoint = w.Meta.Entry, w.Meta.Exit, w.Meta.Load, w.Meta.Score, e.interfaceKey, w.Endpoint
	}
	return c
}

// applyWireGuardFilters is ApplyFilters for a wireguard source
// (selection.Filter): the entry and exit country filters count in
// FilteredByCountry.
func applyWireGuardFilters(m Manifest, f *config.SourceFiltersConfig) Manifest {
	if f == nil {
		return m
	}
	entries := make([]Entry, 0, len(m.Entries))
	var byCountry, byLabel int
	for _, e := range m.Entries {
		switch selection.Filter(e.SelectionCandidate(), f) {
		case selection.Pass:
			entries = append(entries, e)
		case selection.DroppedByLabel:
			byLabel++
		default:
			byCountry++
		}
	}
	m.Entries = entries
	m.Summary.Parsed = len(entries)
	m.Summary.Filtered = byCountry + byLabel
	m.Summary.FilteredByCountry = byCountry
	m.Summary.FilteredByLabelRegex = byLabel
	return m
}
