package status

import (
	"cmp"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/a1ekseev/scd/internal/common/httpserve"
	"github.com/a1ekseev/scd/internal/scd/config"
	"github.com/a1ekseev/scd/internal/scd/state"
	"github.com/a1ekseev/scd/internal/scd/xrayapi"
)

// Status API paths (03 §6.3). Routing compares the URL path; the query is
// ignored (O8).
const (
	pathStatus         = "/api/status"
	pathInstanceStatus = "/api/instance-status"
	pathInstances      = "/api/instances"
	// pathRuntimeState is matched as a prefix, as in TS.
	pathRuntimeState = "/api/runtime-state"
)

const (
	contentTypeJSON = "application/json; charset=utf-8"
	contentTypeHTML = "text/html; charset=utf-8"

	msgRuntimeStateDisabled = "Runtime state endpoint is disabled."
	msgRuntimeStateParams   = "subscriptionId and targetAddress query parameters are required."
)

// fallbackInstanceID is the local instance id when neither
// statusServer.instanceId nor the hostname is available.
const fallbackInstanceID = "localhost"

// Deps are the dependencies of the status handler.
type Deps struct {
	Config config.Loaded
	Memory *state.Memory
	// Aggregator receives the local snapshot on every request; nil creates
	// one for the configured remotes.
	Aggregator *Aggregator
	// Dial connects to Xray for runtime-state; nil means xrayapi.Dial.
	Dial xrayapi.Dialer
	// Now and Hostname default to time.Now and os.Hostname.
	Now      func() time.Time
	Hostname func() (string, error)
	Log      *slog.Logger
}

// NewHandler returns the status server handler (03 §6.3): the JSON API on
// its paths (any method, query ignored for routing) and the dashboard on
// every other path. Every request rebuilds the local snapshot and stores it
// in the aggregator (03 §6.2). Panics become 500 responses (D23).
func NewHandler(d Deps) http.Handler {
	if d.Aggregator == nil {
		d.Aggregator = NewAggregator(d.Config.Config.StatusServer.RemoteInstances)
	}
	if d.Memory == nil {
		d.Memory = state.New()
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Hostname == nil {
		d.Hostname = os.Hostname
	}
	if d.Log == nil {
		d.Log = slog.New(slog.DiscardHandler)
	}
	return httpserve.Recover(d.Log, &handler{d: d})
}

type handler struct{ d Deps }

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	d := h.d
	cfg := d.Config.Config
	now := d.Now()
	local := BuildInstanceSnapshot(d.Memory, cfg, h.instanceID(), now)
	d.Aggregator.StoreLocal(local)

	switch path := r.URL.Path; {
	case path == pathStatus:
		writeJSON(w, http.StatusOK, struct {
			Tunnels []StatusTunnel `json:"tunnels"`
		}{local.Tunnels})
	case path == pathInstanceStatus:
		writeJSON(w, http.StatusOK, local)
	case path == pathInstances:
		writeJSON(w, http.StatusOK, d.Aggregator.Instances(now))
	case strings.HasPrefix(path, pathRuntimeState):
		h.runtimeState(w, r)
	default:
		// The dashboard (task 6.5) renders d.Aggregator.View() here.
		w.Header().Set("Content-Type", contentTypeHTML)
		w.WriteHeader(http.StatusOK)
	}
}

// instanceID is statusServer.instanceId, else the hostname.
func (h *handler) instanceID() string {
	if id := h.d.Config.Config.StatusServer.InstanceID; id != "" {
		return id
	}
	host, err := h.d.Hostname()
	if err != nil {
		return fallbackInstanceID
	}
	return cmp.Or(host, fallbackInstanceID)
}

// runtimeState serves /api/runtime-state (03 §6.3): disabled → 404,
// missing parameters → 400, then BuildRuntimeState.
func (h *handler) runtimeState(w http.ResponseWriter, r *http.Request) {
	if !h.d.Config.Config.StatusServer.RuntimeState.Enabled {
		writeJSON(w, http.StatusNotFound, errorBody{msgRuntimeStateDisabled})
		return
	}
	q := r.URL.Query()
	subID := strings.TrimSpace(q.Get("subscriptionId"))
	addr := strings.TrimSpace(q.Get("targetAddress"))
	if subID == "" || addr == "" {
		writeJSON(w, http.StatusBadRequest, errorBody{msgRuntimeStateParams})
		return
	}
	// The status server logs no requests (03 §6.1); errors are in the body.
	body, code, _ := BuildRuntimeState(r.Context(), h.d, subID, addr)
	writeJSON(w, code, body)
}

// writeJSON writes v as the status API JSON with code.
func writeJSON(w http.ResponseWriter, code int, v any) {
	b, err := marshalJSON(v)
	if err != nil {
		code = http.StatusInternalServerError
		b, _ = marshalJSON(errorBody{err.Error()})
	}
	w.Header().Set("Content-Type", contentTypeJSON)
	w.WriteHeader(code)
	_, _ = w.Write(b)
}
