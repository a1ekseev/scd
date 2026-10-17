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

// PrintedSubscription must mirror SubscriptionConfig field by field, so
// print-config prints every key in the same order.
func TestPrintedSubscriptionMirrorsSubscriptionConfig(t *testing.T) {
	got := jsonTags(reflect.TypeFor[PrintedSubscription]())
	want := jsonTags(reflect.TypeFor[SubscriptionConfig]())
	if !slices.Equal(got, want) {
		t.Fatalf("PrintedSubscription tags %v, SubscriptionConfig tags %v", got, want)
	}
}

func TestRedactSecretsDoesNotModifyOriginal(t *testing.T) {
	tg := SubscriptionTargetConfig{}
	tg.Monitor.Request = &TargetMonitorRequestConfig{URL: "https://probe.example.com/p/TOKEN"}
	r := tg.RedactSecrets()
	if r.Monitor.Request.URL != "https://probe.example.com/<redacted>" || tg.Monitor.Request.URL != "https://probe.example.com/p/TOKEN" {
		t.Fatalf("redacted %q, original %q", r.Monitor.Request.URL, tg.Monitor.Request.URL)
	}
}
