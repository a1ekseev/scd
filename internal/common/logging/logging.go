// Package logging builds the slog logger shared by all services (D9, D27):
// level names fatal..trace plus silent, and json or pretty (key=value) output.
package logging

import (
	"fmt"
	"io"
	"log/slog"
)

// Levels beyond the standard slog set.
const (
	// LevelTrace is below debug.
	LevelTrace = slog.Level(-8)
	// LevelFatal is above error.
	LevelFatal = slog.Level(12)
)

// New returns a logger writing to w. level is one of
// fatal|error|warn|info|debug|trace|silent; format is json or pretty.
func New(w io.Writer, level, format string) (*slog.Logger, error) {
	var lvl slog.Level
	silent := false
	switch level {
	case "fatal":
		lvl = LevelFatal
	case "error":
		lvl = slog.LevelError
	case "warn":
		lvl = slog.LevelWarn
	case "info":
		lvl = slog.LevelInfo
	case "debug":
		lvl = slog.LevelDebug
	case "trace":
		lvl = LevelTrace
	case "silent":
		silent = true
	default:
		return nil, fmt.Errorf("unsupported log level %q (expected fatal, error, warn, info, debug, trace or silent)", level)
	}
	opts := &slog.HandlerOptions{Level: lvl, ReplaceAttr: replaceLevel}
	var h slog.Handler
	switch format {
	case "json":
		h = slog.NewJSONHandler(w, opts)
	case "pretty":
		h = slog.NewTextHandler(w, opts)
	default:
		return nil, fmt.Errorf("unsupported log format %q (expected json or pretty)", format)
	}
	if silent {
		h = slog.DiscardHandler
	}
	return slog.New(h), nil
}

func replaceLevel(groups []string, a slog.Attr) slog.Attr {
	if len(groups) == 0 && a.Key == slog.LevelKey {
		if l, ok := a.Value.Any().(slog.Level); ok {
			switch l {
			case LevelTrace:
				a.Value = slog.StringValue("TRACE")
			case LevelFatal:
				a.Value = slog.StringValue("FATAL")
			}
		}
	}
	return a
}
