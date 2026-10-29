// Package config loads and validates the wgp config file. The core keys are
// parsed here; the subtree of each provider and its identification.<type>
// are handed to the Factory of the provider type, found in the registry of
// internal/wgp/provider. There are no secrets in the file.
package config

import (
	"fmt"
	"path/filepath"
	"regexp"
	"slices"

	"github.com/a1ekseev/scd/internal/common/configload"
	"github.com/a1ekseev/scd/internal/common/cron"
	"github.com/a1ekseev/scd/internal/common/jsonout"
	"github.com/a1ekseev/scd/internal/common/listenaddr"
	"github.com/a1ekseev/scd/internal/common/schema"
	"github.com/a1ekseev/scd/internal/common/wgpstatus"
	"github.com/a1ekseev/scd/internal/wgp/provider"
)

var (
	// idPattern is the form of output and provider ids: an output id is
	// part of DeviceName <prefix><output id>-<gen>-<n>, a provider id names
	// its state file.
	idPattern = regexp.MustCompile(`^[a-z0-9]+$`)
	// prefixPattern is the form of deviceNamePrefix.
	prefixPattern = regexp.MustCompile(`^[a-z0-9-]+$`)
)

// Config is the normalized wgp configuration. Field order is the
// print-config JSON order.
type Config struct {
	Server struct {
		Listen string `json:"listen"`
	} `json:"server"`
	Logging struct {
		Level  string `json:"level"`
		Format string `json:"format"`
	} `json:"logging"`
	State struct {
		Dir string `json:"dir"`
		// KeyFile is the path of the state key; the key itself is never
		// read by Load or printed.
		KeyFile string `json:"keyFile"`
	} `json:"state"`
	// Identification is keyed by provider type; values come from the
	// Factory of the type.
	Identification   map[string]any `json:"identification,omitempty"`
	DeviceNamePrefix string         `json:"deviceNamePrefix"`
	Keys             struct {
		// RevokeOldAfterMinutes is 0 when unset.
		RevokeOldAfterMinutes int `json:"revokeOldAfterMinutes,omitzero"`
	} `json:"keys,omitzero"`
	API       provider.API `json:"api"`
	Providers []Provider   `json:"providers"`
	Outputs   []Output     `json:"outputs"`
}

// Provider is one configured provider. It is printed by MarshalJSON: the
// common keys, then the JSON object of Impl.
type Provider struct {
	ID       string
	Type     string
	Enabled  bool
	Schedule string
	// Impl is built by the Factory of Type.
	Impl provider.Provider
}

// MarshalJSON prints id, type, enabled, schedule and then the keys of Impl.
func (p Provider) MarshalJSON() ([]byte, error) {
	head, err := jsonout.Marshal(struct {
		ID       string `json:"id"`
		Type     string `json:"type"`
		Enabled  bool   `json:"enabled"`
		Schedule string `json:"schedule"`
	}{p.ID, p.Type, p.Enabled, p.Schedule})
	if err != nil || p.Impl == nil {
		return head, err
	}
	body, err := jsonout.Marshal(p.Impl)
	if err != nil {
		return nil, err
	}
	if len(body) < 2 || body[0] != '{' {
		return nil, fmt.Errorf("provider %q: settings must encode as a JSON object", p.ID)
	}
	if string(body) == "{}" {
		return head, nil
	}
	// Both are objects: drop the closing brace of head and the opening one of body.
	return slices.Concat(head[:len(head)-1], []byte(","), body[1:]), nil
}

// Output is one candidate file, fed by exactly one provider.
type Output struct {
	ID       string `json:"id"`
	Path     string `json:"path"`
	Provider string `json:"provider"`
	// Candidates is 0 for a provider type without the key.
	Candidates int `json:"candidates,omitzero"`
}

// Loaded is a validated config with the location of its file.
type Loaded struct {
	// Path is the absolute config path; Dir is its directory.
	Path, Dir string
	Config    Config
}

// Provider returns the configured provider with id.
func (c Config) Provider(id string) (Provider, bool) {
	for _, p := range c.Providers {
		if p.ID == id {
			return p, true
		}
	}
	return Provider{}, false
}

var required = schema.StrOpt{Required: true, Trim: true, MinLen: 1}

