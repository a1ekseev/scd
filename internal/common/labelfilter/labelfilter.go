// Package labelfilter implements the subscription label filter shared by scd
// and sfc (D29): labelIncludeRegex (all must match) and labelExcludeRegex
// (none may match), each a string or a list of RE2 expressions in the
// regexlit format.
package labelfilter

import (
	"regexp"

	"github.com/a1ekseev/scd/internal/common/jsonout"
	"github.com/a1ekseev/scd/internal/common/regexlit"
	"github.com/a1ekseev/scd/internal/common/schema"
)

// Config keys read by Parse.
const (
	IncludeKey = "labelIncludeRegex"
	ExcludeKey = "labelExcludeRegex"
)

// Filter is a parsed label filter. The *Src slices hold the trimmed config
// strings of the compiled expressions, index-aligned with Include/Exclude.
type Filter struct {
	Include, Exclude       []*regexp.Regexp
	IncludeSrc, ExcludeSrc []string
}

// Parse reads labelIncludeRegex and labelExcludeRegex of n. Both are
// optional; problems (empty values, invalid expressions) are recorded as
// issues of n and the offending elements are left out.
func Parse(n *schema.Node) Filter {
	var f Filter
	f.Include, f.IncludeSrc = compile(n, IncludeKey)
	f.Exclude, f.ExcludeSrc = compile(n, ExcludeKey)
	return f
}

func compile(n *schema.Node, key string) ([]*regexp.Regexp, []string) {
	var res []*regexp.Regexp
	var src []string
	for _, e := range n.StringOrList(key, schema.StrOpt{Trim: true, MinLen: 1}) {
		re, err := regexlit.Compile(e.Value)
		if err != nil {
			n.Addf(e.Key, "%v", err)
			continue
		}
		res = append(res, re)
		src = append(src, e.Value)
	}
	return res, src
}

// Match reports whether label matches every Include expression and none of
// the Exclude expressions (substring search, D8). An empty filter matches
// everything.
func (f Filter) Match(label string) bool {
	for _, re := range f.Include {
		if !re.MatchString(label) {
			return false
		}
	}
	for _, re := range f.Exclude {
		if re.MatchString(label) {
			return false
		}
	}
	return true
}

// Empty reports whether the filter has no expressions.
func (f Filter) Empty() bool {
	return len(f.Include) == 0 && len(f.Exclude) == 0
}

// MarshalJSON renders the filter as in the config (for print-config):
// {"labelIncludeRegex":[...],"labelExcludeRegex":[...]}, empty lists omitted.
func (f Filter) MarshalJSON() ([]byte, error) {
	// jsonout: no HTML escaping, so "<&>" in a pattern survives print-config.
	return jsonout.Marshal(struct {
		Include []string `json:"labelIncludeRegex,omitempty"`
		Exclude []string `json:"labelExcludeRegex,omitempty"`
	}{f.IncludeSrc, f.ExcludeSrc})
}
