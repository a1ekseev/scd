package subscription

import (
	"reflect"
	"testing"
)

func TestScanLinesNumbersAndCRLF(t *testing.T) {
	got := ScanLines("a\r\n\r\n b \n")
	want := []Line{{1, "a", "a"}, {3, " b ", "b"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ScanLines = %#v, want %#v", got, want)
	}
}

func TestScanLinesTrailingNewline(t *testing.T) {
	got := ScanLines("vless://x\n")
	if len(got) != 1 || got[0].Number != 1 || got[0].Trimmed != "vless://x" {
		t.Fatalf("ScanLines = %#v", got)
	}
}

func TestScanLinesLoneCRNotSeparator(t *testing.T) {
	got := ScanLines("a\rb\nc")
	want := []Line{{1, "a\rb", "a\rb"}, {2, "c", "c"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ScanLines = %#v, want %#v", got, want)
	}
}

func TestScanLinesOnlySplitsOnLF(t *testing.T) {
	// U+0085 (NEL) and a BOM inside a line must not split it.
	got := ScanLines("a\u0085b\n\xEF\xBB\xBFc\td\n")
	if len(got) != 2 || got[0].Raw != "a\u0085b" {
		t.Fatalf("ScanLines = %#v", got)
	}
}
