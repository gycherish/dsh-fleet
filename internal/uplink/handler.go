// Package uplink accepts node connections and multiplexes requests over them.
//
// It parses exactly the envelope fields listed in docs/envelope.md and nothing
// else: request and response bodies pass through as opaque bytes, and
// telemetry snapshots are stored verbatim. That restraint is what makes this
// control plane independent of the DeepSeek Harness version its nodes run.
package uplink

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/coder/websocket"

	"github.com/gycherish/dsh-fleet/pkg/envelope"
)

// authDeadline bounds how long a socket may stay anonymous. A connection that
// has not presented `hello` by then is closed, so an unauthenticated peer
// cannot hold resources open.
const authDeadline = envelope.AuthDeadlineSeconds * time.Second

// Authenticator verifies a node's presented token and records what it reports.
type Authenticator interface {
	Authenticate(ctx context.Context, nodeID, token string) error
	RecordHello(ctx context.Context, nodeID string, d envelope.NodeDescriptor, caps []string) error
}

// Handler serves the node uplink endpoint.
type Handler struct {
	Registry *Registry
	Auth     Authenticator
	Sink     TelemetrySink
	Log      *slog.Logger
	// NotFound and Revoked are reported with distinct close codes so an
	// operator reading a node's log can tell a wrong token from a deleted node.
	NotFound error
	Revoked  error
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		Subprotocols: []string{envelope.Subprotocol},
	})
	if err != nil {
		h.Log.Warn("uplink: handshake rejected", "err", err)
		return
	}
	ws.SetReadLimit(readLimit)

	// From here on every failure closes the socket with a protocol close code
	// rather than an HTTP status: the upgrade already succeeded.
	ctx := r.Context()
	hello, err := readHello(ctx, ws)
	if err != nil {
		h.Log.Warn("uplink: handshake failed", "err", err)
		_ = ws.Close(websocket.StatusCode(envelope.CloseAuthTimeout), "no hello")
		return
	}

	if hello.Protocol != envelope.ProtocolVersion {
		h.Log.Warn("uplink: protocol mismatch", "node", hello.NodeID, "theirs", hello.Protocol, "ours", envelope.ProtocolVersion)
		_ = ws.Close(websocket.StatusCode(envelope.CloseBadProtocol), "unsupported protocol version")
		return
	}

	authCtx, cancelAuth := context.WithTimeout(ctx, 10*time.Second)
	err = h.Auth.Authenticate(authCtx, hello.NodeID, hello.Token)
	cancelAuth()
	switch {
	case err == nil:
	case h.Revoked != nil && errors.Is(err, h.Revoked):
		_ = ws.Close(websocket.StatusCode(envelope.CloseBadToken), "token revoked")
		return
	case h.NotFound != nil && errors.Is(err, h.NotFound):
		// A wrong token and an unknown id deliberately answer the same way to a
		// caller that is guessing; the log below keeps the distinction for us.
		h.Log.Warn("uplink: rejected", "node", hello.NodeID, "err", err)
		_ = ws.Close(websocket.StatusCode(envelope.CloseUnknownNode), "unknown node or bad token")
		return
	default:
		h.Log.Error("uplink: cannot authenticate", "node", hello.NodeID, "err", err)
		_ = ws.Close(websocket.StatusInternalError, "authentication failed")
		return
	}

	conn := newConn(ws, hello.NodeID, hello.Caps, h.Sink, h.Log)
	if err := h.Registry.Add(conn); err != nil {
		h.Log.Warn("uplink: duplicate connection refused", "node", hello.NodeID)
		_ = ws.Close(websocket.StatusCode(envelope.CloseDuplicate), "node already connected")
		return
	}
	defer h.Registry.Remove(conn)

	recordCtx, cancelRecord := context.WithTimeout(ctx, 10*time.Second)
	if err := h.Auth.RecordHello(recordCtx, hello.NodeID, hello.Node, hello.Caps); err != nil {
		// Not fatal: the node is authenticated and usable even if we could not
		// persist what it told us about itself.
		h.Log.Warn("uplink: cannot record hello", "node", hello.NodeID, "err", err)
	}
	cancelRecord()

	welcome := envelope.Welcome{
		T:                   envelope.TWelcome,
		Protocol:            envelope.ProtocolVersion,
		HeartbeatMs:         int(heartbeatInterval / time.Millisecond),
		MaxChunkBytes:       maxChunkBytes,
		TelemetryIntervalMs: int(telemetryInterval / time.Millisecond),
	}
	if err := conn.write(ctx, welcome); err != nil {
		h.Log.Warn("uplink: cannot send welcome", "node", hello.NodeID, "err", err)
		return
	}

	h.Log.Info("node connected",
		"node", hello.NodeID,
		"dsh", hello.Node.DSHVersion,
		"platform", hello.Node.Platform,
		"caps", hello.Caps,
	)

	err = conn.serve(ctx)
	h.Log.Info("node disconnected", "node", hello.NodeID, "reason", closeReason(err))
	_ = ws.CloseNow()
}

// readHello reads and validates the first frame under the auth deadline.
func readHello(ctx context.Context, ws *websocket.Conn) (*envelope.Hello, error) {
	ctx, cancel := context.WithTimeout(ctx, authDeadline)
	defer cancel()

	_, raw, err := ws.Read(ctx)
	if err != nil {
		return nil, err
	}
	kind, err := envelope.Discriminant(raw)
	if err != nil {
		return nil, err
	}
	if kind != envelope.THello {
		return nil, errors.New("uplink: first frame must be hello")
	}
	var hello envelope.Hello
	if err := json.Unmarshal(raw, &hello); err != nil {
		return nil, err
	}
	if hello.NodeID == "" || hello.Token == "" {
		return nil, errors.New("uplink: hello must carry nodeId and token")
	}
	return &hello, nil
}

func closeReason(err error) string {
	if err == nil {
		return "clean"
	}
	// The one code a node sends us. Naming it keeps a routine reconnect from
	// reading like a fault in the log.
	if websocket.CloseStatus(err) == envelope.CloseHeartbeatLost {
		return "node lost the heartbeat; it will reconnect"
	}
	return err.Error()
}
