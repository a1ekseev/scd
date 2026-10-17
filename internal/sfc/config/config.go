// Package config loads and validates the sfc config file.
package config

import (
	"fmt"
	"os"
	"strings"

	"github.com/a1ekseev/scd/internal/common/configload"
	"github.com/a1ekseev/scd/internal/common/cron"
	"github.com/a1ekseev/scd/internal/common/inputsrc"
	"github.com/a1ekseev/scd/internal/common/jsonout"
	"github.com/a1ekseev/scd/internal/common/labelfilter"
	"github.com/a1ekseev/scd/internal/common/listenaddr"
	"github.com/a1ekseev/scd/internal/common/schema"
)

const defaultFetchTimeoutMs = 5000

// Config is the normalized sfc configuration. Field order is the print-config
// JSON order.
type Config struct {
	Subscriptions []Subscription `json:"subscriptions"`
	Runtime       struct {
		RefreshSchedule string `json:"refreshSchedule"`
	} `json:"runtime"`
	Server struct {
		Listen string `json:"listen"`
	} `json:"server"`
	Logging struct {
		Level  string `json:"level"`
		Format string `json:"format"`
	} `json:"logging"`
}

// Subscription is one upstream subscription and the outputs filtered from it.
type Subscription struct {
	ID string `json:"id"`
	// Input is "-" (stdin), an http(s) URL or an absolute file path.
	Input          string `json:"input"`
	Enabled        bool   `json:"enabled"`
	Format         string `json:"format"`
	FetchTimeoutMs int    `json:"fetchTimeoutMs"`
	// FetchUserAgent is empty when not configured (the fetcher then uses
	// its default, D27 O9).
	FetchUserAgent string `json:"fetchUserAgent,omitempty"`
	// FetchHeaders are extra request headers (D33); nil when not configured.
	// Values are printed as "<redacted>".
	FetchHeaders inputsrc.Headers `json:"fetchHeaders,omitempty"`
	PathRoute    string           `json:"pathRoute"`
	Outputs      []Output         `json:"outputs"`
}

// Output is one filtered view of a subscription served at
// <pathRoute>/<id>. It is printed by MarshalJSON with the filter keys
// labelIncludeRegex/labelExcludeRegex in place of Filter.
type Output struct {
	ID                    string             `json:"id"`
	Enabled               bool               `json:"enabled"`
	Name                  string             `json:"name,omitempty"`
	Filter                labelfilter.Filter `json:"-"`
	UserAgent             []string           `json:"userAgent,omitempty"`
	ProfileTitle          string             `json:"profileTitle,omitempty"`
	ProfileUpdateInterval int                `json:"profileUpdateInterval,omitempty"`
}

// MarshalJSON prints the output in config form: id, enabled, name,
// labelIncludeRegex, labelExcludeRegex, userAgent, profileTitle,
// profileUpdateInterval; empty optional keys are omitted.
func (o Output) MarshalJSON() ([]byte, error) {
	return jsonout.Marshal(struct {
		ID                    string   `json:"id"`
		Enabled               bool     `json:"enabled"`
		Name                  string   `json:"name,omitempty"`
		Include               []string `json:"labelIncludeRegex,omitempty"`
		Exclude               []string `json:"labelExcludeRegex,omitempty"`
		UserAgent             []string `json:"userAgent,omitempty"`
		ProfileTitle          string   `json:"profileTitle,omitempty"`
		ProfileUpdateInterval int      `json:"profileUpdateInterval,omitempty"`
	}{o.ID, o.Enabled, o.Name, o.Filter.IncludeSrc, o.Filter.ExcludeSrc, o.UserAgent, o.ProfileTitle, o.ProfileUpdateInterval})
}

// Loaded is a validated config with the location of its file.
type Loaded struct {
	// Path is the absolute config path; Dir is its directory (the base for
	// relative inputs and the source cache).
	Path, Dir string
	Config    Config
}

// Load reads and validates the config at path. ${NAME} references in string
// values are replaced after YAML parsing (an undefined variable becomes "").
// Unknown keys are rejected. All validation problems are reported together.
func Load(path string) (Loaded, error) {
	doc, err := configload.Load(path, configload.EnvStringsLenient, os.LookupEnv)
	if err != nil {
		return Loaded{}, fmt.Errorf("failed to read config %s: %w", path, err)
	}

	var cfg Config
	var issues schema.Issues
	root := schema.Root(doc.Root, &issues)
	root.Strict("subscriptions", "runtime", "server", "logging")
	required := schema.StrOpt{Required: true, Trim: true, MinLen: 1}

	ids := ids{subs: map[string]bool{}, outputs: map[string]bool{}}
	nodes := root.Array("subscriptions", true, 1)
	for _, sn := range nodes {
		if sn != nil {
			cfg.Subscriptions = append(cfg.Subscriptions, parseSubscription(sn, doc.Dir, &ids))
		}
	}
	checkEnabled(root, cfg.Subscriptions)

	runtime := root.Object("runtime", true)
	runtime.Strict("refreshSchedule")
	if expr, ok := runtime.String("refreshSchedule", schema.StrOpt{Required: true, Trim: true}); ok {
		if _, err := cron.Parse(expr); err != nil {
			runtime.Addf("refreshSchedule", "%v", err)
		}
		cfg.Runtime.RefreshSchedule = expr
	}

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

	if err := issues.Err(path); err != nil {
		return Loaded{}, err
	}
	return Loaded{Path: doc.Path, Dir: doc.Dir, Config: cfg}, nil
}

