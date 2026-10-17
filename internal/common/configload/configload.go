// Package configload reads a YAML config file into a generic tree with
// optional ${NAME} environment interpolation.
package configload

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"go.yaml.in/yaml/v3"
)

// EnvMode selects how ${NAME} references are expanded.
type EnvMode int

const (
	// EnvNone keeps ${NAME} literally (gtl).
	EnvNone EnvMode = iota
	// EnvRawStrict expands ${NAME} in the raw text before YAML parsing,
	// comments included; an undefined variable is an error (scd).
	EnvRawStrict
	// EnvStringsLenient expands ${NAME} after YAML parsing in string values
	// only (keys are kept); an undefined variable becomes "" (sfc).
	EnvStringsLenient
)

var envRef = regexp.MustCompile(`(?i)\$\{([A-Z0-9_]+)\}`)

var httpInput = regexp.MustCompile(`^(?i)https?://`)

// Doc is a loaded config file.
type Doc struct {
	// Path is the absolute path of the file.
	Path string
	// Dir is the directory of Path.
	Dir string
	// Root is the decoded YAML: maps are map[string]any, sequences []any.
	// An empty document is an empty map.
	Root any
}

// Load reads path, applies env interpolation according to mode and decodes
// a single YAML document. lookup resolves variables (os.LookupEnv when nil).
// File system errors are returned as is (errors.Is(err, fs.ErrNotExist)
// works); YAML errors are those of go.yaml.in/yaml/v3. Callers add the file
// context.
func Load(path string, mode EnvMode, lookup func(string) (string, bool)) (Doc, error) {
	if lookup == nil {
		lookup = os.LookupEnv
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return Doc{}, err
	}
	raw, err := os.ReadFile(abs)
	if err != nil {
		return Doc{}, err
	}
	text := string(raw)
	if mode == EnvRawStrict {
		if text, err = expandStrict(text, lookup); err != nil {
			return Doc{}, err
		}
	}
	root, err := decodeSingle(text)
	if err != nil {
		return Doc{}, err
	}
	if mode == EnvStringsLenient {
		root = expandLenient(root, lookup)
	}
	return Doc{Path: abs, Dir: filepath.Dir(abs), Root: root}, nil
}

// ResolveInput resolves a subscription input against the config directory:
// "-", http(s) URLs (case-insensitive scheme) and absolute paths are kept,
// anything else is joined to dir.
func ResolveInput(dir, input string) string {
	if input == "-" || httpInput.MatchString(input) || filepath.IsAbs(input) {
		return input
	}
	return filepath.Join(dir, input)
}

func expandStrict(text string, lookup func(string) (string, bool)) (string, error) {
	var missing error
	out := envRef.ReplaceAllStringFunc(text, func(m string) string {
		if missing != nil {
			return m
		}
		name := envRef.FindStringSubmatch(m)[1]
		v, ok := lookup(name)
		if !ok {
			missing = fmt.Errorf("missing environment variable %q required by config", name)
		}
		return v
	})
	return out, missing
}

func expandLenient(v any, lookup func(string) (string, bool)) any {
	switch t := v.(type) {
	case string:
		return envRef.ReplaceAllStringFunc(t, func(m string) string {
			val, _ := lookup(envRef.FindStringSubmatch(m)[1])
			return val
		})
	case map[string]any:
		for k, e := range t {
			t[k] = expandLenient(e, lookup)
		}
	case []any:
		for i, e := range t {
			t[i] = expandLenient(e, lookup)
		}
	}
	return v
}

func decodeSingle(text string) (any, error) {
	dec := yaml.NewDecoder(strings.NewReader(text))
	var root any
	if err := dec.Decode(&root); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err != nil {
			return nil, err
		}
		return nil, errors.New("config must contain a single YAML document")
	}
	if root == nil {
		return map[string]any{}, nil
	}
	return normalize(root), nil
}

// normalize turns map[any]any (YAML mappings with non-string keys) into
// map[string]any so that callers only see one map type.
func normalize(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, e := range t {
			t[k] = normalize(e)
		}
	case map[any]any:
		m := make(map[string]any, len(t))
		for k, e := range t {
			m[fmt.Sprint(k)] = normalize(e)
		}
		return m
	case []any:
		for i, e := range t {
			t[i] = normalize(e)
		}
	}
	return v
}
