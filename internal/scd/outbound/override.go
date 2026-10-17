package outbound

import "github.com/a1ekseev/scd/internal/scd/normalize"

const (
	flowVision       = "xtls-rprx-vision"
	flowVisionUDP443 = "xtls-rprx-vision-udp443"
)

// ForTarget returns a copy of n for one target: with tag, and with the
// xtls-rprx-vision flow of a REALITY link turned into xtls-rprx-vision-udp443
// when the target sets visionUdp443Override (01 §12.1). A TLS vision flow
// is never rewritten.
func ForTarget(n normalize.Normalized, tag string, visionUDP443Override bool) normalize.Normalized {
	n.Tag = tag
	if visionUDP443Override && n.Security == "reality" && n.Flow == flowVision {
		n.Flow = flowVisionUDP443
	}
	return n
}
