// Package config loads and validates the gtl config file.
package config

import (
	"fmt"
	"math"
	"strings"

	"github.com/a1ekseev/scd/internal/common/configload"
	"github.com/a1ekseev/scd/internal/common/listenaddr"
	"github.com/a1ekseev/scd/internal/common/schema"
)

const (
	defaultMaxSizeKb = 10240
	// maxSizeKbLimit is the largest size whose byte count fits in int64.
	maxSizeKbLimit = math.MaxInt64 / 1024
)

// Config is the normalized gtl configuration. Field order is the print-config
// JSON order.
type Config struct {
	Server struct {
		Listen string `json:"listen"`
	} `json:"server"`
	Logging struct {
		Level  string `json:"level"`
		Format string `json:"format"`
	} `json:"logging"`
	Load struct {
		Path      string `json:"path"`
		MaxSizeKb int64  `json:"maxSizeKb"`
	} `json:"load"`
}

// Load reads and validates the config at path. All validation problems are
// reported together. ${NAME} references are not interpolated.
func Load(path string) (Config, error) {
	doc, err := configload.Load(path, configload.EnvNone, nil)
	if err != nil {
		return Config{}, fmt.Errorf("failed to read config %s: %w", path, err)
	}

	var cfg Config
	var issues schema.Issues
	root := schema.Root(doc.Root, &issues)
	root.Strict("server", "logging", "load")

	server := root.Object("server", true)
	server.Strict("listen")
	if listen, ok := server.String("listen", schema.StrOpt{Required: true}); ok {
		if _, _, err := listenaddr.Parse(listen); err != nil {
			server.Addf("listen", "%s", err.Error())
		} else {
			cfg.Server.Listen = listen
		}
	}

	cfg.Logging.Level, cfg.Logging.Format = "info", "json"
	if logging := root.Object("logging", false); logging != nil {
		logging.Strict("level", "format")
		cfg.Logging.Level = logging.Enum("level", []string{"fatal", "error", "warn", "info", "debug", "trace", "silent"}, "info")
		cfg.Logging.Format = logging.Enum("format", []string{"json", "pretty"}, "json")
	}

	load := root.Object("load", true)
	load.Strict("path", "maxSizeKb")
	if p, ok := load.String("path", schema.StrOpt{Required: true}); ok {
		cfg.Load.Path = p
		validatePath(load, p)
	}
	minSize, maxSize, def := 1, int(maxSizeKbLimit), defaultMaxSizeKb
	cfg.Load.MaxSizeKb = int64(load.Int("maxSizeKb", schema.IntOpt{Default: &def, Min: &minSize, Max: &maxSize}))

	if err := issues.Err(path); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func validatePath(load *schema.Node, p string) {
	if !strings.HasPrefix(p, "/") {
		load.Addf("path", "must start with /")
	}
	if p == "/" {
		load.Addf("path", "must not equal /")
	}
	if len(p) > 1 && strings.HasSuffix(p, "/") {
		load.Addf("path", "must not end with /")
	}
	if strings.ContainsAny(p, "?#") {
		load.Addf("path", "must not contain query or hash")
	}
}
