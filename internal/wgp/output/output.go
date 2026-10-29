// Package output publishes the candidate file of a wgp output: one
// "<kind>://" line per candidate, written atomically with mode 0600.
package output

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/a1ekseev/scd/internal/common/candidate"
	"github.com/a1ekseev/scd/internal/common/securefile"
)

// Output is one candidate file.
type Output struct {
	// ID is the output id from the config.
	ID string
	// Path is the absolute path of the file.
	Path string
	// Kind is the kind of every line, the Kind of the provider.
	Kind candidate.Kind
}

// Change is what a publish changed in the file.
type Change string

// Changes of Publish; "" means the file already held these bytes.
const (
	// ChangeMetadata: the same candidates (PublicHash, in order), other
	// metadata or labels.
	ChangeMetadata Change = "metadata"
	// ChangeCandidates: another candidate list, order, or no readable old
	// file.
	ChangeCandidates Change = "candidates"
)

// maxOld caps the bytes of the old file read for the comparison.
const maxOld = 1 << 20

// Publish writes lines to out.Path, one per line, in the given order: the
// bytes depend only on lines, so a provider that returns its lines in a
// stable order gets a stable file. Every line must parse as a candidate of
// out.Kind. An empty list or a bad line is an error and leaves the file as
// it was. A file that already holds exactly these bytes is not rewritten
// (its mtime is kept) and the change is ""; otherwise the file is written
// and the change says whether the ordered PublicHash list of the old file
// is the same (ChangeMetadata) or not (ChangeCandidates). Errors never hold
// line contents: the lines carry private keys.
func Publish(out Output, lines []string) (Change, error) {
	if len(lines) == 0 {
		return "", fmt.Errorf("output %q: no candidates", out.ID)
	}
	return publish(out, lines)
}

// PublishEmpty writes an empty file to out.Path: the provider has no live
// candidate, so scd removes the tunnels of the source. Only the daemon
// calls it, for a tick that returned no lines and no error. The change is
// as for Publish.
func PublishEmpty(out Output) (Change, error) {
	return publish(out, nil)
}

func publish(out Output, lines []string) (Change, error) {
	var buf bytes.Buffer
	hashes := make([]string, 0, len(lines))
	for i, l := range lines {
		h, err := check(out.Kind, l)
		if err != nil {
			return "", fmt.Errorf("output %q: line %d: %w", out.ID, i+1, err)
		}
		hashes = append(hashes, h)
		buf.WriteString(l)
		buf.WriteByte('\n')
	}
	data := buf.Bytes()
	// Any read error (no file yet, wrong mode, too long) means: rewrite.
	old, err := securefile.ReadPrivate(out.Path, maxOld)
	if err == nil && bytes.Equal(old, data) {
		return "", nil
	}
	change := ChangeCandidates
	if err == nil && slices.Equal(oldHashes(out.Kind, old), hashes) {
		change = ChangeMetadata
	}
	if err := securefile.WriteAtomic(out.Path, data); err != nil {
		return "", fmt.Errorf("output %q: %w", out.ID, err)
	}
	return change, nil
}

// oldHashes is the PublicHash list of the lines of an old file, nil when a
// line does not parse.
func oldHashes(kind candidate.Kind, data []byte) []string {
	hashes := []string{}
	for l := range strings.Lines(string(data)) {
		h, err := check(kind, strings.TrimSuffix(l, "\n"))
		if err != nil {
			return nil
		}
		hashes = append(hashes, h)
	}
	return hashes
}

// check parses line with the parser of kind and returns its PublicHash.
func check(kind candidate.Kind, line string) (string, error) {
	if strings.ContainsAny(line, "\r\n") {
		return "", errors.New("line break inside a candidate line")
	}
	switch kind {
	case candidate.KindWireGuard:
		w, err := candidate.ParseWireGuard(line)
		if err != nil {
			return "", err
		}
		return w.PublicHash(), nil
	case candidate.KindMasque:
		m, err := candidate.ParseMasque(line)
		if err != nil {
			return "", err
		}
		return m.PublicHash(), nil
	}
	return "", fmt.Errorf("unknown candidate kind %q", kind)
}
