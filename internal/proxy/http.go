// Package proxy forwards browser requests to a node over its uplink.
//
// It is deliberately thin: it moves bytes, correlates ids, and applies the one
// policy this project owns. It never decodes a dsh request or response body.
package proxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/coder/websocket"

	"github.com/gycherish/dsh-fleet/internal/uplink"
	"github.com/gycherish/dsh-fleet/pkg/envelope"
)

// Access is how much of dsh's loopback-pinned method set this control plane
// forwards.
//
// dsh pins that set inside its own browser carrier, which a custom carrier
// does not pass through — so the boundary is this project's to draw.
//
// It is drawn at AccessFull, because the point of this project is to drive
// your own machine from your phone, and a console where half the buttons
// answer 403 is not that. Withholding the writes read as a safety measure but
// was not one: the console already requires a login, the uplink is
// authenticated per machine, and anyone through both of those can run shell
// commands via an ordinary session. Refusing `settings.update` while allowing
// `session.prompt` protects nothing and breaks the Agent presets page.
//
// The narrower levels stay for anyone who wants a console they can hand out
// as read-only.
type Access string

const (
	// AccessNone refuses the whole pinned set. The settings pages will not
	// load at all.
	AccessNone Access = "none"
	// AccessRead forwards the reads and refuses the writes. A console for
	// looking, not touching — some pages will show a transport failure where
	// they expected to save.
	AccessRead Access = "read"
	// AccessFull forwards everything. The default.
	AccessFull Access = "full"
)

// ParseAccess reads a configured level, defaulting to AccessFull.
func ParseAccess(value string) (Access, error) {
	switch Access(strings.ToLower(strings.TrimSpace(value))) {
	case "":
		return AccessFull, nil
	case AccessNone:
		return AccessNone, nil
	case AccessRead:
		return AccessRead, nil
	case AccessFull:
		return AccessFull, nil
	default:
		return "", fmt.Errorf("proxy: privileged access must be none, read or full, got %q", value)
	}
}

// privilegedRead names pinned methods that only report state.
//
// None of them returns a secret: dsh keeps secret-role values out of every
// `settings.describe` layer, and `credentials.describe` reports only whether a
// reference is configured, where it comes from, and whether it is writable.
// They do disclose how a machine is set up, which is why AccessNone exists.
var privilegedRead = map[string]struct{}{
	"settings.describe":    {},
	"credentials.describe": {},
	"agentPreset.read":     {},
}

// privilegedWrite names pinned methods that change something or act on the
// node's own desktop.
//
// `llm.discoverModels` is here despite writing nothing: the caller supplies an
// API key in its payload, so allowing it means allowing a secret to be sent
// from wherever the browser is.
var privilegedWrite = map[string]struct{}{
	"host.pickDirectory":       {},
	"host.openPath":            {},
	"settings.openDocument":    {},
	"settings.update":          {},
	"settings.replace":         {},
	"settings.mutate":          {},
	"credentials.set":          {},
	"credentials.unset":        {},
	"agentPreset.copy":         {},
	"agentPreset.openDocument": {},
	"agentPreset.remove":       {},
	"llm.discoverModels":       {},
}

// refuse reports whether this level withholds a method, and why.
func (a Access) refuse(method string) (bool, string) {
	if a == AccessFull {
		return false, ""
	}
	if _, ok := privilegedWrite[method]; ok {
		return true, "this console is read-only (unset DSHF_PRIVILEGED_ACCESS, or set it to full, to allow changes)"
	}
	if _, ok := privilegedRead[method]; ok && a == AccessNone {
		return true, "this console cannot read machine settings (DSHF_PRIVILEGED_ACCESS=read or full to allow)"
	}
	return false, ""
}

// Auditor records what the gate decided. Bodies are never passed to it: they
// are opaque by design and may carry prompts, file contents, or secrets.
type Auditor interface {
	RecordDecision(userID, nodeID, ns, path string, allowed bool, status int, reason string)
}