// Load reads and validates the config at path. ${NAME} is not expanded.
// Unknown keys are rejected. All validation problems are reported together.
// The file system is not checked: the state directory and key are checked
// when serve, login or logout open them.
func Load(path string) (Loaded, error) {
	doc, err := configload.Load(path, configload.EnvNone, nil)
	if err != nil {
		return Loaded{}, fmt.Errorf("failed to read config %s: %w", path, err)
	}

	var cfg Config
	var issues schema.Issues
	root := schema.Root(doc.Root, &issues)
	root.Strict("server", "logging", "state", "identification", "deviceNamePrefix", "keys", "api", "providers", "outputs")

	server := root.Object("server", true)
	server.Strict("listen")
	if listen, ok := server.String("listen", required); ok {
		if _, _, err := listenaddr.Parse(listen); err != nil {
			server.Addf("listen", "%v", err)
		}
		cfg.Server.Listen = listen
	}

	cfg.Logging.Level, cfg.Logging.Format = "info", "json"
	if logging := root.Object("logging", false); logging != nil {
		logging.Strict("level", "format")
		cfg.Logging.Level = logging.Enum("level", []string{"fatal", "error", "warn", "info", "debug", "trace", "silent"}, "info")
		cfg.Logging.Format = logging.Enum("format", []string{"json", "pretty"}, "json")
	}

	state := root.Object("state", true)
	state.Strict("dir", "keyFile")
	cfg.State.Dir = absPath(state, "dir")
	cfg.State.KeyFile = absPath(state, "keyFile")

	if prefix, ok := root.String("deviceNamePrefix", required); ok {
		if !prefixPattern.MatchString(prefix) {
			root.Addf("deviceNamePrefix", "must match [a-z0-9-]+")
		}
		cfg.DeviceNamePrefix = prefix
	}

	keys := root.Object("keys", false)
	keys.Strict("revokeOldAfterMinutes")
	one := 1
	cfg.Keys.RevokeOldAfterMinutes = keys.Int("revokeOldAfterMinutes", schema.IntOpt{Min: &one})

	cfg.API = parseAPI(root.Object("api", false))

	entries := parseProviders(root)
	cfg.Outputs = parseOutputs(root, entries)
	cfg.Identification = parseIdentification(root, entries)

	cfg.Providers = []Provider{}
	byProvider := map[string]Output{}
	for _, o := range cfg.Outputs {
		byProvider[o.Provider] = o
	}
	for _, e := range entries {
		out, hasOutput := byProvider[e.p.ID]
		if e.p.Enabled && !hasOutput && e.p.ID != "" {
			e.n.Addf("", "enabled provider %q has no output", e.p.ID)
		}
		if e.known {
			e.p.Impl = e.f.New(e.n, provider.Env{
				ID:                    e.p.ID,
				Output:                provider.Output{ID: out.ID, Candidates: out.Candidates},
				Identification:        cfg.Identification[e.p.Type],
				DeviceNamePrefix:      cfg.DeviceNamePrefix,
				RevokeOldAfterMinutes: cfg.Keys.RevokeOldAfterMinutes,
				API:                   cfg.API,
			})
		}
		cfg.Providers = append(cfg.Providers, e.p)
	}

	if err := issues.Err(path); err != nil {
		return Loaded{}, err
	}
	return Loaded{Path: doc.Path, Dir: doc.Dir, Config: cfg}, nil
}

// absPath returns the required clean absolute path at key.
func absPath(n *schema.Node, key string) string {
	p, ok := n.String(key, required)
	if !ok {
		return ""
	}
	if !filepath.IsAbs(p) || filepath.Clean(p) != p {
		n.Addf(key, "must be a clean absolute path")
	}
	return p
}

func parseAPI(n *schema.Node) provider.API {
	n.Strict("timeoutMs", "retry")
	zero, one := 0, 1
	defTimeout, defAttempts, defMin, defMax := 15000, 3, 3000, 8000
	var api provider.API
	api.TimeoutMs = n.Int("timeoutMs", schema.IntOpt{Default: &defTimeout, Min: &one})
	r := n.Object("retry", false)
	r.Strict("attempts", "minDelayMs", "maxDelayMs")
	api.Retry.Attempts = r.Int("attempts", schema.IntOpt{Default: &defAttempts, Min: &one})
	api.Retry.MinDelayMs = r.Int("minDelayMs", schema.IntOpt{Default: &defMin, Min: &zero})
	api.Retry.MaxDelayMs = r.Int("maxDelayMs", schema.IntOpt{Default: &defMax, Min: &zero})
	if api.Retry.MaxDelayMs < api.Retry.MinDelayMs {
		r.Addf("maxDelayMs", "must be at least minDelayMs")
	}
	return api
}

