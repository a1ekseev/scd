package status

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/a1ekseev/scd/internal/common/inputsrc"
	"github.com/a1ekseev/scd/internal/common/jsonout"
	"github.com/a1ekseev/scd/internal/common/neterr"
	"github.com/a1ekseev/scd/internal/scd/config"
)

// Remote snapshot validation errors (03 §7).
var (
	errNotObject         = errors.New("Remote status response must be a JSON object.")     //nolint:revive,staticcheck // contract text of the status API (03 §7)
	errUnsupportedSchema = errors.New("Remote status response has an unsupported schema.") //nolint:revive,staticcheck // contract text of the status API (03 §7)
)

// remoteState is the collection state of one remote instance.
type remoteState struct {
	id, apiURL                              string
	lastAttemptAt, lastSuccessAt, lastError string
	// raw is the last valid snapshot as received (passed through to
	// /api/instances with unknown fields); parsed is its typed form.
	raw    json.RawMessage
	parsed *InstanceSnapshot
}

// Aggregator holds the last local snapshot and the state of every remote
// instance (TS StatusAggregationState). It is safe for concurrent use.
type Aggregator struct {
	mu      sync.Mutex
	local   *InstanceSnapshot
	remotes map[string]*remoteState
}

// NewAggregator returns an aggregator with a placeholder for every remote.
func NewAggregator(remotes []config.RemoteStatusInstanceConfig) *Aggregator {
	a := &Aggregator{remotes: make(map[string]*remoteState, len(remotes))}
	for _, r := range remotes {
		a.remotes[r.ID] = &remoteState{id: r.ID, apiURL: r.APIURL}
	}
	return a
}

// StoreLocal replaces the local snapshot.
func (a *Aggregator) StoreLocal(s InstanceSnapshot) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.local = &s
}

// sortedRemotes returns the remotes sorted by id; a.mu must be held.
func (a *Aggregator) sortedRemotes() []*remoteState {
	list := make([]*remoteState, 0, len(a.remotes))
	for _, r := range a.remotes {
		list = append(list, r)
	}
	slices.SortFunc(list, func(x, y *remoteState) int { return strings.Compare(x.id, y.id) })
	return list
}

// View returns the dashboard view: the local snapshot (zero before the
// first store) and the remotes sorted by id. The returned snapshots share
// their slices and RemoteView.Snapshot with the aggregator and must not be
// mutated: stored snapshots are only ever replaced, never modified, so
// readers need no lock.
func (a *Aggregator) View() AggregatedView {
	a.mu.Lock()
	defer a.mu.Unlock()
	var v AggregatedView
	if a.local != nil {
		v.Local = *a.local
	}
	for _, r := range a.sortedRemotes() {
		v.Remotes = append(v.Remotes, RemoteView{
			ID: r.id, APIURL: r.apiURL,
			LastAttemptAt: r.lastAttemptAt, LastSuccessAt: r.lastSuccessAt, LastError: r.lastError,
			Snapshot: r.parsed,
		})
	}
	return v
}

// localInstance is a local entry of /api/instances.
type localInstance struct {
	ID            string            `json:"id"`
	Source        string            `json:"source"`
	LastSuccessAt string            `json:"lastSuccessAt"`
	Snapshot      *InstanceSnapshot `json:"snapshot"`
}

// remoteInstance is a remote entry of /api/instances; the snapshot is the
// remote JSON as received.
type remoteInstance struct {
	ID            string          `json:"id"`
	Source        string          `json:"source"`
	APIURL        string          `json:"apiUrl"`
	LastAttemptAt string          `json:"lastAttemptAt,omitempty"`
	Snapshot      json.RawMessage `json:"snapshot,omitempty"`
	LastSuccessAt string          `json:"lastSuccessAt,omitempty"`
	LastError     string          `json:"lastError,omitempty"`
}

// aggregatedSnapshot is /api/instances (TS AggregatedStatusSnapshot).
type aggregatedSnapshot struct {
	CapturedAt string `json:"capturedAt"`
	Instances  []any  `json:"instances"`
}

// Instances returns /api/instances: the local instance first (if stored),
// then the remotes sorted by id.
func (a *Aggregator) Instances(now time.Time) any {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := aggregatedSnapshot{CapturedAt: formatTime(now), Instances: []any{}}
	if a.local != nil {
		out.Instances = append(out.Instances, localInstance{ID: a.local.InstanceID, Source: "local", LastSuccessAt: a.local.CapturedAt, Snapshot: a.local})
	}
	for _, r := range a.sortedRemotes() {
		out.Instances = append(out.Instances, remoteInstance{
			ID: r.id, Source: "remote", APIURL: r.apiURL, LastAttemptAt: r.lastAttemptAt,
			Snapshot: r.raw, LastSuccessAt: r.lastSuccessAt, LastError: r.lastError,
		})
	}
	return out
}

