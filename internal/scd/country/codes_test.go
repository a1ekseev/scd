package country

import (
	"slices"
	"testing"
)

func TestSupported(t *testing.T) {
	if len(codes) != 249 || !slices.IsSorted(codes) || len(slices.Compact(slices.Clone(codes))) != len(codes) {
		t.Fatalf("codes: %d, sorted %v", len(codes), slices.IsSorted(codes))
	}
	for _, c := range []string{"AE", "DE", "NO", "ZA", "IS", "NG", "AX", "ZW"} {
		if !Supported(c) {
			t.Errorf("%s not supported", c)
		}
	}
	for _, c := range []string{"ZZ", "UN", "EU", "XK", "AA", "de", "", "USA"} {
		if Supported(c) {
			t.Errorf("%q supported", c)
		}
	}
}
