// Package all links every wgp provider type into the binary. It is the only
// list of provider packages: adding a provider is a new package under
// internal/wgp/provider plus one blank import here.
package all

import (
	_ "github.com/a1ekseev/scd/internal/wgp/provider/proton" // Proton VPN persistent certificates
	_ "github.com/a1ekseev/scd/internal/wgp/provider/static" // .conf files of a directory
	_ "github.com/a1ekseev/scd/internal/wgp/provider/warp"   // Cloudflare WARP devices
)
