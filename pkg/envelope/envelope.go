// Package envelope is the Go mirror of the dsh-fleet uplink wire contract.
//
// docs/envelope.md is authoritative; this file follows it, and so does
// dsh/src/protocol.ts. When they disagree, the document wins.
//
// The load-bearing property of these types is what they DO NOT contain: no dsh
// business payload is ever parsed here. Request and response bodies are opaque
// [Payload] values, and telemetry snapshots are [json.RawMessage]. Keeping it
// that way is what makes the control plane independent of the DeepSeek Harness
// version its nodes run.
package envelope

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"unicode/utf8"
)

// ProtocolVersion is the wire version this build speaks.
const ProtocolVersion = 1

// Subprotocol is the WebSocket subprotocol required on the uplink handshake.
const Subprotocol = "dshf.v1"

// AuthDeadlineSeconds bounds how long a socket may stay anonymous before it
// must present a hello frame.
const AuthDeadlineSeconds = 5

// Close codes the control plane uses to refuse or end an uplink.
const (
	CloseBadToken    = 4001 // bad or revoked node token
	CloseUnknownNode = 4002 // no node registered under the presented id
	CloseBadProtocol = 4003 // unsupported protocol version
	CloseDuplicate   = 4004 // another live connection holds this node id
	CloseAuthTimeout = 4005 // no hello within the authentication deadline

	// CloseHeartbeatLost is sent by the node, not this side: two heartbeats
	// went unanswered so it is reconnecting. Log it; do not hold it against
	// the node.
	CloseHeartbeatLost = 4006
)

// Frame discriminants.
const (
	THello     = "hello"
	TWelcome   = "welcome"
	TReq       = "req"
	TCancel    = "cancel"
	THead      = "head"
	TData      = "data"
	TEnd       = "end"
	TErr       = "err"
	TTelemetry = "tlm"
	TPing      = "ping"
	TPong      = "pong"

	TWsOpen  = "ws.open"
	TWsUp    = "ws.up"
	TWsMsg   = "ws.msg"
	TWsClose = "ws.close"
)

// Namespaces a request may address.
const (
	NsDSH   = "dsh"   // tunnelled HTTP to the node's /api fetch handler
	NsFleet = "fleet" // this project's own node methods
)

// Terminal failure classes for one request.
const (
	ErrCancelled   = "cancelled"
	ErrUnsupported = "unsupported"
	ErrUnavailable = "unavailable"
	ErrDenied      = "denied"
	ErrInternal    = "internal"
)

// Payload is an opaque body. Enc "u" carries UTF-8 text verbatim; "b" carries
// standard base64 of raw bytes.
type Payload struct {
	Enc string `json:"enc"`
	D   string `json:"d"`
}

// ErrBadEncoding reports a Payload whose Enc is neither "u" nor "b".
var ErrBadEncoding = errors.New("envelope: payload enc must be \"u\" or \"b\"")

// Bytes decodes a payload to raw bytes.
func (p Payload) Bytes() ([]byte, error) {
	switch p.Enc {
	case "u":
		return []byte(p.D), nil
	case "b":
		return base64.StdEncoding.DecodeString(p.D)
	default:
		return nil, fmt.Errorf("%w (got %q)", ErrBadEncoding, p.Enc)
	}
}

// NewPayload encodes bytes, preferring verbatim UTF-8 when textual is true and
// the bytes are valid UTF-8.
//
// The hot path is events.mux, which carries every assistant token; paying
// base64's inflation there would cost bandwidth on every chunk.
func NewPayload(b []byte, textual bool) Payload {
	if textual && utf8.Valid(b) {
		return Payload{Enc: "u", D: string(b)}
	}
	return Payload{Enc: "b", D: base64.StdEncoding.EncodeToString(b)}
}

// NodeDescriptor carries the identity and build facts a node presents at
// handshake.
type NodeDescriptor struct {
	Label         string `json:"label,omitempty"`
	Platform      string `json:"platform"`
	Arch          string `json:"arch"`
	DSHVersion    string `json:"dshVersion"`
	PluginVersion string `json:"pluginVersion"`
	Cwd           string `json:"cwd"`
}

// Hello is the first frame on every connection. It carries the node token
// in-band because the WHATWG WebSocket constructor cannot set request headers.
type Hello struct {
	T        string `json:"t"`
	Protocol int    `json:"protocol"`
	NodeID   string `json:"nodeId"`
	// Username names the account whose token this is, and selects the credential
	// kind: present means Token is that person's `ut_` token and the machine
	// enrols itself; absent means Token is this machine's own `nt_` token, minted
	// by `dshf node add`.
	//
	// Two kinds rather than one because they suit different situations. A
	// container is handed a machine token and never sees a UI; a person types
	// their own username and token into a plugin form and expects the machine to
	// appear without a second step on the control plane.
	Username string         `json:"username,omitempty"`
	Token    string         `json:"token"`
	Node     NodeDescriptor `json:"node"`
	Caps     []string       `json:"caps"`
}