// Collect fetches the snapshot of remote r once (03 §7): GET apiUrl with
// Accept: application/json, timeoutMs covering the request and the body.
// A valid snapshot replaces the stored one and clears lastError; any
// failure only sets lastError (snapshot and lastSuccessAt are kept); a
// failure caused by the cancellation of ctx is neither recorded nor logged.
// It never fails. client nil means http.DefaultClient, log nil discards.
func (a *Aggregator) Collect(ctx context.Context, r config.RemoteStatusInstanceConfig, client *http.Client, log *slog.Logger, now func() time.Time) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	a.mu.Lock()
	cur, ok := a.remotes[r.ID]
	if !ok {
		cur = &remoteState{id: r.ID, apiURL: r.APIURL}
		a.remotes[r.ID] = cur
	}
	cur.lastAttemptAt = formatTime(now())
	a.mu.Unlock()

	raw, parsed, err := fetchRemote(ctx, r, cmp.Or(client, http.DefaultClient))
	if err != nil && ctx.Err() != nil {
		// Cut short by shutdown: not a failure of the remote (final review M4).
		return
	}

	a.mu.Lock()
	prevErr := cur.lastError
	if err != nil {
		cur.lastError = err.Error()
	} else {
		cur.raw, cur.parsed = raw, &parsed
		cur.lastSuccessAt = formatTime(now())
		cur.lastError = ""
	}
	a.mu.Unlock()

	apiURL := inputsrc.RedactURL(r.APIURL)
	if err != nil {
		level := slog.LevelWarn
		if err.Error() == prevErr {
			level = slog.LevelDebug // the same failure as the last collection
		}
		log.LogAttrs(ctx, level, "Remote instance status collection failed; keeping the last successful snapshot.",
			slog.String("event", "remote_status_collection_failed"), slog.String("remoteInstanceId", r.ID),
			slog.String("apiUrl", apiURL), slog.String("error", err.Error()))
		return
	}
	if prevErr != "" {
		log.LogAttrs(ctx, slog.LevelInfo, "Remote instance status collection recovered.",
			slog.String("event", "remote_status_recovered"), slog.String("remoteInstanceId", r.ID), slog.String("apiUrl", apiURL))
	}
	log.LogAttrs(ctx, slog.LevelDebug, "Remote instance status collected.",
		slog.String("event", "remote_status_collected"), slog.String("remoteInstanceId", r.ID), slog.String("apiUrl", apiURL))
}

// fetchRemote performs one remote request and validates the response. Its
// errors name the URL only as inputsrc.RedactURL and have no local address
// (neterr.Stable): they reach the edge-triggered log and lastError of
// /api/instances.
func fetchRemote(ctx context.Context, r config.RemoteStatusInstanceConfig, client *http.Client) (json.RawMessage, InstanceSnapshot, error) {
	raw, parsed, err := fetchRemoteRaw(ctx, r, client)
	if err == nil {
		return raw, parsed, nil
	}
	// net/http quotes the URL with its password masked, so RedactInputIn
	// alone would miss it.
	if ue, ok := errors.AsType[*url.Error](err); ok {
		ue.URL = inputsrc.RedactURL(r.APIURL)
	}
	err = neterr.Stable(err)
	if msg := inputsrc.RedactInputIn(err.Error(), r.APIURL); msg != err.Error() {
		err = errors.New(msg)
	}
	return nil, InstanceSnapshot{}, err
}

// fetchRemoteRaw is fetchRemote without the redaction.
func fetchRemoteRaw(ctx context.Context, r config.RemoteStatusInstanceConfig, client *http.Client) (json.RawMessage, InstanceSnapshot, error) {
	if r.TimeoutMs > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(r.TimeoutMs)*time.Millisecond)
		defer cancel()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.APIURL, http.NoBody)
	if err != nil {
		return nil, InstanceSnapshot{}, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, InstanceSnapshot{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, InstanceSnapshot{}, fmt.Errorf("Remote status API returned HTTP %d.", resp.StatusCode) //nolint:revive,staticcheck // contract text (03 §7)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, InstanceSnapshot{}, err
	}
	parsed, err := parseRemoteSnapshot(body)
	if err != nil {
		return nil, InstanceSnapshot{}, err
	}
	return json.RawMessage(body), parsed, nil
}

