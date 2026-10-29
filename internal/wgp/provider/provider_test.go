package provider

import (
	"strings"
	"testing"

	"github.com/a1ekseev/scd/internal/common/schema"
)

func TestRegister(t *testing.T) {
	t.Cleanup(func() { delete(registry, "regtest") })
	f := Factory{New: func(*schema.Node, Env) Provider { return nil }}
	Register("regtest", f)
	if _, ok := Lookup("regtest"); !ok {
		t.Fatal("not registered")
	}
	for name, c := range map[string]struct {
		typ string
		f   Factory
	}{
		"repeat":  {"regtest", f},
		"no name": {"", f},
		"no New":  {"other", Factory{}},
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if r := recover(); r == nil || !strings.HasPrefix(r.(string), "provider: ") {
					t.Fatalf("recover %v", r)
				}
			}()
			Register(c.typ, c.f)
		})
	}
}