// Welcome accepts a node and fixes the runtime parameters for the connection.
type Welcome struct {
	T                   string `json:"t"`
	Protocol            int    `json:"protocol"`
	HeartbeatMs         int    `json:"heartbeatMs"`
	MaxChunkBytes       int    `json:"maxChunkBytes"`
	TelemetryIntervalMs int    `json:"telemetryIntervalMs"`
}

// Req is one request for a node to serve.
//
// Path is the exact /api path including query string for NsDSH, or a method
// name for NsFleet. The control plane never inspects it beyond the namespace
// routing and its own privilege gate, which matches on the method name only.
type Req struct {
	T       string            `json:"t"`
	ID      string            `json:"id"`
	Ns      string            `json:"ns"`
	Method  string            `json:"method"`
	Path    string            `json:"path"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    *Payload          `json:"body,omitempty"`
}

// Cancel aborts one in-flight request. The node still answers with an Err
// frame carrying ErrCancelled, so the caller frees its slot on one code path.
type Cancel struct {
	T  string `json:"t"`
	ID string `json:"id"`
}

// Head is a response status line.
type Head struct {
	T       string            `json:"t"`
	ID      string            `json:"id"`
	Status  int               `json:"status"`
	Headers map[string]string `json:"headers"`
}

// Data is one response body chunk. Seq starts at 0 and increments per request;
// a receiver must treat a gap or repeat as fatal rather than reordering, since
// silently reordering an SSE stream corrupts the browser's event sequence in
// ways that only surface much later.
type Data struct {
	T    string  `json:"t"`
	ID   string  `json:"id"`
	Seq  int     `json:"seq"`
	Body Payload `json:"body"`
}

// End completes a response body. Chunks lets the receiver assert it saw them all.
type End struct {
	T      string `json:"t"`
	ID     string `json:"id"`
	Chunks int    `json:"chunks"`
}

// Err is a terminal failure. It may legally follow a Head when a stream fails
// mid-body.
type Err struct {
	T       string `json:"t"`
	ID      string `json:"id"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Telemetry is an unsolicited node snapshot. Snapshot stays raw: the control
// plane stores it verbatim as jsonb and must not require any particular field,
// so a newer node can add keys without a control-plane release.
type Telemetry struct {
	T        string          `json:"t"`
	Ts       int64           `json:"ts"`
	Snapshot json.RawMessage `json:"snapshot"`
}

// WsOpen asks the node to dial one WebSocket on its own server and bridge it.
//
// The event downlinks (/api/events.mux, /api/events.host) are upgrades, not
// SSE — a plain GET answers 426 with no fallback — and they carry every
// assistant token. Forwarding ordinary requests while dropping upgrades yields
// a UI that loads, renders, and then never updates.
type WsOpen struct {
	T       string            `json:"t"`
	ID      string            `json:"id"`
	Path    string            `json:"path"`
	Headers map[string]string `json:"headers,omitempty"`
}

// WsUp reports that a bridged socket connected.
type WsUp struct {
	T        string `json:"t"`
	ID       string `json:"id"`
	Protocol string `json:"protocol,omitempty"`
}

// WsMsg is one message in either direction on a bridged socket.
type WsMsg struct {
	T    string  `json:"t"`
	ID   string  `json:"id"`
	Body Payload `json:"body"`
}

// WsClose is either end closing a bridged socket.
type WsClose struct {
	T      string `json:"t"`
	ID     string `json:"id"`
	Code   int    `json:"code,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// Ping probes liveness; Pong echoes Ts.
type Ping struct {
	T  string `json:"t"`
	Ts int64  `json:"ts"`
}

// Pong answers a Ping.
type Pong struct {
	T  string `json:"t"`
	Ts int64  `json:"ts"`
}

// Discriminant reads the frame type from a raw frame without decoding the rest.
//
// This is the only inspection the router performs before dispatch, which keeps
// unknown frame types cheap to ignore.
func Discriminant(raw []byte) (string, error) {
	var probe struct {
		T string `json:"t"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return "", fmt.Errorf("envelope: unparseable frame: %w", err)
	}
	if probe.T == "" {
		return "", errors.New("envelope: frame has no discriminant")
	}
	return probe.T, nil
}
