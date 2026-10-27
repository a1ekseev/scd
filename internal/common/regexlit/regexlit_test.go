package regexlit

import (
	"strings"
	"testing"
)

func TestCompileLiteral(t *testing.T) {
	tests := []struct {
		in, pattern, match string
	}{
		{"/Германия/i", "(?i)Германия", "🇩🇪 Франкфурт, германия"},
		{"/a/b/i", "(?i)a/b", "xA/By"},
		{`/,\s*Extra\s*$/i`, `(?i),\s*Extra\s*$`, "X, extra"},
		{".*", ".*", "anything / at all"},
		{"/Extra/", "Extra", "a Extra b"},
		{"/^a.b$/s", "(?s)^a.b$", "a\nb"},
		{"/^b/m", "(?m)^b", "a\nb"},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			re, err := Compile(tc.in)
			if err != nil {
				t.Fatal(err)
			}
			if re.String() != tc.pattern {
				t.Fatalf("pattern = %q, want %q", re.String(), tc.pattern)
			}
			if !re.MatchString(tc.match) {
				t.Fatalf("%q does not match %q", re, tc.match)
			}
		})
	}
}

func TestCompileRejects(t *testing.T) {
	tests := []struct{ in, want string }{
		{"/x/g", `unsupported regex flag "g" (only i, m, s are allowed)`},
		{"/x/ii", "unsupported regex flag"},
		{"/x/y", "unsupported regex flag"},
		{"/(/i", "error parsing regexp"},
		{"/broken", `regex literal must end with "/flags"`},
		{"/", `regex literal must end with "/flags"`},
		{"/a(?!b)/", "lookahead, lookbehind and backreferences are not supported (RE2 syntax)"},
		{"/a(?=b)/", "lookahead"},
		{"/(?<=a)b/", "lookahead"},
		{"/(?<!a)b/", "lookahead"},
		{`/(a)\1/`, "lookahead"},
		{`(a)\2`, "lookahead"},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			re, err := Compile(tc.in)
			if err == nil {
				t.Fatalf("expected error, got %v", re)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not contain %q", err, tc.want)
			}
		})
	}
}

func TestMatchIsSubstringSearch(t *testing.T) {
	re, err := Compile("/Extra/")
	if err != nil {
		t.Fatal(err)
	}
	if !re.MatchString("a Extra b") {
		t.Fatal("expected substring match")
	}
}
