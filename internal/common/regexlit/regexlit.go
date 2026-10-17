// Package regexlit compiles config regular expressions (RE2) given either as
// a bare pattern or as a "/pattern/flags" literal.
package regexlit

import (
	"errors"
	"fmt"
	"regexp"
	"regexp/syntax"
	"strings"
)

const unsupportedHint = "lookahead, lookbehind and backreferences are not supported (RE2 syntax)"

// Compile compiles s. A string starting with "/" must be a literal
// "/pattern/flags" with flags drawn from i, m, s (each at most once);
// anything else is used as an RE2 pattern as is. Matching is a substring
// search (regexp.MatchString).
func Compile(s string) (*regexp.Regexp, error) {
	pattern := s
	if strings.HasPrefix(s, "/") {
		end := strings.LastIndexByte(s, '/')
		if end == 0 {
			return nil, errors.New(`regex literal must end with "/flags"`)
		}
		pattern = s[1:end]
		flags := s[end+1:]
		for i, f := range flags {
			if f != 'i' && f != 'm' && f != 's' {
				return nil, fmt.Errorf("unsupported regex flag %q (only i, m, s are allowed)", string(f))
			}
			if strings.ContainsRune(flags[:i], f) {
				return nil, fmt.Errorf("unsupported regex flag %q (repeated)", string(f))
			}
		}
		if flags != "" {
			pattern = "(?" + flags + ")" + pattern
		}
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		if unsupported(err) {
			return nil, fmt.Errorf("%s: %w", unsupportedHint, err)
		}
		return nil, err
	}
	return re, nil
}

func unsupported(err error) bool {
	var se *syntax.Error
	if !errors.As(err, &se) {
		return false
	}
	switch se.Code {
	case syntax.ErrInvalidPerlOp:
		return true
	case syntax.ErrInvalidEscape:
		return len(se.Expr) == 2 && se.Expr[0] == '\\' && se.Expr[1] >= '1' && se.Expr[1] <= '9'
	}
	return strings.HasPrefix(se.Expr, "(?<=") || strings.HasPrefix(se.Expr, "(?<!")
}