// ids tracks subscription and output ids; both must be unique across all
// subscriptions, disabled ones included.
type ids struct {
	subs, outputs map[string]bool
}

func parseSubscription(n *schema.Node, dir string, seen *ids) Subscription {
	n.Strict("id", "input", "enabled", "format", "fetchTimeoutMs", "fetchUserAgent", "fetchHeaders", "pathRoute", "outputs")
	required := schema.StrOpt{Required: true, Trim: true, MinLen: 1}
	var s Subscription
	if id, ok := n.String("id", required); ok {
		if seen.subs[id] {
			n.Addf("id", "duplicate subscription id %q", id)
		}
		seen.subs[id] = true
		s.ID = id
	}
	if input, ok := n.String("input", required); ok {
		s.Input = configload.ResolveInput(dir, input)
	}
	s.Enabled = n.Bool("enabled", true)
	s.Format = n.Enum("format", []string{"auto", "plain", "base64"}, "auto")
	minTimeout, defTimeout := 1, defaultFetchTimeoutMs
	s.FetchTimeoutMs = n.Int("fetchTimeoutMs", schema.IntOpt{Default: &defTimeout, Min: &minTimeout})
	s.FetchUserAgent, _ = n.String("fetchUserAgent", schema.StrOpt{Trim: true, MinLen: 1})
	if h := n.StringMap("fetchHeaders"); len(h) > 0 {
		for _, is := range inputsrc.CheckHeaders(h) {
			n.Addf("fetchHeaders."+is.Key, "%s", is.Message)
		}
		s.FetchHeaders = inputsrc.Headers(h)
	}
	if route, ok := n.String("pathRoute", required); ok {
		checkPathRoute(n, route)
		s.PathRoute = route
	}
	for _, on := range n.Array("outputs", true, 1) {
		if on != nil {
			s.Outputs = append(s.Outputs, parseOutput(on, seen))
		}
	}
	return s
}

func checkPathRoute(n *schema.Node, route string) {
	if !strings.HasPrefix(route, "/") {
		n.Addf("pathRoute", "must start with /")
	}
	if route == "/" {
		n.Addf("pathRoute", "must not equal /")
	}
	if len(route) > 1 && strings.HasSuffix(route, "/") {
		n.Addf("pathRoute", "must not end with /")
	}
}

func parseOutput(n *schema.Node, seen *ids) Output {
	n.Strict("id", "enabled", "name", labelfilter.IncludeKey, labelfilter.ExcludeKey, "userAgent", "profileTitle", "profileUpdateInterval")
	var o Output
	if id, ok := n.String("id", schema.StrOpt{Required: true, Trim: true, MinLen: 1}); ok {
		if seen.outputs[id] {
			n.Addf("id", "duplicate output id %q", id)
		}
		seen.outputs[id] = true
		o.ID = id
	}
	o.Enabled = n.Bool("enabled", true)
	o.Name, _ = n.String("name", schema.StrOpt{Trim: true, EmptyToAbsent: true})
	o.Filter = labelfilter.Parse(n)
	// An invalid filter is already reported by Parse; only a missing one is
	// reported here (D29: sfc outputs need at least one of the keys).
	if !n.Has(labelfilter.IncludeKey) && !n.Has(labelfilter.ExcludeKey) {
		n.Addf("", "%s or %s is required", labelfilter.IncludeKey, labelfilter.ExcludeKey)
	}
	o.UserAgent = n.StringList("userAgent", schema.StrOpt{Trim: true, MinLen: 1})
	o.ProfileTitle, _ = n.String("profileTitle", schema.StrOpt{Trim: true, EmptyToAbsent: true})
	minInterval, maxInterval := 1, 24
	o.ProfileUpdateInterval = n.Int("profileUpdateInterval", schema.IntOpt{Min: &minInterval, Max: &maxInterval})
	return o
}

// checkEnabled requires at least one enabled subscription with at least one
// enabled output. Nothing is reported when no subscription could be parsed
// (that problem is already reported).
func checkEnabled(root *schema.Node, subs []Subscription) {
	if len(subs) == 0 {
		return
	}
	anySub, anyOutput := false, false
	for _, s := range subs {
		if !s.Enabled {
			continue
		}
		anySub = true
		for _, o := range s.Outputs {
			anyOutput = anyOutput || o.Enabled
		}
	}
	switch {
	case !anySub:
		root.Addf("subscriptions", "at least one subscription must be enabled")
	case !anyOutput:
		root.Addf("subscriptions", "at least one output must be enabled")
	}
}
