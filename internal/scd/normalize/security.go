package normalize

import "github.com/a1ekseev/scd/internal/common/vless"

// applySecurity fills the TLS/REALITY fields of n for n.Security (task 4.4).
func applySecurity(_ vless.Link, _ *Normalized) *vless.Skip {
	return nil
}
