package country

import (
	"slices"
	"testing"
)

func TestSupported(t *testing.T) {
	if len(codes) != 45 || !slices.IsSorted(codes) {
		t.Fatalf("codes: %d, sorted %v", len(codes), slices.IsSorted(codes))
	}
	for _, c := range []string{"AE", "DE", "NO", "ZA"} {
		if !Supported(c) {
			t.Errorf("%s not supported", c)
		}
	}
	for _, c := range []string{"ZZ", "de", "", "USA", "XK"} {
		if Supported(c) {
			t.Errorf("%q supported", c)
		}
	}
}
