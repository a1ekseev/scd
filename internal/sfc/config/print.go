package config

import "github.com/a1ekseev/scd/internal/common/inputsrc"

// printedSubscription is a subscription as print-config prints it, with
// fetchHeaders as a plain map. Its fields mirror Subscription in the same
// order.
type printedSubscription struct {
	ID             string            `json:"id"`
	Input          string            `json:"input"`
	Enabled        bool              `json:"enabled"`
	Format         string            `json:"format"`
	FetchTimeoutMs int               `json:"fetchTimeoutMs"`
	FetchUserAgent string            `json:"fetchUserAgent,omitempty"`
	FetchHeaders   map[string]string `json:"fetchHeaders,omitempty"`
	PathRoute      string            `json:"pathRoute"`
	Outputs        []Output          `json:"outputs"`
}

// printedConfig is Config with printed subscriptions; the shallower
// Subscriptions field hides the embedded one and keeps its JSON position.
type printedConfig struct {
	Subscriptions []printedSubscription `json:"subscriptions"`
	Config
}

// Printed returns the config as print-config prints it: unless
// includeSecrets, subscription inputs are redacted with
// inputsrc.RedactInput and fetchHeaders values are "<redacted>" (D33,
// final review I3).
func (c Config) Printed(includeSecrets bool) any {
	subs := make([]printedSubscription, 0, len(c.Subscriptions))
	for _, s := range c.Subscriptions {
		p := printedSubscription{
			ID: s.ID, Input: s.Input, Enabled: s.Enabled, Format: s.Format, FetchTimeoutMs: s.FetchTimeoutMs,
			FetchUserAgent: s.FetchUserAgent, FetchHeaders: s.FetchHeaders.Printed(includeSecrets),
			PathRoute: s.PathRoute, Outputs: s.Outputs,
		}
		if !includeSecrets {
			p.Input = inputsrc.RedactInput(s.Input)
		}
		subs = append(subs, p)
	}
	return printedConfig{Subscriptions: subs, Config: c}
}
