package jsonout

import (
	"bytes"
	"testing"
)

func TestWriteNoHTMLEscape(t *testing.T) {
	var b bytes.Buffer
	if err := Write(&b, map[string]string{"a": "<&>"}); err != nil {
		t.Fatal(err)
	}
	if want := "{\n  \"a\": \"<&>\"\n}\n"; b.String() != want {
		t.Fatalf("got %q, want %q", b.String(), want)
	}
}

func TestMarshalNoTrailingNewline(t *testing.T) {
	got, err := Marshal(map[string]string{"a": "<&>"})
	if err != nil {
		t.Fatal(err)
	}
	if want := "{\n  \"a\": \"<&>\"\n}"; string(got) != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestMarshalError(t *testing.T) {
	if _, err := Marshal(make(chan int)); err == nil {
		t.Fatal("expected error")
	}
}