// Handler forwards a browser request to the node this browser selected.
//
// It serves the whole origin below the control plane's own reserved prefix,
// because the dsh web client addresses `/api/...` and its assets as absolute
// paths. Hosting a node under a path prefix would break every one of those
// requests, so the node's application gets the root and the console moves out
// of its way.
type Handler struct {
	Registry *uplink.Registry
	Log      *slog.Logger
	Audit    Auditor
	// SelectNode resolves which node this request belongs to.
	SelectNode func(*http.Request) string
	// SelectUser names the account behind a request, for the audit trail.
	//
	// A hook rather than a direct read of the session, so this package stays
	// unaware of how anyone signs in. Nil records decisions with no actor,
	// which is what the schema's nullable column already allowed for — and
	// what every row said before this existed.
	SelectUser func(*http.Request) string
	// NoSelection handles a request that named no node, typically by sending
	// the browser to the chooser.
	NoSelection http.Handler
	// Unreachable handles a request for a machine that is not connected.
	//
	// It gets its own seam because the bare 502 this used to write was a dead
	// end: the machine owns the origin root, so an error page served in its
	// place carries none of the console's chrome, and someone who opened an
	// offline machine on a phone had no way back at all. Nil keeps the bare
	// status, which is right for a client that is not a browser.
	Unreachable http.Handler
	// Privileged is how much of the pinned set to forward. The zero value is
	// treated as AccessFull, so a Handler nobody configured is a working one.
	Privileged Access
	// Inject is spliced into every HTML page the machine serves. Empty leaves
	// the machine's pages exactly as it wrote them.
	Inject string
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	nodeID := h.SelectNode(r)
	if nodeID == "" {
		h.NoSelection.ServeHTTP(w, r)
		return
	}
	rest := r.URL.EscapedPath()
	if rest == "" {
		rest = "/"
	}
	if r.URL.RawQuery != "" {
		rest += "?" + r.URL.RawQuery
	}

	// The gate runs before the connection lookup on purpose. A refusal must not
	// depend on whether the machine happens to be up: otherwise the response
	// leaks liveness for methods the caller may not use, and — worse — a denied
	// attempt against an offline machine would leave no audit record at all.
	ns, method := classify(rest)
	if ns == envelope.NsDSH {
		level := h.Privileged
		if level == "" {
			level = AccessFull
		}
		if blocked, why := level.refuse(method); blocked {
			h.record(r, nodeID, ns, method, false, http.StatusForbidden, why)
			http.Error(w, why, http.StatusForbidden)
			return
		}
	}

	conn, online := h.Registry.Get(nodeID)
	if !online {
		h.record(r, nodeID, ns, method, true, http.StatusBadGateway, "machine is not connected")
		if h.Unreachable != nil {
			h.Unreachable.ServeHTTP(w, r)
			return
		}
		http.Error(w, "node is not connected", http.StatusBadGateway)
		return
	}

	// The event downlinks are upgrades, not ordinary requests. Falling through
	// to the request path would answer 426 and leave a UI that renders once and
	// never updates.
	if isUpgrade(r) {
		h.bridge(w, r, conn, nodeID, ns, method, rest)
		return
	}

	// A body cap belongs here rather than at the node: an oversized upload
	// should never reach the uplink at all.
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 32<<20))
	if err != nil {
		http.Error(w, "request body too large or unreadable", http.StatusRequestEntityTooLarge)
		return
	}

	headers := map[string]string{}
	// Only the headers the node's own server needs to answer correctly. The
	// browser's cookies and authorization deliberately stay here: they belong
	// to this control plane, and a node has no business seeing them.
	for _, name := range []string{"content-type", "accept", "accept-language", "if-none-match", "range"} {
		if value := r.Header.Get(name); value != "" {
			headers[name] = value
		}
	}

	textual := strings.HasPrefix(r.Header.Get("content-type"), "application/json")
	resp, err := conn.Do(r.Context(), ns, r.Method, rest, headers, body, textual)
	if err != nil {
		h.fail(w, r, nodeID, ns, method, err)
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
	// A page from the machine is the only place the console can put its own
	// chrome, since the machine owns the origin root. Documents are small and
	// arrive in one piece, so buffering one to splice a script tag in costs
	// nothing a reader would notice.
	if h.Inject != "" && isDocument(resp) {
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxDocumentBytes))
		if err == nil {
			w.WriteHeader(resp.Status)
			h.record(r, nodeID, ns, method, true, resp.Status, "")
			_, _ = w.Write(splice(body, h.Inject))
			return
		}
		// Fall through: a document we could not read whole is still better
		// delivered without the overlay than not delivered at all.
		h.Log.Warn("proxy: cannot buffer document for injection", "node", nodeID, "path", rest, "err", err)
	}

	w.WriteHeader(resp.Status)
	h.record(r, nodeID, ns, method, true, resp.Status, "")

	// Flush per chunk. The event downlinks upgrade rather than stream through
	// here, but a large `session.export` and any future streaming response
	// still must not be buffered whole before the browser sees a byte.
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

