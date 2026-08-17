/**
 * The uplink: one outbound WebSocket to the control plane, and the frame pump
 * that serves requests over it.
 *
 * The node dials out, so it needs no inbound port and works behind NAT. A
 * `dsh` request is replayed verbatim against this node's own web server, so
 * this file contains no knowledge of any dsh route or RPC method and does not
 * move when the harness wire does.
 *
 * @module @dsh-fleet/node/uplink
 */

import type { Context } from '@deepseek-ai/cordis'
import {
  CloseCode,
  PROTOCOL_VERSION,
  SUBPROTOCOL,
  decodePayload,
  encodePayload,
  isTextualContentType,
  type ControlFrame,
  type ErrorCode,
  type NodeFrame,
  type ReqFrame,
  type WelcomeFrame,
} from './protocol.ts'
import { FleetError, dispatchFleet, type FileAccessPolicy } from './fleet.ts'
import { buildSnapshot, dshVersion } from './telemetry.ts'

/** Everything the uplink needs from the plugin's validated configuration. */
export interface UplinkOptions {
  url: string
  nodeId: string
  /** Account the token belongs to, when it is a person's rather than this machine's. */
  username: string | undefined
  token: string
  label: string | undefined
  pluginVersion: string
  /** Backoff floor in milliseconds. */
  reconnectBaseMs: number
  /** Backoff ceiling in milliseconds. */
  reconnectMaxMs: number
  /** Refused if the control plane proposes a larger chunk. */
  maxChunkBytes: number
  /** Origin of this node's own `dsh web` server, for asset requests. */
  localWebUrl: string
  /** File-family confinement, passed through to the `fleet` dispatcher. */
  fileAccess: FileAccessPolicy
}

/** Runtime parameters adopted from `welcome` until the connection ends. */
interface Negotiated {
  heartbeatMs: number
  maxChunkBytes: number
  telemetryIntervalMs: number
}

/** The only close code an application may send outside the 3000-4999 range. */
const CLOSE_NORMAL = 1000

/**
 * Close a socket without letting the close itself throw.
 *
 * `WebSocket.close()` accepts 1000 or 3000-4999 and rejects everything else
 * with InvalidAccessError — 1001 "going away" and 1006 "abnormal" included,
 * because the runtime reserves those for itself. The reason has a 123-byte
 * ceiling that throws the same way.
 *
 * Every call here runs inside a close handler or a timer, where there is no
 * caller to catch anything: a throw becomes an unhandled rejection and takes
 * the whole harness process down. It did, on every uplink drop that left a
 * bridge open. One of these codes also arrives from the control plane, which
 * would make the ceiling a remote kill switch — so sanitise rather than trust,
 * and swallow whatever the runtime still objects to.
 */
export function closeSocket(socket: WebSocket | undefined, code: number, reason: string): void {
  if (socket === undefined) return
  const safe = code === CLOSE_NORMAL || (code >= 3000 && code <= 4999) ? code : CLOSE_NORMAL
  const bytes = new TextEncoder().encode(reason)
  // Decoding a cut that lands mid-character yields a replacement char; drop it.
  const text = bytes.length <= 123 ? reason : new TextDecoder().decode(bytes.slice(0, 123)).replace(/�$/, '')
  try {
    socket.close(safe, text)
  } catch {
    // Already closing, or the runtime refused anyway. The socket is going away
    // either way, and nothing downstream depends on how it got there.
  }
}

/** Close codes that mean retrying with the same configuration cannot work. */
const FATAL_CLOSE: ReadonlySet<number> = new Set([
  CloseCode.BAD_TOKEN,
  CloseCode.UNKNOWN_NODE,
  CloseCode.BAD_PROTOCOL,
])

/**
 * Owns one logical connection to the control plane across reconnects.
 *
 * `start()` returns immediately; the first connection attempt runs in the
 * background so a control plane that is down never blocks `dsh web` from
 * booting. `stop()` is idempotent and reaches quiescence.
 */
export class Uplink {
  private socket: WebSocket | undefined
  private negotiated: Negotiated | undefined
  private readonly inFlight = new Map<string, AbortController>()
  /** Bridged browser sockets, keyed by the control plane's correlation id. */
  private readonly bridges = new Map<string, WebSocket>()
  private heartbeatTimer: ReturnType<typeof setInterval> | undefined
  private telemetryTimer: ReturnType<typeof setInterval> | undefined
  private retryTimer: ReturnType<typeof setTimeout> | undefined
  private missedPongs = 0
  private attempt = 0
  private stopped = false
  private readonly startedAt = Date.now()

  constructor(
    private readonly ctx: Context,
    private readonly options: UplinkOptions,
  ) {}

