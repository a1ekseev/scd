// Package normalize turns parsed VLESS links into scd outbound entries; this
// file builds the outbound tag (01 §6.4).
package normalize

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"

	"github.com/a1ekseev/scd/internal/scd/country"
)

const maxSlug = 32

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

func isRegionalIndicator(r rune) bool { return r >= 0x1F1E6 && r <= 0x1F1FF }

// InferCity extracts the city from label: the text before the first comma
// after leading flags and spaces. A part that is empty or contains "extra"
// (any case) yields no city.
func InferCity(label string) (string, bool) {
	s := strings.TrimLeftFunc(label, func(r rune) bool {
		return isRegionalIndicator(r) || unicode.IsSpace(r)
	})
	s = strings.TrimSpace(s)
	if s == "" {
		return "", false
	}
	if i := strings.IndexByte(s, ','); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	if s == "" || strings.Contains(strings.ToLower(s), "extra") {
		return "", false
	}
	return s, true
}

// Slugify makes an ASCII slug: NFKD, combining marks removed, lower case,
// runs of non-alphanumerics become "-", edge dashes trimmed, then cut to 32
// characters (a trailing "-" may remain after the cut).
func Slugify(s string) string {
	d := norm.NFKD.String(s)
	d = strings.Map(func(r rune) rune {
		if r >= 0x300 && r <= 0x36F {
			return -1
		}
		return r
	}, d)
	d = nonSlug.ReplaceAllString(strings.ToLower(d), "-")
	d = strings.Trim(d, "-")
	if len(d) > maxSlug {
		d = d[:maxSlug]
	}
	return d
}

// Sha8 returns the first 8 hex characters of the SHA-256 of s.
func Sha8(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:8]
}

// BuildTag returns "vless-<cc|xx>-<city>-<profile>-<sha8(raw)>". The city part
// is the slug of the label city, else sha8 of it, else the slug of address,
// else "endpoint".
func BuildTag(label, address, profile, raw string) string {
	cc := "xx"
	if info, ok := country.FromLabel(label); ok {
		cc = strings.ToLower(info.ISO2)
	}
	city, ok := InferCity(label)
	slug := ""
	if ok {
		slug = Slugify(city)
		if slug == "" {
			slug = Sha8(city)
		}
	}
	if slug == "" {
		slug = Slugify(address)
	}
	if slug == "" {
		slug = "endpoint"
	}
	return "vless-" + cc + "-" + slug + "-" + profile + "-" + Sha8(raw)
}
