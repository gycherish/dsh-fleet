/**
 * The uplink: one outbound WebSocket to the control plane, and the frame pump
 * that serves requests over it.
 *
 * The node dials out, so it needs no inbound port and works behind NAT. The
 * `dsh` namespace is served by handing the request to
 * `toFetchHandler(ctx.apiProxy)` — dsh's own carrier adapter — which is why
 * this file contains no knowledge of any dsh RPC method and does not move when
 * the harness wire does.
 *
 * @module @dsh-fleet/node/uplink
 */

import type { Context } from '@deepseek-ai/cordis'
import { toFetchHandler } from '@deepseek-ai/dsh-host-apiproxy'
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
  token: string
  label: string | undefined
  pluginVersion: string
  /** Backoff floor in milliseconds. */
  reconnectBaseMs: number
  /** Backoff ceiling in milliseconds. */
  reconnectMaxMs: number
  /** Refused if the control plane proposes a larger chunk. */
  maxChunkBytes: number
  /** File-family confinement, passed through to the `fleet` dispatcher. */
  fileAccess: FileAccessPolicy
}

/** Runtime parameters adopted from `welcome` until the connection ends. */
interface Negotiated {
  heartbeatMs: number
  maxChunkBytes: number
  telemetryIntervalMs: number
}

/** Close codes that mean retrying with the same configuration cannot work. */
const FATAL_CLOSE: ReadonlySet<number> = new Set([
  CloseCode.BAD_TOKEN,
  CloseCode.UNKNOWN_NODE,
  CloseCode.BAD_PROTOCOL,
])

/** Origin for the synthetic `Request` handed to the fetch handler. */
const SYNTHETIC_ORIGIN = 'http://dsh-fleet.node'

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
  private heartbeatTimer: ReturnType<typeof setInterval> | undefined
  private telemetryTimer: ReturnType<typeof setInterval> | undefined
  private retryTimer: ReturnType<typeof setTimeout> | undefined
  private missedPongs = 0
  private attempt = 0
  private stopped = false
  private readonly startedAt = Date.now()
  private readonly fetchApi: { fetch: typeof fetch }

  constructor(
    private readonly ctx: Context,
    private readonly options: UplinkOptions,
  ) {
    this.fetchApi = toFetchHandler(ctx.apiProxy)
  }

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
    this.socket?.close(1001, 'node shutting down')
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
      void this.onMessage(event.data)
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
        this.socket?.close(1006, 'heartbeat lost')
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

  private async serveDsh(frame: ReqFrame, signal: AbortSignal): Promise<void> {
    const init: RequestInit = { method: frame.method, headers: frame.headers ?? {}, signal }
    if (frame.body !== undefined && frame.method !== 'GET' && frame.method !== 'HEAD') {
      init.body = decodePayload(frame.body)
    }
    const response = await this.fetchApi.fetch(new Request(`${SYNTHETIC_ORIGIN}${frame.path}`, init))
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