// validTunnelStates are the tunnel states a remote snapshot may report.
var validTunnelStates = map[string]bool{"healthy": true, "degraded": true, "repairing": true, "idle": true}

// parseRemoteSnapshot validates a remote InstanceSnapshot shallowly, as TS
// does (03 §7): version 1, string instanceId/capturedAt, the subscription →
// target → tunnel structure and the identifying fields and state of every
// tunnel. Other fields are not checked: values of an unexpected type are
// left out of the typed snapshot (the raw JSON keeps them).
func parseRemoteSnapshot(data []byte) (InstanceSnapshot, error) {
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		return InstanceSnapshot{}, err
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return InstanceSnapshot{}, errNotObject
	}
	if !validSnapshot(obj) {
		return InstanceSnapshot{}, errUnsupportedSchema
	}
	// Values that do not fit the typed snapshot are dropped from it only.
	pruned, err := json.Marshal(prune(obj, reflect.TypeFor[InstanceSnapshot]()))
	if err != nil {
		return InstanceSnapshot{}, err
	}
	var s InstanceSnapshot
	if err := json.Unmarshal(pruned, &s); err != nil {
		return InstanceSnapshot{}, err
	}
	return s, nil
}

// prune removes from the decoded JSON v every object member whose value
// does not fit the corresponding field of t (recursively), so that a typed
// decode cannot fail or half-fill a field.
func prune(v any, t reflect.Type) any {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.Struct:
		m := v.(map[string]any)
		out := make(map[string]any, len(m))
		for f := range t.Fields() {
			name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
			if val, ok := m[name]; ok && name != "" && fits(val, f.Type) {
				out[name] = prune(val, f.Type)
			}
		}
		return out
	case reflect.Slice:
		list := v.([]any)
		out := make([]any, 0, len(list))
		for _, e := range list {
			if fits(e, t.Elem()) {
				out = append(out, prune(e, t.Elem()))
			}
		}
		return out
	default:
		return v
	}
}

// fits reports whether the decoded JSON value v can be decoded into t.
func fits(v any, t reflect.Type) bool {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.String:
		_, ok := v.(string)
		return ok
	case reflect.Bool:
		_, ok := v.(bool)
		return ok
	case reflect.Int, reflect.Int64:
		f, ok := v.(float64)
		return ok && f == math.Trunc(f) && math.Abs(f) <= 1<<53
	case reflect.Float64:
		_, ok := v.(float64)
		return ok
	case reflect.Struct:
		_, ok := v.(map[string]any)
		return ok
	case reflect.Slice:
		_, ok := v.([]any)
		return ok
	default:
		return false
	}
}

func validSnapshot(obj map[string]any) bool {
	if version, ok := obj["version"].(float64); !ok || version != SnapshotVersion {
		return false
	}
	if !isString(obj["instanceId"]) || !isString(obj["capturedAt"]) {
		return false
	}
	subs, ok := obj["subscriptions"].([]any)
	if !ok {
		return false
	}
	tunnels, ok := obj["tunnels"].([]any)
	if !ok {
		return false
	}
	for _, s := range subs {
		sm, ok := s.(map[string]any)
		if !ok || !isString(sm["subscriptionId"]) {
			return false
		}
		targets, ok := sm["targets"].([]any)
		if !ok {
			return false
		}
		for _, t := range targets {
			tm, ok := t.(map[string]any)
			if !ok || !isString(tm["targetAddress"]) {
				return false
			}
			list, ok := tm["tunnels"].([]any)
			if !ok || slices.ContainsFunc(list, invalidTunnel) {
				return false
			}
		}
	}
	return !slices.ContainsFunc(tunnels, invalidTunnel)
}

func isString(v any) bool {
	_, ok := v.(string)
	return ok
}

func invalidTunnel(v any) bool {
	m, ok := v.(map[string]any)
	if !ok {
		return true
	}
	for _, k := range []string{"subscriptionId", "targetAddress", "displayName", "endpoint"} {
		if !isString(m[k]) {
			return true
		}
	}
	st, ok := m["state"].(string)
	return !ok || !validTunnelStates[st]
}

// marshalJSON is the status API JSON encoding (two-space indent, no HTML
// escaping, no trailing newline).
func marshalJSON(v any) ([]byte, error) { return jsonout.Marshal(v) }