  /** Begin connecting, and keep reconnecting until {@link stop} is called. */
  start(): void {
    this.connect()
  }

  /**
   * Stop reconnecting, abort in-flight work, and close the socket.
   * @returns once every timer is cleared and the socket is closing.
   */
  async stop(): Promise<void> {
    this.stopped = true
    this.clearTimers()
    for (const controller of this.inFlight.values()) controller.abort(new Error('node is shutting down'))
    this.inFlight.clear()
    for (const bridge of this.bridges.values()) closeSocket(bridge, CLOSE_NORMAL, 'node shutting down')
    this.bridges.clear()
    closeSocket(this.socket, CLOSE_NORMAL, 'node shutting down')
    this.socket = undefined
    // Yield once so the close frame reaches the socket before the fiber's
    // remaining disposers run; the control plane then marks the node offline
    // immediately instead of after a heartbeat timeout.
    await Promise.resolve()
  }

  private log(): { warn: (message: string) => void; info: (message: string) => void } {
    const logger = this.ctx.logger('fleet-node')
    return {
      warn: (message: string) => { logger.warn(message) },
      info: (message: string) => { logger.info(message) },
    }
  }

  private connect(): void {
    if (this.stopped) return
    let socket: WebSocket
    try {
      socket = new WebSocket(this.options.url, [SUBPROTOCOL])
    } catch (error: unknown) {
      // A malformed URL throws synchronously; retrying cannot fix it, but the
      // node stays up so an operator can correct the config and HMR reload.
      this.log().warn(`uplink: cannot open ${this.options.url}: ${String(error)}`)
      return
    }
    this.socket = socket
    socket.binaryType = 'arraybuffer'

    socket.addEventListener('open', () => {
      this.attempt = 0
      this.send({
        t: 'hello',
        protocol: PROTOCOL_VERSION,
        nodeId: this.options.nodeId,
        ...(this.options.username === undefined ? {} : { username: this.options.username }),
        token: this.options.token,
        node: {
          label: this.options.label,
          platform: process.platform,
          arch: process.arch,
          dshVersion: dshVersion(),
          pluginVersion: this.options.pluginVersion,
          cwd: process.cwd(),
        },
        caps: ['dsh', 'fleet.telemetry', 'fleet.file'],
      })
    })

    socket.addEventListener('message', (event: MessageEvent) => {
      // Nothing above this frame can catch: an unhandled rejection here ends
      // the harness process, so a malformed frame from the control plane would
      // be enough to stop the machine's dsh. Log and keep the uplink running.
      this.onMessage(event.data).catch((error: unknown) => {
        this.log().warn(`uplink: frame handler failed: ${String(error)}`)
      })
    })

    socket.addEventListener('close', (event: CloseEvent) => {
      this.onClose(event.code, event.reason)
    })

    socket.addEventListener('error', () => {
      // The close event always follows and carries the actionable code; logging
      // here as well would double every transient reconnect line.
    })
  }

  private onClose(code: number, reason: string): void {
    if (this.socket === undefined) return
    this.socket = undefined
    this.negotiated = undefined
    this.clearTimers()
    for (const controller of this.inFlight.values()) controller.abort(new Error('uplink closed'))
    this.inFlight.clear()
    // Correlation ids are per-connection, so a bridged socket cannot survive
    // the uplink that addressed it.
    for (const bridge of this.bridges.values()) closeSocket(bridge, CLOSE_NORMAL, 'uplink closed')
    this.bridges.clear()
    if (this.stopped) return

    if (FATAL_CLOSE.has(code)) {
      // Token, identity, and protocol failures are configuration errors. Retrying
      // would hammer the control plane with a request that cannot start working.
      this.log().warn(`uplink: refused by control plane (${String(code)}${reason ? `: ${reason}` : ''}); not retrying`)
      this.stopped = true
      return
    }

    this.attempt += 1
    const ceiling = Math.min(this.options.reconnectMaxMs, this.options.reconnectBaseMs * 2 ** (this.attempt - 1))
    const delay = Math.round(ceiling * (0.5 + Math.random() * 0.5))
    this.retryTimer = setTimeout(() => { this.connect() }, delay)
  }

