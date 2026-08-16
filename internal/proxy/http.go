// Package proxy forwards browser requests to a node over its uplink.
//
// It is deliberately thin: it moves bytes, correlates ids, and applies the one
// policy this project owns. It never decodes a dsh request or response body.
package proxy

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/gycherish/dsh-fleet/internal/uplink"
	"github.com/gycherish/dsh-fleet/pkg/envelope"
)

// privileged names the dsh methods that dsh's own browser carrier pins to
// loopback.
//
// A custom carrier does not pass through that carrier's `/api` route, so the
// pin is simply absent here and this project owns the boundary instead.
// Reading a preset is reconnaissance about which plugins a session runs;
// openPath and pickDirectory drive the node's desktop; the configuration plane
// exposes and mutates credentials. None of that should be reachable from a
// phone by default.
var privileged = map[string]struct{}{
	"host.pickDirectory":       {},
	"host.openPath":            {},
	"settings.describe":        {},
	"settings.openDocument":    {},
	"settings.update":          {},
	"settings.replace":         {},
	"settings.mutate":          {},
	"credentials.describe":     {},
	"credentials.set":          {},
	"credentials.unset":        {},
	"agentPreset.read":         {},
	"agentPreset.copy":         {},
	"agentPreset.openDocument": {},
	"agentPreset.remove":       {},
}

// Auditor records what the gate decided. Bodies are never passed to it: they
// are opaque by design and may carry prompts, file contents, or secrets.
type Auditor interface {
	RecordDecision(nodeID, ns, path string, allowed bool, status int, reason string)
}

// Handler serves /n/{nodeId}/... by forwarding to that node.
type Handler struct {
	Registry *uplink.Registry
	Log      *slog.Logger
	Audit    Auditor
	// AllowPrivileged opens the method set above. It exists so a single-user
	// deployment can opt in deliberately; it must never default to true.
	AllowPrivileged bool
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	nodeID := r.PathValue("node")
	rest := "/" + strings.TrimPrefix(r.PathValue("rest"), "/")
	if r.URL.RawQuery != "" {
		rest += "?" + r.URL.RawQuery
	}

	conn, online := h.Registry.Get(nodeID)
	if !online {
		http.Error(w, "node is not connected", http.StatusBadGateway)
		return
	}

	ns, method := classify(rest)
	if ns == envelope.NsDSH && !h.AllowPrivileged {
		if _, blocked := privileged[method]; blocked {
			h.record(nodeID, ns, method, false, http.StatusForbidden, "privileged method blocked by control-plane policy")
			http.Error(w, "method is not available over the fleet carrier", http.StatusForbidden)
			return
		}
	}

	// A body cap belongs here rather than at the node: an oversized upload
	// should never reach the uplink at all.
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 32<<20))
	if err != nil {
		http.Error(w, "request body too large or unreadable", http.StatusRequestEntityTooLarge)
		return
	}

	headers := map[string]string{}
	if ct := r.Header.Get("content-type"); ct != "" {
		headers["content-type"] = ct
	}
	if acc := r.Header.Get("accept"); acc != "" {
		headers["accept"] = acc
	}

	textual := strings.HasPrefix(r.Header.Get("content-type"), "application/json")
	resp, err := conn.Do(r.Context(), ns, r.Method, rest, headers, body, textual)
	if err != nil {
		h.fail(w, nodeID, ns, method, err)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	for key, value := range resp.Headers {
		// Hop-by-hop framing is this server's to decide, not the node's.
		switch strings.ToLower(key) {
		case "content-length", "transfer-encoding", "connection":
			continue
		}
		w.Header().Set(key, value)
	}
	w.WriteHeader(resp.Status)
	h.record(nodeID, ns, method, true, resp.Status, "")

	// Flush per chunk: events.mux is an SSE stream, and buffering it would
	// hold every assistant token until the response ended.
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if _, writeErr := w.Write(buf[:n]); writeErr != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) {
				h.Log.Warn("proxy: response stream ended early", "node", nodeID, "path", rest, "err", readErr)
			}
			return
		}
	}
}

// classify splits a forwarded path into its namespace and method name.
func classify(path string) (ns, method string) {
	clean := strings.SplitN(strings.TrimPrefix(path, "/"), "?", 2)[0]
	if rest, ok := strings.CutPrefix(clean, "api/"); ok {
		return envelope.NsDSH, rest
	}
	if rest, ok := strings.CutPrefix(clean, "fleet/"); ok {
		return envelope.NsFleet, rest
	}
	return envelope.NsDSH, clean
}

func (h *Handler) fail(w http.ResponseWriter, nodeID, ns, method string, err error) {
	var nodeErr *uplink.RequestError
	if errors.As(err, &nodeErr) {
		status := map[string]int{
			envelope.ErrUnsupported: http.StatusNotImplemented,
			envelope.ErrUnavailable: http.StatusServiceUnavailable,
			envelope.ErrDenied:      http.StatusForbidden,
			envelope.ErrCancelled:   499, // client closed request
		}[nodeErr.Code]
		if status == 0 {
			status = http.StatusBadGateway
		}
		h.record(nodeID, ns, method, true, status, nodeErr.Code)
		http.Error(w, nodeErr.Message, status)
		return
	}
	h.record(nodeID, ns, method, true, http.StatusBadGateway, err.Error())
	http.Error(w, "node request failed", http.StatusBadGateway)
}

func (h *Handler) record(nodeID, ns, method string, allowed bool, status int, reason string) {
	if h.Audit != nil {
		h.Audit.RecordDecision(nodeID, ns, method, allowed, status, reason)
	}
}
