// Package output publishes the candidate file of a wgp output: one
// "<kind>://" line per candidate, written atomically with mode 0600.
package output

import (
	"bytes"
	"errors"
	"fmt"
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

// Publish writes lines to out.Path, one per line, in the given order: the
// bytes depend only on lines, so a provider that returns its lines in a
// stable order gets a stable file. Every line must parse as a candidate of
// out.Kind. An empty list or a bad line is an error and leaves the file as
// it was. A file that already holds exactly these bytes is not rewritten
// (its mtime is kept) and changed is false. Errors never hold line contents:
// the lines carry private keys.
func Publish(out Output, lines []string) (changed bool, err error) {
	if len(lines) == 0 {
		return false, fmt.Errorf("output %q: no candidates", out.ID)
	}
	return publish(out, lines)
}

// PublishEmpty writes an empty file to out.Path: the provider has no live
// candidate, so scd removes the tunnels of the source. Only the daemon
// calls it, for a tick that returned no lines and no error. changed is as
// for Publish.
func PublishEmpty(out Output) (changed bool, err error) {
	return publish(out, nil)
}

func publish(out Output, lines []string) (changed bool, err error) {
	var buf bytes.Buffer
	for i, l := range lines {
		if err := check(out.Kind, l); err != nil {
			return false, fmt.Errorf("output %q: line %d: %w", out.ID, i+1, err)
		}
		buf.WriteString(l)
		buf.WriteByte('\n')
	}
	data := buf.Bytes()
	// Any read error (no file yet, wrong mode, too long) means: rewrite.
	if old, err := securefile.ReadPrivate(out.Path, int64(len(data))); err == nil && bytes.Equal(old, data) {
		return false, nil
	}
	if err := securefile.WriteAtomic(out.Path, data); err != nil {
		return false, fmt.Errorf("output %q: %w", out.ID, err)
	}
	return true, nil
}

// check parses line with the parser of kind.
func check(kind candidate.Kind, line string) error {
	if strings.ContainsAny(line, "\r\n") {
		return errors.New("line break inside a candidate line")
	}
	switch kind {
	case candidate.KindWireGuard:
		_, err := candidate.ParseWireGuard(line)
		return err
	case candidate.KindMasque:
		_, err := candidate.ParseMasque(line)
		return err
	}
	return fmt.Errorf("unknown candidate kind %q", kind)
}
