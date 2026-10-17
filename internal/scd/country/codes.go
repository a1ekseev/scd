// Package country holds the countries scd recognizes in subscription labels
// (by flag emoji) and accepts in filters.countryAllowlist (01 §7).
package country

import "slices"

// codes are the 45 supported ISO 3166-1 alpha-2 codes, sorted.
var codes = []string{
	"AE", "AR", "AT", "AU", "BE", "BG", "BR", "CA", "CH", "CO", "CZ", "DE", "DK", "ES", "FI",
	"FR", "GB", "GR", "HK", "HR", "HU", "IE", "IL", "IN", "IT", "JP", "KR", "KZ", "LT", "MX",
	"MY", "NL", "NO", "PE", "PL", "PT", "RO", "RU", "SE", "SG", "SK", "TR", "UA", "US", "ZA",
}

// Supported reports whether iso2 (upper case) is a supported country code.
func Supported(iso2 string) bool {
	_, found := slices.BinarySearch(codes, iso2)
	return found
}
