// Package schema is a small validator over a decoded YAML tree
// (map[string]any / []any / scalars). Every check that fails records an Issue
// with the dotted path of the field; all Node methods are nil-safe so that a
// missing or invalid parent does not cascade into more issues.
//
// A YAML null (an empty value) is treated as an absent key.
package schema

import (
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Issue is one validation problem. Path is dotted ("a.b.0.c"), "" is the root.
type Issue struct {
	Path    string
	Message string
}

// Issues is a list of validation problems in the order they were found.
type Issues []Issue

// Err returns nil when there are no issues, otherwise an error listing every
// issue of the config file as "\n  - <path>: <message>".
func (is Issues) Err(file string) error {
	if len(is) == 0 {
		return nil
	}
	var b strings.Builder
	b.WriteString("invalid config " + file + ":")
	for _, i := range is {
		p := i.Path
		if p == "" {
			p = "<root>"
		}
		b.WriteString("\n  - " + p + ": " + i.Message)
	}
	return errors.New(b.String())
}

// Node is an object (mapping) in the tree being validated.
type Node struct {
	m      map[string]any
	path   string
	issues *Issues
}

// StrOpt configures String, StringList and StringOrList.
type StrOpt struct {
	// Required reports a missing value as "required".
	Required bool
	// Trim removes leading and trailing white space before other checks.
	Trim bool
	// MinLen is the minimum length in characters (after Trim).
	MinLen int
	// Default is returned (with ok) when the key is absent.
	Default *string
	// EmptyToAbsent (String only) treats an empty string (after Trim) as an
	// absent key.
	EmptyToAbsent bool
	// AllowEmptyList (StringList, StringListElems) accepts an empty array and
	// returns an empty non-nil slice.
	AllowEmptyList bool
}

// IntOpt configures Int.
type IntOpt struct {
	// Required reports a missing value as "required".
	Required bool
	// Default is returned when the key is absent.
	Default *int
	// Min and Max are inclusive bounds.
	Min, Max *int
}

// StringElem is a valid element of a StringOrList value. Key is relative to
// the node: the key itself for a single string, "<key>.<i>" for a list
// element (ready for Addf).
type StringElem struct {
	Key   string
	Value string
}

// Root starts validation of v, which must be an object.
func Root(v any, issues *Issues) *Node {
	m, ok := v.(map[string]any)
	if !ok {
		*issues = append(*issues, Issue{"", mismatch("object", v)})
		return nil
	}
	return &Node{m: m, issues: issues}
}

// Path returns the dotted path of the node ("" for the root).
func (n *Node) Path() string {
	if n == nil {
		return ""
	}
	return n.path
}

// Has reports whether key is present with a non-null value.
func (n *Node) Has(key string) bool {
	_, ok := n.get(key)
	return ok
}

// Addf records an issue at <node path>.<key> (key "" means the node itself).
func (n *Node) Addf(key, format string, args ...any) {
	if n == nil {
		return
	}
	n.add(n.child(key), fmt.Sprintf(format, args...))
}

// Object returns the child object at key, or nil when it is absent or not an
// object.
func (n *Node) Object(key string, required bool) *Node {
	v, ok := n.value(key, required)
	if !ok {
		return nil
	}
	return n.object(n.child(key), v)
}

// Array returns the elements of the array at key as object nodes; a
// non-object element is reported and left nil. A shorter array than minItems
// is reported but still returned.
func (n *Node) Array(key string, required bool, minItems int) []*Node {
	v, ok := n.value(key, required)
	if !ok {
		return nil
	}
	p := n.child(key)
	list, ok := v.([]any)
	if !ok {
		n.add(p, mismatch("array", v))
		return nil
	}
	n.checkLen(p, len(list), minItems)
	out := make([]*Node, len(list))
	for i, e := range list {
		out[i] = n.object(join(p, strconv.Itoa(i)), e)
	}
	return out
}

// String returns the string at key. ok is false when the key is absent
// (without Default) or the value is invalid.
func (n *Node) String(key string, o StrOpt) (string, bool) {
	v, present := n.get(key)
	if present {
		s, valid := n.str(n.child(key), v, o)
		if s != "" || !o.EmptyToAbsent || !valid {
			return s, valid
		}
	}
	if o.Default != nil {
		return *o.Default, true
	}
	if o.Required && n != nil {
		n.add(n.child(key), "required")
	}
	return "", false
}

// StringList returns the valid elements of the string array at key. An
// empty array is reported (unless AllowEmptyList); an absent key gives nil.
func (n *Node) StringList(key string, o StrOpt) []string {
	return values(n.StringListElems(key, o))
}

// StringListElems is StringList that also returns the key of each valid
// element ("<key>.<i>", ready for Addf). With AllowEmptyList an empty array
// gives an empty non-nil slice.
func (n *Node) StringListElems(key string, o StrOpt) []StringElem {
	v, ok := n.value(key, o.Required)
	if !ok {
		return nil
	}
	p := n.child(key)
	list, ok := v.([]any)
	if !ok {
		n.add(p, mismatch("array", v))
		return nil
	}
	if o.AllowEmptyList && len(list) == 0 {
		return []StringElem{}
	}
	return n.strList(p, key, list, o)
}

// StringOrList accepts either a single string or an array of strings at key
// and returns the valid elements. An empty array is reported.
func (n *Node) StringOrList(key string, o StrOpt) []StringElem {
	v, ok := n.value(key, o.Required)
	if !ok {
		return nil
	}
	p := n.child(key)
	switch t := v.(type) {
	case string:
		if s, valid := n.str(p, t, o); valid {
			return []StringElem{{key, s}}
		}
		return nil
	case []any:
		return n.strList(p, key, t, o)
	}
	n.add(p, mismatch("string or array", v))
	return nil
}

// Int returns the YAML integer at key. Floats (even 5000.0) and strings are
// reported. Absent or invalid values give Default (or 0).
func (n *Node) Int(key string, o IntOpt) int {
	def := 0
	if o.Default != nil {
		def = *o.Default
	}
	v, ok := n.value(key, o.Required)
	if !ok {
		return def
	}
	p := n.child(key)
	var i int
	switch t := v.(type) {
	case int:
		i = t
	case int64, uint64:
		n.add(p, "integer out of range")
		return def
	default:
		n.add(p, mismatch("integer", v))
		return def
	}
	if o.Min != nil && i < *o.Min {
		n.add(p, fmt.Sprintf("must be at least %d", *o.Min))
		return def
	}
	if o.Max != nil && i > *o.Max {
		n.add(p, fmt.Sprintf("must be at most %d", *o.Max))
		return def
	}
	return i
}

// Bool returns the YAML boolean at key (strings such as "yes" are reported);
// absent or invalid values give def.
func (n *Node) Bool(key string, def bool) bool {
	v, ok := n.get(key)
	if !ok {
		return def
	}
	b, ok := v.(bool)
	if !ok {
		n.add(n.child(key), mismatch("boolean", v))
		return def
	}
	return b
}

// Enum returns the string at key, which must be one of allowed. An absent key
// gives def; def == "" makes the key required.
func (n *Node) Enum(key string, allowed []string, def string) string {
	v, ok := n.value(key, def == "")
	if !ok {
		return def
	}
	p := n.child(key)
	s, ok := v.(string)
	if !ok {
		n.add(p, mismatch("string", v))
		return def
	}
	if !slices.Contains(allowed, s) {
		n.add(p, "must be one of "+strings.Join(allowed, "|"))
		return def
	}
	return s
}

// URL returns the string at key, which must parse with net/url and have a
// scheme and a host.
func (n *Node) URL(key string, required bool) (string, bool) {
	v, ok := n.value(key, required)
	if !ok {
		return "", false
	}
	p := n.child(key)
	s, ok := v.(string)
	if !ok {
		n.add(p, mismatch("string", v))
		return "", false
	}
	u, err := url.Parse(s)
	if err != nil || u.Scheme == "" || u.Host == "" {
		n.add(p, "invalid URL")
		return "", false
	}
	return s, true
}

// StringMap returns the object at key as a map of strings (e.g. HTTP
// headers); keys are kept as written. Absent gives nil.
func (n *Node) StringMap(key string) map[string]string {
	v, ok := n.get(key)
	if !ok {
		return nil
	}
	p := n.child(key)
	m, ok := v.(map[string]any)
	if !ok {
		n.add(p, mismatch("object", v))
		return nil
	}
	out := make(map[string]string, len(m))
	for _, k := range sortedKeys(m) {
		s, ok := m[k].(string)
		if !ok {
			n.add(join(p, k), mismatch("string", m[k]))
			continue
		}
		out[k] = s
	}
	return out
}

// Strict reports every key of the node that is not in known, in sorted
// order, as `unknown key "x"` at the node path.
func (n *Node) Strict(known ...string) {
	if n == nil {
		return
	}
	for _, k := range sortedKeys(n.m) {
		if !slices.Contains(known, k) {
			n.add(n.path, fmt.Sprintf("unknown key %q", k))
		}
	}
}

func (n *Node) get(key string) (any, bool) {
	if n == nil {
		return nil, false
	}
	v, ok := n.m[key]
	return v, ok && v != nil
}

// value is get plus a "required" issue for an absent required key.
func (n *Node) value(key string, required bool) (any, bool) {
	v, ok := n.get(key)
	if !ok && required && n != nil {
		n.add(n.child(key), "required")
	}
	return v, ok
}

func (n *Node) object(p string, v any) *Node {
	m, ok := v.(map[string]any)
	if !ok {
		n.add(p, mismatch("object", v))
		return nil
	}
	return &Node{m: m, path: p, issues: n.issues}
}

func (n *Node) str(p string, v any, o StrOpt) (string, bool) {
	s, ok := v.(string)
	if !ok {
		n.add(p, mismatch("string", v))
		return "", false
	}
	if o.Trim {
		s = strings.TrimSpace(s)
	}
	if s == "" && o.EmptyToAbsent {
		return "", true
	}
	if l := utf8.RuneCountInString(s); l < o.MinLen {
		if o.MinLen == 1 {
			n.add(p, "must not be empty")
		} else {
			n.add(p, fmt.Sprintf("must be at least %d characters long", o.MinLen))
		}
		return "", false
	}
	return s, true
}

func (n *Node) strList(p, key string, list []any, o StrOpt) []StringElem {
	n.checkLen(p, len(list), 1)
	var out []StringElem
	for i, e := range list {
		idx := strconv.Itoa(i)
		if s, ok := n.str(join(p, idx), e, o); ok {
			out = append(out, StringElem{key + "." + idx, s})
		}
	}
	return out
}

func (n *Node) checkLen(p string, got, minItems int) {
	switch {
	case got >= minItems:
	case minItems == 1:
		n.add(p, "must not be empty")
	default:
		n.add(p, fmt.Sprintf("must have at least %d items", minItems))
	}
}

func (n *Node) child(key string) string { return join(n.path, key) }

func (n *Node) add(p, msg string) { *n.issues = append(*n.issues, Issue{p, msg}) }

func join(p, key string) string {
	switch {
	case key == "":
		return p
	case p == "":
		return key
	}
	return p + "." + key
}

func values(elems []StringElem) []string {
	if elems == nil {
		return nil
	}
	out := make([]string, len(elems))
	for i, e := range elems {
		out[i] = e.Value
	}
	return out
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

func mismatch(want string, v any) string {
	return "expected " + want + ", got " + typeName(v)
}

func typeName(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case string:
		return "string"
	case bool:
		return "boolean"
	case int, int64, uint64:
		return "int"
	case float64:
		return "float"
	case map[string]any:
		return "object"
	case []any:
		return "array"
	}
	return fmt.Sprintf("%T", v)
}