// maxDocumentBytes caps what may be buffered for injection. An application
// shell is a few kilobytes; anything approaching this is not a page.
const maxDocumentBytes = 8 << 20

// isDocument reports an HTML page — the app shell, not an API answer that
// happens to be text. A 304 carries no body to splice.
func isDocument(resp *uplink.Response) bool {
	if resp.Status != http.StatusOK {
		return false
	}
	for key, value := range resp.Headers {
		if strings.EqualFold(key, "content-type") {
			return strings.HasPrefix(strings.ToLower(strings.TrimSpace(value)), "text/html")
		}
	}
	return false
}

// splice inserts tag before the document's closing body tag.
//
// Before </body> rather than in <head> so the script never delays first paint,
// and so document.body exists when it runs. A document without the tag still
// gets the script: browsers are forgiving about it, and an unclosed body is
// not a reason to strand someone with no way back to the chooser.
func splice(document []byte, tag string) []byte {
	marker := []byte("</body>")
	at := bytes.LastIndex(bytes.ToLower(document), marker)
	if at < 0 {
		return append(document, tag...)
	}
	out := make([]byte, 0, len(document)+len(tag))
	out = append(out, document[:at]...)
	out = append(out, tag...)
	return append(out, document[at:]...)
}

// isUpgrade reports a WebSocket handshake. Both header values are
// case-insensitive, and Connection may list several tokens.
func isUpgrade(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket") &&
		strings.Contains(strings.ToLower(r.Header.Get("Connection")), "upgrade")
}

// bridge relays one browser socket to the node's own server.
//
// Two hops, because neither end can reach the other directly: the browser
// talks only to this control plane, and the node accepts no inbound
// connections at all.
func (h *Handler) bridge(w http.ResponseWriter, r *http.Request, conn *uplink.Conn, nodeID, ns, method, path string) {
	remote, err := conn.OpenWS(r.Context(), path, nil)
	if err != nil {
		h.record(r, nodeID, ns, method, true, http.StatusBadGateway, "socket bridge failed")
		h.Log.Warn("proxy: cannot bridge socket", "node", nodeID, "path", path, "err", err)
		http.Error(w, "cannot reach the machine's event stream", http.StatusBadGateway)
		return
	}
	defer remote.Close(int(websocket.StatusNormalClosure), "browser gone")

	// Accepted only after the far end is up, so a failure is still an HTTP
	// status the browser can read rather than an immediate close.
	client, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// Same-origin is enforced by the console's session cookie; the browser
		// is talking to the origin it loaded from.
		OriginPatterns: []string{"*"},
	})
	if err != nil {
		h.Log.Warn("proxy: browser upgrade rejected", "node", nodeID, "err", err)
		return
	}
	defer func() { _ = client.CloseNow() }()
	h.record(r, nodeID, ns, method, true, http.StatusSwitchingProtocols, "")

	ctx, cancel := context.WithCancel(context.WithoutCancel(r.Context()))
	defer cancel()

	// Node → browser.
	go func() {
		defer cancel()
		for text := range remote.Messages {
			if err := client.Write(ctx, websocket.MessageText, []byte(text)); err != nil {
				return
			}
		}
	}()

	// Browser → node. These downlinks are declared downlink-only, but relaying
	// both ways costs one loop and keeps the bridge honest for any later
	// endpoint that talks back.
	for {
		kind, data, err := client.Read(ctx)
		if err != nil {
			return
		}
		if kind != websocket.MessageText {
			continue
		}
		if err := remote.Send(ctx, string(data)); err != nil {
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

func (h *Handler) fail(w http.ResponseWriter, r *http.Request, nodeID, ns, method string, err error) {
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
		h.record(r, nodeID, ns, method, true, status, nodeErr.Code)
		http.Error(w, nodeErr.Message, status)
		return
	}
	h.record(r, nodeID, ns, method, true, http.StatusBadGateway, err.Error())
	http.Error(w, "node request failed", http.StatusBadGateway)
}

func (h *Handler) record(r *http.Request, nodeID, ns, method string, allowed bool, status int, reason string) {
	if h.Audit == nil {
		return
	}
	// Without the actor the trail answers "what crossed the boundary" but not
	// "who pushed it across", which is most of what anyone reads an audit log
	// for. The column was always there; nothing filled it until now.
	userID := ""
	if h.SelectUser != nil {
		userID = h.SelectUser(r)
	}
	h.Audit.RecordDecision(userID, nodeID, ns, method, allowed, status, reason)
}
