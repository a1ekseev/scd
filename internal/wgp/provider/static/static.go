// Package static is the wgp provider type "static": it publishes every .conf
// file of a directory (wg-quick format, exactly one [Peer]) as a
// "wireguard://" candidate line with provider=static. Files are read in name
// order; a rejected file does not stop the others. Hook keys (PostUp, …) are
// ignored with a warning and never run.
package static

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/a1ekseev/scd/internal/common/candidate"
	"github.com/a1ekseev/scd/internal/common/schema"
	"github.com/a1ekseev/scd/internal/common/wgpstatus"
	"github.com/a1ekseev/scd/internal/wgp/provider"
)

// Type is the provider type name.
const Type = "static"

// Defaults fill the fields a .conf file leaves out.
type Defaults struct {
	MTU        int      `json:"mtu"`
	AllowedIPs []string `json:"allowedIPs"`
	DNS        []string `json:"dns"`
}

// Static is one configured static provider. Its JSON is its config keys.
type Static struct {
	id           string
	Dir          string   `json:"dir"`
	MaxFileBytes int      `json:"maxFileBytes"`
	Defaults     Defaults `json:"defaults"`
}

func init() {
	provider.Register(Type, provider.Factory{New: newStatic})
}

func newStatic(n *schema.Node, env provider.Env) provider.Provider {
	n.Strict(slices.Concat(provider.CommonKeys, []string{"dir", "maxFileBytes", "defaults"})...)
	s := &Static{id: env.ID}
	if dir, ok := n.String("dir", schema.StrOpt{Required: true, Trim: true, MinLen: 1}); ok {
		if !filepath.IsAbs(dir) || filepath.Clean(dir) != dir {
			n.Addf("dir", "must be a clean absolute path")
		}
		s.Dir = dir
	}
	one, defMax := 1, 64<<10
	s.MaxFileBytes = n.Int("maxFileBytes", schema.IntOpt{Default: &defMax, Min: &one})

	d := n.Object("defaults", false)
	d.Strict("mtu", "allowedIPs", "dns")
	minMTU, maxMTU, defMTU := 576, 1500, 1420
	s.Defaults.MTU = d.Int("mtu", schema.IntOpt{Default: &defMTU, Min: &minMTU, Max: &maxMTU})
	s.Defaults.AllowedIPs = []string{"0.0.0.0/0", "::/0"}
	if d.Has("allowedIPs") {
		s.Defaults.AllowedIPs = d.StringList("allowedIPs", schema.StrOpt{Trim: true, MinLen: 1})
	}
	s.Defaults.DNS = []string{}
	if d.Has("dns") {
		s.Defaults.DNS = d.StringList("dns", schema.StrOpt{Trim: true, MinLen: 1, AllowEmptyList: true})
	}
	// The defaults go through the candidate parser like the file fields.
	if _, err := candidate.ParseWireGuard(s.line(conf{
		privateKey: dummyKey, endpoint: "192.0.2.1:1", publicKey: dummyKey, address: []string{"192.0.2.2"},
	})); err != nil {
		n.Addf("defaults", "%v", err)
	}
	return s
}

// dummyKey is a valid key for checking the defaults alone.
const dummyKey = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="

// ID implements provider.Provider.
func (s *Static) ID() string { return s.id }

// Kind implements provider.Provider.
func (*Static) Kind() candidate.Kind { return candidate.KindWireGuard }

