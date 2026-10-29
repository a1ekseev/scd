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

// Skip and failure codes of a wireguard and a masque source.
const (
	// CodeWireGuardInvalid: the line breaks the candidate format.
	CodeWireGuardInvalid = "wireguard_invalid"
	// CodeWireGuardInputRefused: the input file is not a regular file or
	// group and others have access to it.
	CodeWireGuardInputRefused = "wireguard_input_refused"
	// CodeMasqueInvalid is CodeWireGuardInvalid of a masque source.
	CodeMasqueInvalid = "masque_invalid"
	// CodeMasqueInputRefused is CodeWireGuardInputRefused of a masque source.
	CodeMasqueInputRefused = "masque_input_refused"
)

// Entry.Protocol of a candidate.
const (
	ProtocolWireGuard = "wireguard"
	ProtocolMasque    = "masque"
)

// BuildWireGuard is BuildCandidates of a wireguard candidate file.
func BuildWireGuard(text, sourceFile, tagPrefix, domainStrategy string, now time.Time) Manifest {
	return BuildCandidates(candidate.KindWireGuard, text, sourceFile, tagPrefix, domainStrategy, now)
}

// BuildCandidates builds the manifest of a candidate file of kind (one
// "<kind>://" line per candidate) with tags of source tagPrefix;
// domainStrategy is the endpoint domain strategy of wireguard. The skipped
// lines have no raw text (they may hold a key); an accepted line keeps the
// redacted candidate line as Raw. Entries keep the line order; the first
// line of a tag wins.
func BuildCandidates(kind candidate.Kind, text, sourceFile, tagPrefix, domainStrategy string, now time.Time) Manifest {
	lines := subscription.ScanLines(text)
	m := Manifest{
		Version: Version, GeneratedAt: jsonout.Timestamp(now), SourceFile: sourceFile,
		Entries: []Entry{}, Skipped: []vless.Skip{}, Summary: Summary{TotalLines: len(lines)},
	}
	seen := map[string]bool{}
	for _, l := range lines {
		if !strings.HasPrefix(l.Trimmed, string(kind)+"://") {
			m.skip(vless.Skip{Line: l.Number, Code: vless.CodeUnsupportedScheme, Reason: "The line is not a " + string(kind) + ":// candidate."})
			continue
		}
		e, err := candidateEntry(kind, l.Trimmed, l.Number, tagPrefix, domainStrategy)
		if err != nil {
			m.skip(vless.Skip{
				Line: l.Number, Code: string(kind) + "_invalid", Reason: "The " + string(kind) + " candidate line is invalid.",
				Details: []string{err.Error()},
			})
			continue
		}
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

// candidateEntry parses line as a candidate of kind and returns its entry;
// the error is the parser's *candidate.FieldError.
func candidateEntry(kind candidate.Kind, line string, number int, tagPrefix, domainStrategy string) (Entry, error) {
	if kind == candidate.KindMasque {
		c, err := candidate.ParseMasque(line)
		if err != nil {
			return Entry{}, err
		}
		return masqueEntry(c, number, tagPrefix), nil
	}
	c, err := candidate.ParseWireGuard(line)
	if err != nil {
		return Entry{}, err
	}
	return wireGuardEntry(c, number, tagPrefix, domainStrategy), nil
}

// newCandidateEntry is the entry of a candidate with label (the endpoint
// when empty), exit metadata, endpoint host and port, tag profile and public
// hash: its country is the exit metadata, else the label flag.
func newCandidateEntry(protocol, profile, label, exit, endpoint, host string, port, line int, tagPrefix, hash, raw string) Entry {
	shown := label
	if shown == "" {
		shown = endpoint
	}
	tag := normalize.BuildCandidateTag(tagPrefix, label, host, profile, hash)
	var info *country.Info
	if i, ok := country.FromISO2(exit); ok {
		info = &i
	} else if i, ok := country.FromLabel(shown); ok {
		info = &i
	}
	city, _ := normalize.InferCity(label)
	n := normalize.Normalized{
		Kind: "outbound", Protocol: protocol, Profile: profile, Tag: tag, Line: line, Raw: raw,
		Label: shown, Address: host, Port: port, ALPN: []string{}, Country: info, City: city, Query: normalize.Params{},
	}
	return Entry{
		Kind: n.Kind, Protocol: protocol, Tag: tag, Label: shown, Profile: profile, Line: line,
		Country: info, City: city, Normalized: n,
	}
}

// wireGuardEntry is the entry of wireguard candidate c.
func wireGuardEntry(c candidate.WireGuard, line int, tagPrefix, domainStrategy string) Entry {
	host, port, _ := net.SplitHostPort(c.Endpoint) // valid: ParseWireGuard checked it
	portN, _ := strconv.Atoi(port)
	e := newCandidateEntry(ProtocolWireGuard, "wg", c.Label, c.Meta.Exit, c.Endpoint, host, portN, line, tagPrefix, c.PublicHash(), c.Redacted())
	e.JSONOutbound, e.Candidate = outbound.BuildWireGuard(c, e.Tag, domainStrategy), &c
	e.selection = &selection.Candidate{
		Tag: e.Tag, Label: c.Label, Entry: c.Meta.Entry, Exit: c.Meta.Exit, Load: c.Meta.Load, Score: c.Meta.Score,
		Key: c.InterfacePublicKey(), Endpoint: c.Endpoint,
	}
	return e
}

// SelectionCandidate is what selection needs of a candidate entry (the tag
// and label only for VLESS).
func (e Entry) SelectionCandidate() selection.Candidate {
	if e.selection != nil {
		return *e.selection
	}
	return selection.Candidate{Tag: e.Tag, Label: e.Label}
}

// applyCandidateFilters is ApplyFilters for a wireguard or masque source
// (selection.Filter): the entry and exit country filters count in
// FilteredByCountry.
func applyCandidateFilters(m Manifest, f *config.SourceFiltersConfig) Manifest {
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