  private async onMessage(raw: unknown): Promise<void> {
    let frame: ControlFrame
    try {
      frame = JSON.parse(typeof raw === 'string' ? raw : new TextDecoder().decode(raw as ArrayBuffer)) as ControlFrame
    } catch {
      this.log().warn('uplink: control plane sent an unparseable frame')
      return
    }
    switch (frame.t) {
      case 'welcome':
        this.onWelcome(frame)
        return
      case 'ping':
        this.missedPongs = 0
        this.send({ t: 'pong', ts: frame.ts })
        return
      case 'cancel':
        this.inFlight.get(frame.id)?.abort(new Error('cancelled by control plane'))
        return
      case 'req':
        await this.serve(frame)
        return
      case 'ws.open':
        this.bridgeOpen(frame)
        return
      case 'ws.msg':
        this.bridges.get(frame.id)?.send(new TextDecoder().decode(decodePayload(frame.body)))
        return
      case 'ws.close':
        closeSocket(this.bridges.get(frame.id), frame.code ?? CLOSE_NORMAL, frame.reason ?? '')
        return
      default:
        // Forward compatibility: a newer control plane may add frames this
        // build does not know. Ignoring is correct; closing would make an
        // additive change a breaking one.
        this.log().warn(`uplink: ignoring unknown frame "${String((frame as { t: unknown }).t)}"`)
    }
  }

  private onWelcome(frame: WelcomeFrame): void {
    this.negotiated = {
      heartbeatMs: frame.heartbeatMs,
      maxChunkBytes: Math.min(frame.maxChunkBytes, this.options.maxChunkBytes),
      telemetryIntervalMs: frame.telemetryIntervalMs,
    }
    this.missedPongs = 0
    this.log().info(`uplink: connected to ${this.options.url} as "${this.options.nodeId}"`)

    this.heartbeatTimer = setInterval(() => {
      this.missedPongs += 1
      if (this.missedPongs > 2) {
        // Two missed probes means the socket is open but the peer is gone —
        // TCP will not tell us for minutes, so close and let backoff retry.
        closeSocket(this.socket, CloseCode.HEARTBEAT_LOST, 'heartbeat lost')
        return
      }
      this.send({ t: 'pong', ts: Date.now() })
    }, frame.heartbeatMs)

    this.sendTelemetry()
    this.telemetryTimer = setInterval(() => { this.sendTelemetry() }, frame.telemetryIntervalMs)
  }

  private sendTelemetry(): void {
    try {
      this.send({ t: 'tlm', ts: Date.now(), snapshot: buildSnapshot(this.ctx, this.startedAt) })
    } catch (error: unknown) {
      // Telemetry must never be why a node looks offline.
      this.log().warn(`uplink: telemetry snapshot failed: ${String(error)}`)
    }
  }

  private async serve(frame: ReqFrame): Promise<void> {
    const controller = new AbortController()
    this.inFlight.set(frame.id, controller)
    try {
      if (frame.ns === 'dsh') {
        await this.serveDsh(frame, controller.signal)
      } else if (frame.ns === 'fleet') {
        await this.serveFleet(frame, controller.signal)
      } else {
        this.fail(frame.id, 'unsupported', `unknown namespace "${String(frame.ns)}"`)
      }
    } catch (error: unknown) {
      const code: ErrorCode = error instanceof FleetError
        ? error.code
        : controller.signal.aborted ? 'cancelled' : 'internal'
      this.fail(frame.id, code, error instanceof Error ? error.message : String(error))
    } finally {
      this.inFlight.delete(frame.id)
    }
  }

  /**
   * Open a socket to this node's own server and relay it to the browser.
   *
   * The node dials rather than the control plane, for the same reason it dials
   * the uplink: nothing here listens for inbound connections.
   */
  private bridgeOpen(frame: { id: string; path: string }): void {
    const target = new URL(frame.path, this.options.localWebUrl)
    target.protocol = target.protocol === 'https:' ? 'wss:' : 'ws:'

    let socket: WebSocket
    try {
      socket = new WebSocket(target)
    } catch (error: unknown) {
      this.fail(frame.id, 'internal', `cannot open ${target.href}: ${String(error)}`)
      return
    }
    this.bridges.set(frame.id, socket)

    socket.addEventListener('open', () => {
      this.send({ t: 'ws.up', id: frame.id, protocol: socket.protocol || undefined })
    })
    socket.addEventListener('message', (event: MessageEvent) => {
      const text = typeof event.data === 'string'
        ? event.data
        // These downlinks are text-only today; decoding keeps one payload
        // shape rather than adding a binary flag nothing sets.
        : new TextDecoder().decode(event.data as ArrayBuffer)
      this.send({ t: 'ws.msg', id: frame.id, body: encodePayload(new TextEncoder().encode(text), true) })
    })
    socket.addEventListener('close', (event: CloseEvent) => {
      this.bridges.delete(frame.id)
      this.send({ t: 'ws.close', id: frame.id, code: event.code, reason: event.reason })
    })
    socket.addEventListener('error', () => {
      // `close` always follows and carries the code the other end should see.
    })
  }

