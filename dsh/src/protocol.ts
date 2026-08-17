/**
 * The dsh-fleet uplink wire contract, TypeScript side. Derived from
 * `docs/envelope.md`, which is authoritative when the two disagree; the Go
 * mirror lives in `pkg/envelope`.
 *
 * Nothing here understands a dsh business payload. Request and response bodies
 * are carried as opaque {@link Payload} values so this plugin — and the control
 * plane behind it — stay independent of the dsh version they tunnel.
 *
 * @module @dsh-fleet/node/protocol
 */

/** Wire protocol version this build speaks. */
export const PROTOCOL_VERSION = 1

/** WebSocket subprotocol offered on the uplink handshake. */
export const SUBPROTOCOL = 'dshf.v1'

/** Close codes the control plane uses to refuse or end a connection. */
export const CloseCode = {
  /** Bad or revoked node token. */
  BAD_TOKEN: 4001,
  /** No node registered under the presented id. */
  UNKNOWN_NODE: 4002,
  /** The node's protocol version is not supported. */
  BAD_PROTOCOL: 4003,
  /** Another live connection already holds this node id. */
  DUPLICATE: 4004,
  /** The node did not authenticate within the control plane's deadline. */
  AUTH_TIMEOUT: 4005,
} as const

/** One close code value. */
export type CloseCodeValue = (typeof CloseCode)[keyof typeof CloseCode]

/**
 * An opaque body. `u` carries UTF-8 text verbatim; `b` carries standard base64
 * of raw bytes. The sender picks; no receiver branches on the choice beyond
 * decoding.
 */
export interface Payload {
  enc: 'u' | 'b'
  d: string
}

/** Namespaces a request may address. */
export type Namespace = 'dsh' | 'fleet'

/** Terminal failure classes for one request. */
export type ErrorCode = 'cancelled' | 'unsupported' | 'unavailable' | 'denied' | 'internal'

/** Identity and build facts a node presents at handshake. */
export interface NodeDescriptor {
  /** Operator-facing name; the control plane may override it for display. */
  label?: string | undefined
  /** `process.platform` of the node. */
  platform: string
  /** `process.arch` of the node. */
  arch: string
  /** Resolved DeepSeek Harness version, or `unknown` when it cannot be read. */
  dshVersion: string
  /** This plugin's version. */
  pluginVersion: string
  /** The harness launch directory. */
  cwd: string
}

/** First frame on every connection: identity, capabilities, and the node token. */
export interface HelloFrame {
  t: 'hello'
  protocol: number
  nodeId: string
  /**
   * Node token. Carried in-band rather than as an `Authorization` header
   * because the WHATWG `WebSocket` constructor cannot set request headers, and
   * a token in the URL would reach proxy access logs.
   */
  token: string
  node: NodeDescriptor
  /** Namespaces and optional `fleet` families this node serves. */
  caps: string[]
}

/** Acceptance plus the runtime parameters the node adopts for this connection. */
export interface WelcomeFrame {
  t: 'welcome'
  protocol: number
  heartbeatMs: number
  maxChunkBytes: number
  telemetryIntervalMs: number
}

/** One request from the control plane. */
export interface ReqFrame {
  t: 'req'
  id: string
  ns: Namespace
  method: string
  /** `/api/…` path including query string for `dsh`; a method name for `fleet`. */
  path: string
  headers?: Record<string, string> | undefined
  body?: Payload | undefined
}

/** Abort one in-flight request; the node still answers with `err`/`cancelled`. */
export interface CancelFrame {
  t: 'cancel'
  id: string
}

/** Response status line. */
export interface HeadFrame {
  t: 'head'
  id: string
  status: number
  headers: Record<string, string>
}

/** One response body chunk. `seq` starts at 0 and increments per request. */
export interface DataFrame {
  t: 'data'
  id: string
  seq: number
  body: Payload
}

