package buildinfo

import "testing"

func TestUserAgent(t *testing.T) {
	orig := Version
	t.Cleanup(func() { Version = orig })

	t.Run("default", func(t *testing.T) {
		Version = "dev"
		if got := UserAgent(); got != "scd/dev" {
			t.Fatalf("UserAgent() = %q, want scd/dev", got)
		}
	})
	t.Run("set", func(t *testing.T) {
		Version = "1.2.3"
		if got := UserAgent(); got != "scd/1.2.3" {
			t.Fatalf("UserAgent() = %q, want scd/1.2.3", got)
		}
	})
}