  private async serveDsh(frame: ReqFrame, signal: AbortSignal): Promise<void> {
    const init: RequestInit = { method: frame.method, headers: frame.headers ?? {}, signal }
    if (frame.body !== undefined && frame.method !== 'GET' && frame.method !== 'HEAD') {
      init.body = decodePayload(frame.body)
    }

    // Everything goes to this node's own HTTP server, `/api` included.
    //
    // Calling `ctx.apiProxy` in-process looks more direct and is wrong: `/api`
    // is a COMPOSITE handler. The Typert gateway claims its Remote endpoints
    // first (`/api/pluginInventory/list`, `/api/dynamicCordisRunner/*`, the
    // goal and feedback domains) and only unclaimed ones fall through to the
    // API Proxy. A carrier wired straight to the proxy answers 404 for that
    // whole surface -- which is exactly what the mobile suite caught.
    //
    // The same reasoning covers the frontend: SPA fallback, the boot manifest
    // injected into index.html, and `/plugins/<id>/client.js` all live on the
    // webserver's fallback seat. One hop over loopback buys the complete,
    // correct surface instead of an approximation of two of its parts.
    //
    // The request carries no `Origin` and no Fetch Metadata, and its Host is
    // loopback, so dsh's `/api` trust fence admits it. The control plane's own
    // gate remains the boundary that matters.
    const response = await fetch(new URL(frame.path, this.options.localWebUrl), init)
    await this.stream(frame.id, response, signal)
  }

  private async serveFleet(frame: ReqFrame, signal: AbortSignal): Promise<void> {
    const body: unknown = frame.body === undefined
      ? undefined
      : JSON.parse(new TextDecoder().decode(decodePayload(frame.body)))
    const result = await dispatchFleet(this.ctx, this.options.fileAccess, frame.path, body, signal)
    const encoded = new TextEncoder().encode(JSON.stringify(result.value))
    this.send({ t: 'head', id: frame.id, status: result.status, headers: { 'content-type': 'application/json' } })
    this.send({ t: 'data', id: frame.id, seq: 0, body: encodePayload(encoded, true) })
    this.send({ t: 'end', id: frame.id, chunks: 1 })
  }

  /**
   * Forward one response as `head` → `data`* → `end`, honouring backpressure.
   *
   * The chunking bound exists for `session.export`, which streams a whole ZIP:
   * without it a large archive would be buffered into the socket faster than
   * the network drains it.
   */
  private async stream(id: string, response: Response, signal: AbortSignal): Promise<void> {
    const headers: Record<string, string> = {}
    response.headers.forEach((value, key) => { headers[key] = value })
    this.send({ t: 'head', id, status: response.status, headers })

    const textual = isTextualContentType(response.headers.get('content-type'))
    const limit = this.negotiated?.maxChunkBytes ?? this.options.maxChunkBytes
    let seq = 0

    if (response.body === null) {
      this.send({ t: 'end', id, chunks: 0 })
      return
    }

    const reader = response.body.getReader()
    try {
      while (true) {
        const { done, value } = await reader.read()
        if (done) break
        if (signal.aborted) throw new Error('cancelled')
        for (let offset = 0; offset < value.length; offset += limit) {
          await this.drain(limit)
          this.send({ t: 'data', id, seq, body: encodePayload(value.subarray(offset, offset + limit), textual) })
          seq += 1
        }
      }
      this.send({ t: 'end', id, chunks: seq })
    } finally {
      // Releasing the lock lets the response cancel cleanly when the caller
      // aborted mid-stream; leaving it held would pin the underlying source.
      reader.releaseLock()
      if (signal.aborted) await response.body.cancel().catch(() => undefined)
    }
  }

  /** Wait until the socket's send buffer falls under eight chunks. */
  private async drain(limit: number): Promise<void> {
    const socket = this.socket
    if (socket === undefined) throw new Error('uplink closed mid-response')
    const high = limit * 8
    while (socket.bufferedAmount > high) {
      if (socket.readyState !== WebSocket.OPEN) throw new Error('uplink closed mid-response')
      await new Promise(resolve => setTimeout(resolve, 10))
    }
  }

  private fail(id: string, code: ErrorCode, message: string): void {
    this.send({ t: 'err', id, code, message })
  }

  private send(frame: NodeFrame): void {
    const socket = this.socket
    if (socket === undefined || socket.readyState !== WebSocket.OPEN) return
    socket.send(JSON.stringify(frame))
  }

  private clearTimers(): void {
    if (this.heartbeatTimer !== undefined) clearInterval(this.heartbeatTimer)
    if (this.telemetryTimer !== undefined) clearInterval(this.telemetryTimer)
    if (this.retryTimer !== undefined) clearTimeout(this.retryTimer)
    this.heartbeatTimer = undefined
    this.telemetryTimer = undefined
    this.retryTimer = undefined
  }
}