// Tick implements provider.Provider: it reads every .conf file of Dir. A
// rejected file is a warning and makes the state error; the other files are
// still published. No accepted file is an error.
func (s *Static) Tick(context.Context, provider.Store) (provider.Result, error) {
	entries, err := os.ReadDir(s.Dir) // sorted by name, as strings.Compare
	if err != nil {
		return provider.Result{}, err
	}
	var res provider.Result
	var rejected []string
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".conf") {
			continue
		}
		line, warns, err := s.file(e.Name())
		for _, w := range warns {
			res.Warnings = append(res.Warnings, e.Name()+": "+w)
		}
		if err != nil {
			rejected = append(rejected, e.Name()+": "+err.Error())
			continue
		}
		res.Lines = append(res.Lines, line)
	}
	if len(res.Lines) == 0 {
		if len(rejected) == 0 {
			return provider.Result{}, fmt.Errorf("no .conf files in %s", s.Dir)
		}
		return provider.Result{}, errors.New(strings.Join(rejected, "; "))
	}
	res.Status = wgpstatus.Provider{State: wgpstatus.StateOK, Issued: len(res.Lines)}
	if len(rejected) > 0 {
		res.Status.State, res.Status.Error = wgpstatus.StateError, strings.Join(rejected, "; ")
	}
	return res, nil
}

// file reads and converts one .conf file.
func (s *Static) file(name string) (line string, warnings []string, err error) {
	p := filepath.Join(s.Dir, name)
	info, err := os.Lstat(p)
	switch {
	case err != nil:
		return "", nil, err
	case !info.Mode().IsRegular():
		return "", nil, errors.New("not a regular file")
	case info.Mode().Perm()&0o007 != 0:
		return "", nil, fmt.Errorf("mode %#o: must have no access for others", info.Mode().Perm())
	}
	f, err := os.Open(p)
	if err != nil {
		return "", nil, err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, int64(s.MaxFileBytes)+1))
	if err != nil {
		return "", nil, err
	}
	if len(data) > s.MaxFileBytes {
		return "", nil, fmt.Errorf("larger than maxFileBytes %d", s.MaxFileBytes)
	}
	c, warnings, err := parseConf(data)
	if err != nil {
		return "", warnings, err
	}
	if c.label == "" {
		c.label = strings.TrimSuffix(name, ".conf")
	}
	w, err := candidate.ParseWireGuard(s.line(c))
	if err != nil {
		return "", warnings, err
	}
	return candidate.FormatWireGuard(w), warnings, nil
}

// conf holds the raw values of a .conf file; lists are already split.
type conf struct {
	privateKey, endpoint, publicKey, presharedKey, mtu, keepalive, reserved, label string
	address, dns, allowedIPs                                                       []string
}

// line builds the candidate line of c with the defaults; ParseWireGuard
// then applies the same checks as scd.
func (s *Static) line(c conf) string {
	q := []string{"provider=" + Type}
	add := func(k string, v ...string) {
		if j := strings.Join(v, ","); j != "" {
			q = append(q, k+"="+url.QueryEscape(j))
		}
	}
	add("publickey", c.publicKey)
	add("address", c.address...)
	add("dns", orDefault(c.dns, s.Defaults.DNS)...)
	add("mtu", orDefault([]string{c.mtu}, []string{strconv.Itoa(s.Defaults.MTU)})...)
	add("allowedips", orDefault(c.allowedIPs, s.Defaults.AllowedIPs)...)
	add("reserved", c.reserved)
	if !strings.EqualFold(c.keepalive, "off") {
		add("keepalive", c.keepalive)
	}
	add("presharedkey", c.presharedKey)
	return "wireguard://" + url.User(c.privateKey).String() + "@" + c.endpoint + "?" + strings.Join(q, "&") + "#" + url.PathEscape(c.label)
}

// orDefault returns v, or def when v is empty.
func orDefault(v, def []string) []string {
	if strings.Join(v, "") == "" {
		return def
	}
	return v
}

// ignored are wg-quick keys that wgp never runs or uses.
var ignored = map[string]bool{
	"listenport": true, "table": true, "fwmark": true, "saveconfig": true,
	"preup": true, "postup": true, "predown": true, "postdown": true,
}

