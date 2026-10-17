package config

import (
	"reflect"
	"slices"
	"testing"
)

func jsonTags(t reflect.Type) []string {
	var out []string
	for f := range t.Fields() {
		out = append(out, f.Tag.Get("json"))
	}
	return out
}

// printedSubscription must mirror Subscription field by field, so
// print-config prints every key in the same order.
func TestPrintedSubscriptionMirrorsSubscription(t *testing.T) {
	got := jsonTags(reflect.TypeFor[printedSubscription]())
	want := jsonTags(reflect.TypeFor[Subscription]())
	if !slices.Equal(got, want) {
		t.Fatalf("printedSubscription tags %v, Subscription tags %v", got, want)
	}
}
