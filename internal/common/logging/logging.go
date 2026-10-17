// Package logging builds the slog logger shared by all services (D9, D27):
// level names fatal..trace plus silent, and json or pretty (key=value) output.
package logging

import (
	"bytes"
	"encoding"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"reflect"
	"strings"
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
	var h slog.Handler
	switch format {
	case "json":
		h = slog.NewJSONHandler(w, &slog.HandlerOptions{Level: lvl, ReplaceAttr: replaceLevel})
	case "pretty":
		h = slog.NewTextHandler(w, &slog.HandlerOptions{Level: lvl, ReplaceAttr: replacePretty})
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

// replacePretty is the pretty (text) ReplaceAttr: the level names of
// replaceLevel, and slices, arrays, maps and structs rendered as their JSON
// text instead of Go's %+v (which shows Go field names), so nested values
// such as subscriptions, failedItems or causes read like in the json format.
// Errors, fmt.Stringers, encoding.TextMarshalers, []byte, time.Time and
// time.Duration keep the text handler's rendering; so does a value that
// cannot be marshalled.
func replacePretty(groups []string, a slog.Attr) slog.Attr {
	a = replaceLevel(groups, a)
	if a.Value.Kind() != slog.KindAny {
		return a
	}
	v := a.Value.Any()
	switch v.(type) {
	case nil, error, fmt.Stringer, encoding.TextMarshaler, []byte: // time.Time and time.Duration are Stringers
		return a
	}
	switch reflect.TypeOf(v).Kind() {
	case reflect.Slice, reflect.Array, reflect.Map, reflect.Struct:
		var b bytes.Buffer
		enc := json.NewEncoder(&b)
		enc.SetEscapeHTML(false) // "<redacted>", "a&b" stay readable
		if err := enc.Encode(v); err == nil {
			a.Value = slog.StringValue(strings.TrimSuffix(b.String(), "\n"))
		}
	}
	return a
}