// parseConf parses the wg-quick format: one [Interface] and exactly one
// [Peer]; keys are case-insensitive; Address, DNS and AllowedIPs may repeat.
// Errors name lines and keys, never values: the file holds private keys.
func parseConf(data []byte) (conf, []string, error) {
	var c conf
	var warnings []string
	section := ""
	counts := map[string]int{}
	seen := map[string]bool{}
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(nil, len(data)+1)
	for n := 1; sc.Scan(); n++ {
		raw := strings.TrimSpace(sc.Text())
		if comment, ok := strings.CutPrefix(raw, "#"); ok {
			k, v, ok := strings.Cut(comment, "=")
			if !ok {
				continue
			}
			k, v = strings.ToLower(strings.TrimSpace(k)), strings.TrimSpace(v)
			if k != "label" && k != "reserved" {
				continue
			}
			if seen["#"+k] {
				return c, warnings, fmt.Errorf("line %d: repeated # %s", n, k)
			}
			seen["#"+k] = true
			if k == "label" {
				c.label = v
			} else {
				c.reserved = strings.Join(splitList(v), ",")
			}
			continue
		}
		text, _, _ := strings.Cut(raw, "#")
		text = strings.TrimSpace(text)
		if text == "" {
			continue
		}
		if name, ok := strings.CutPrefix(text, "["); ok && strings.HasSuffix(name, "]") {
			section = strings.ToLower(strings.TrimSuffix(name, "]"))
			if section != "interface" && section != "peer" {
				return c, warnings, fmt.Errorf("line %d: unknown section", n)
			}
			counts[section]++
			clear(seen)
			continue
		}
		k, v, ok := strings.Cut(text, "=")
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if !ok || k == "" || strings.ContainsFunc(k, func(r rune) bool { return (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') }) {
			return c, warnings, fmt.Errorf("line %d: not a key = value line", n)
		}
		key := strings.ToLower(k)
		if section == "" {
			return c, warnings, fmt.Errorf("line %d: key %s outside a section", n, k)
		}
		if section == "interface" && ignored[key] {
			warnings = append(warnings, fmt.Sprintf("line %d: %s ignored", n, k))
			continue
		}
		target, list := c.field(section, key)
		if target == nil && list == nil {
			return c, warnings, fmt.Errorf("line %d: unknown key %s in [%s]", n, k, section)
		}
		if list != nil {
			*list = append(*list, splitList(v)...)
			continue
		}
		if seen[key] {
			return c, warnings, fmt.Errorf("line %d: repeated key %s", n, k)
		}
		seen[key] = true
		*target = v
	}
	if err := sc.Err(); err != nil {
		return c, warnings, err
	}
	switch {
	case counts["interface"] != 1:
		return c, warnings, fmt.Errorf("must have exactly one [Interface], has %d", counts["interface"])
	case counts["peer"] != 1:
		return c, warnings, fmt.Errorf("must have exactly one [Peer], has %d", counts["peer"])
	case strings.ContainsFunc(c.endpoint, func(r rune) bool { return strings.ContainsRune("/?#@ \t", r) }):
		// The endpoint is pasted into the line: keep it from adding parts.
		return c, warnings, errors.New("endpoint: must be host:port")
	}
	return c, warnings, nil
}

// field returns where key of section goes: a single value or a list.
func (c *conf) field(section, key string) (*string, *[]string) {
	switch section + "." + key {
	case "interface.privatekey":
		return &c.privateKey, nil
	case "interface.address":
		return nil, &c.address
	case "interface.dns":
		return nil, &c.dns
	case "interface.mtu":
		return &c.mtu, nil
	case "peer.publickey":
		return &c.publicKey, nil
	case "peer.presharedkey":
		return &c.presharedKey, nil
	case "peer.endpoint":
		return &c.endpoint, nil
	case "peer.allowedips":
		return nil, &c.allowedIPs
	case "peer.persistentkeepalive":
		return &c.keepalive, nil
	}
	return nil, nil
}

// splitList splits a comma-separated value and trims the items.
func splitList(v string) []string {
	parts := strings.Split(v, ",")
	for i, p := range parts {
		parts[i] = strings.TrimSpace(p)
	}
	return parts
}
