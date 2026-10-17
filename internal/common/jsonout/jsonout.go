// Package jsonout encodes JSON the way every service prints it: two-space
// indent and no HTML escaping.
package jsonout

import (
	"bytes"
	"encoding/json"
	"io"
)

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
