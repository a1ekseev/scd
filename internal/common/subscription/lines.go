// Package subscription holds helpers for working with subscription text.
package subscription

import "strings"

// Line is one non-empty subscription line.
type Line struct {
	// Number is the 1-based line index before empty lines were filtered out.
	Number int
	// Raw is the line without its trailing "\r".
	Raw string
	// Trimmed is Raw with surrounding white space removed.
	Trimmed string
}

// ScanLines splits text on "\n" only (a trailing "\r" is dropped, a lone "\r"
// or U+0085 is not a separator) and drops lines that are empty after trimming.
func ScanLines(text string) []Line {
	var out []Line
	for i, raw := range strings.Split(text, "\n") {
		raw = strings.TrimSuffix(raw, "\r")
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" {
			continue
		}
		out = append(out, Line{Number: i + 1, Raw: raw, Trimmed: trimmed})
	}
	return out
}