// entry is a provider with its subtree and factory, until the factory runs.
type entry struct {
	p     Provider
	n     *schema.Node
	f     provider.Factory
	known bool
}

func parseProviders(root *schema.Node) []*entry {
	nodes := root.Array("providers", true, 1)
	var entries []*entry
	seen := map[string]bool{}
	for _, n := range nodes {
		if n == nil {
			continue
		}
		e := &entry{n: n}
		if id, ok := n.String("id", required); ok {
			if !idPattern.MatchString(id) {
				n.Addf("id", "must match [a-z0-9]+")
			}
			if seen[id] {
				n.Addf("id", "duplicate provider id %q", id)
			}
			seen[id] = true
			e.p.ID = id
		}
		if typ, ok := n.String("type", required); ok {
			e.p.Type = typ
			if e.f, e.known = provider.Lookup(typ); !e.known {
				n.Addf("type", "unknown provider type %q", typ)
			}
		}
		e.p.Enabled = n.Bool("enabled", true)
		if expr, ok := n.String("schedule", required); ok {
			if _, err := cron.Parse(expr); err != nil {
				n.Addf("schedule", "%v", err)
			}
			e.p.Schedule = expr
		}
		entries = append(entries, e)
	}
	return entries
}

func parseOutputs(root *schema.Node, entries []*entry) []Output {
	byID := map[string]*entry{}
	for _, e := range entries {
		if e.p.ID != "" {
			byID[e.p.ID] = e
		}
	}
	outputs := []Output{}
	ids, fed, paths := map[string]bool{}, map[string]string{}, map[string]bool{}
	dir := ""
	for _, n := range root.Array("outputs", true, 0) {
		if n == nil {
			continue
		}
		n.Strict("id", "path", "provider", "candidates")
		var o Output
		if id, ok := n.String("id", required); ok {
			if !idPattern.MatchString(id) {
				n.Addf("id", "must match [a-z0-9]+")
			}
			if ids[id] {
				n.Addf("id", "duplicate output id %q", id)
			}
			ids[id] = true
			o.ID = id
		}
		if p := absPath(n, "path"); p != "" {
			switch {
			case filepath.Base(p) == wgpstatus.Name:
				n.Addf("path", "must not be named %s", wgpstatus.Name)
			case paths[p]:
				n.Addf("path", "duplicate output path")
			case dir != "" && filepath.Dir(p) != dir:
				n.Addf("path", "must be in %s, like the other outputs", dir)
			}
			if dir == "" {
				dir = filepath.Dir(p)
			}
			paths[p] = true
			o.Path = p
		}
		var e *entry
		if pid, ok := n.String("provider", required); ok {
			o.Provider = pid
			switch e = byID[pid]; {
			case e == nil:
				n.Addf("provider", "unknown provider %q", pid)
			case fed[pid] != "":
				n.Addf("provider", "provider %q already feeds output %q", pid, fed[pid])
			default:
				fed[pid] = o.ID
			}
		}
		if e != nil && e.known {
			if e.f.Candidates {
				one := 1
				o.Candidates = n.Int("candidates", schema.IntOpt{Required: true, Min: &one})
			} else if n.Has("candidates") {
				n.Addf("candidates", "not allowed for provider type %q", e.p.Type)
			}
		}
		outputs = append(outputs, o)
	}
	return outputs
}

// parseIdentification parses identification.<type> of every registered type
// that has one. It is required for the types of the configured providers.
func parseIdentification(root *schema.Node, entries []*entry) map[string]any {
	used := map[string]bool{}
	for _, e := range entries {
		if e.known && e.f.Identification != nil {
			used[e.p.Type] = true
		}
	}
	var known []string
	for _, t := range provider.Types() {
		if f, _ := provider.Lookup(t); f.Identification != nil {
			known = append(known, t)
		}
	}
	n := root.Object("identification", len(used) > 0)
	n.Strict(known...)
	var out map[string]any
	for _, t := range known {
		sub := n.Object(t, used[t])
		if sub == nil && !used[t] {
			continue
		}
		f, _ := provider.Lookup(t)
		if out == nil {
			out = map[string]any{}
		}
		v := f.Identification(sub)
		if sub != nil {
			out[t] = v
		}
	}
	return out
}
