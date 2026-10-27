// Package buildinfo holds build-time version information shared by all services.
package buildinfo

// Version is the application version; overridden at startup from the
// ldflags-injected value in cmd/scd.
var Version = "dev"

// UserAgent returns the default User-Agent used when fetching subscriptions.
func UserAgent() string {
	return "scd/" + Version
}
