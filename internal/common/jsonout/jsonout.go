// Package jsonout encodes JSON the way every service prints it: two-space
// indent, no HTML escaping and JavaScript-style timestamps.
package jsonout

import (
	"bytes"
	"encoding/json"
	"io"
	"time"
)

// TimestampLayout is the JSON timestamp format of every report and API
// (JavaScript's Date.toISOString: UTC with milliseconds).
const TimestampLayout = "2006-01-02T15:04:05.000Z"

// Timestamp formats t in UTC with TimestampLayout.
func Timestamp(t time.Time) string {
	return t.UTC().Format(TimestampLayout)
}

// Write encodes v to w with a two-space indent, without HTML escaping, and
// with a trailing newline.
func Write(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// Marshal is Write into a byte slice without the trailing newline (HTTP bodies).
func Marshal(v any) ([]byte, error) {
	var b bytes.Buffer
	if err := Write(&b, v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n")), nil
}
