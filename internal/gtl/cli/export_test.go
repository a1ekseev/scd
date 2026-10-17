package cli

import "time"

// NotifyContext exposes notifyContext to the external tests.
var NotifyContext = notifyContext

// SetShutdownTimeout overrides the shutdown timeout and returns a restore func.
func SetShutdownTimeout(d time.Duration) func() {
	old := shutdownTimeout
	shutdownTimeout = d
	return func() { shutdownTimeout = old }
}
