package outbound

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	xlog "github.com/xtls/xray-core/common/log"
	"github.com/xtls/xray-core/core"
	"github.com/xtls/xray-core/infra/conf"
)

// discard drops Xray log messages.
type discard struct{}

// Handle implements xlog.Handler.
func (discard) Handle(xlog.Message) {}

// init silences Xray's logger: common/log registers a stdout handler in its
// own init, and conf.Build logs warnings (e.g. the ws deprecation) through it,
// which would corrupt scd's stdout (review B1). scd never runs Xray in
// process, so nothing else uses that logger.
func init() {
	xlog.RegisterHandler(discard{})
}

// ToProto converts o into the protobuf handler config of the Xray API by
// building it with infra/conf, exactly as Xray builds a JSON config (D19).
// A local build error (Xray is not contacted) is returned as "invalid outbound <tag>: <err>".
func ToProto(o Outbound) (*core.OutboundHandlerConfig, error) {
	raw, err := json.Marshal(o)
	if err != nil {
		return nil, fmt.Errorf("invalid outbound %s: %w", o.Tag, err)
	}
	var dc conf.OutboundDetourConfig
	if err := json.Unmarshal(raw, &dc); err != nil {
		return nil, fmt.Errorf("invalid outbound %s: %w", o.Tag, err)
	}
	h, err := dc.Build()
	if err != nil {
		return nil, fmt.Errorf("invalid outbound %s: %w", o.Tag, err)
	}
	return h, nil
}

// Hash returns the hex sha256 of the JSON of list (the outbound manifest
// hash, 09 §5).
func Hash(list []Outbound) string {
	b, err := json.Marshal(list)
	if err != nil {
		// Outbound holds only strings, numbers, slices, maps and RawMessages
		// that normalize validated as JSON objects.
		panic(fmt.Sprintf("outbound: marshal: %v", err))
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