/** Response body complete; `chunks` lets the receiver assert it saw them all. */
export interface EndFrame {
  t: 'end'
  id: string
  chunks: number
}

/** Terminal failure. May follow `head` when a stream fails mid-body. */
export interface ErrFrame {
  t: 'err'
  id: string
  code: ErrorCode
  message: string
}

/**
 * Ask the node to open a WebSocket to its own server and bridge it.
 *
 * `/api/events.mux` and `/api/events.host` are upgrades, not SSE — a plain GET
 * answers 426 with no fallback — and they carry every assistant token. A
 * carrier that forwards ordinary requests but drops upgrades produces a UI
 * that loads, renders, and then never updates.
 */
export interface WsOpenFrame {
  t: 'ws.open'
  id: string
  path: string
  headers?: Record<string, string> | undefined
}

/** The bridged socket is connected and will start relaying messages. */
export interface WsUpFrame {
  t: 'ws.up'
  id: string
  /** Subprotocol the node's server selected, if any. */
  protocol?: string | undefined
}

/** One message in either direction on a bridged socket. */
export interface WsMsgFrame {
  t: 'ws.msg'
  id: string
  body: Payload
}

/** Either end closing a bridged socket. */
export interface WsCloseFrame {
  t: 'ws.close'
  id: string
  code?: number | undefined
  reason?: string | undefined
}

/** Unsolicited node snapshot. The `snapshot` object is deliberately open. */
export interface TelemetryFrame {
  t: 'tlm'
  ts: number
  snapshot: Record<string, unknown>
}

/** Liveness probe. */
export interface PingFrame {
  t: 'ping'
  ts: number
}

/** Liveness answer, echoing the probe's `ts`. */
export interface PongFrame {
  t: 'pong'
  ts: number
}

/** Any frame the node may send. */
export type NodeFrame =
  | HelloFrame
  | HeadFrame
  | DataFrame
  | EndFrame
  | ErrFrame
  | TelemetryFrame
  | PongFrame
  | WsUpFrame
  | WsMsgFrame
  | WsCloseFrame

/** Any frame the control plane may send. */
export type ControlFrame =
  | WelcomeFrame
  | ReqFrame
  | CancelFrame
  | PingFrame
  | WsOpenFrame
  | WsMsgFrame
  | WsCloseFrame

/**
 * Encode raw response bytes, preferring verbatim UTF-8 for textual content.
 *
 * The hot path is `events.mux`, which carries every assistant token; paying
 * base64's inflation there would cost bandwidth and CPU on every chunk.
 *
 * @param bytes - the raw chunk.
 * @param textual - whether the response's content type is known to be textual.
 * @returns the encoded payload.
 */
export function encodePayload(bytes: Uint8Array, textual: boolean): Payload {
  if (textual) {
    try {
      return { enc: 'u', d: new TextDecoder('utf-8', { fatal: true }).decode(bytes) }
    } catch {
      // A chunk boundary split a multi-byte sequence, or the content type lied.
      // Base64 is always correct, so fall through rather than corrupting bytes.
    }
  }
  return { enc: 'b', d: Buffer.from(bytes).toString('base64') }
}

/**
 * Decode an opaque payload back to bytes.
 * @param payload - the encoded body.
 * @returns the raw bytes.
 */
export function decodePayload(payload: Payload): Uint8Array {
  return payload.enc === 'u'
    ? new TextEncoder().encode(payload.d)
    : new Uint8Array(Buffer.from(payload.d, 'base64'))
}

/** Content types whose bytes are carried as verbatim UTF-8 rather than base64. */
const TEXTUAL = /^(?:text\/|application\/(?:json|javascript|xml)|[^;]*\+json)/i

/**
 * Decide whether a response body should ride as UTF-8 text.
 * @param contentType - the response's `content-type`, if any.
 * @returns true when the body is known-textual.
 */
export function isTextualContentType(contentType: string | null): boolean {
  return contentType !== null && TEXTUAL.test(contentType.trim())
}
