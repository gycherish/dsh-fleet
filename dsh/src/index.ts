/**
 * `@dsh-fleet/node` — serves this machine's dsh `/api` surface to a dsh-fleet
 * control plane over one outbound WebSocket.
 *
 * The plugin is a second carrier for `ctx.apiProxy`. dsh's own HTTP carrier
 * (`toFetchHandler` behind the `/api` route) is the first; the gateway is
 * transport-agnostic by design, so nothing here forks or patches the harness.
 *
 * One consequence deserves stating plainly: this carrier does **not** pass
 * through `dsh-client-connection`'s `/api` route, so the browser-trust fence
 * and its loopback pin on the privileged method set (`settings.*`,
 * `credentials.*`, `host.pickDirectory`, `host.openPath`, agent-preset
 * authoring) are not applied here. Re-gating those is the control plane's
 * responsibility, and it is a requirement, not a nicety.
 *
 * This module uses NAMED EXPORTS ONLY. A default export hides loader metadata
 * such as `inject` — see dsh's `docs/postmortem/0001-acp-default-export-drops-inject.md`.
 *
 * @module @dsh-fleet/node
 */

import { createRequire } from 'node:module'
import type { Context } from '@deepseek-ai/cordis'
import z from '@deepseek-ai/schemastery'
import type {} from '@deepseek-ai/dsh-host-apiproxy'
import { Uplink } from './uplink.ts'

export { PROTOCOL_VERSION } from './protocol.ts'
export type { Snapshot, EntrySnapshot } from './telemetry.ts'

/** Loader display name for this plugin. */
export const name = 'dsh-fleet-node'

/**
 * The gateway is the only hard requirement: without it there is nothing to
 * carry. `fs` is read through `ctx.get()` at the use site instead, so a node
 * composing no filesystem provider still serves the `dsh` namespace.
 */
export const inject = ['apiProxy']

/** Plugin configuration. */
export interface Config {
  /** Control-plane uplink endpoint, `ws://` or `wss://`. */
  url: string
  /** This node's registered id. */
  nodeId: string
  /** Node token issued by `dshf node add`. */
  token: string
  /** Operator-facing display name; defaults to the hostname at the console. */
  label?: string
  /** Backoff floor for reconnects, in milliseconds. */
  reconnectBaseMs: number
  /** Backoff ceiling for reconnects, in milliseconds. */
  reconnectMaxMs: number
  /** Largest response chunk this node will put on the wire, in bytes. */
  maxChunkBytes: number
  /**
   * Origin of this node's own `dsh web` server. Asset requests are served
   * from it rather than reimplemented here, so SPA fallback, the boot manifest
   * injected into `index.html`, and `/plugins/<id>/client.js` all behave
   * exactly as they do for a local browser.
   *
   * Change it when the surface binds a non-default port.
   */
  localWebUrl: string
  /**
   * Absolute directories `fleet.file.*` may read. EMPTY DISABLES THE FAMILY.
   *
   * The default is empty on purpose: `ctx.fs` confines mutations, not reads, so
   * an unconfigured root list would make a remote file browser an
   * arbitrary-file-read surface on this machine.
   */
  fileRoots: string[]
  /** Inclusive cap on one `fleet.file.read` response, in bytes. */
  maxReadBytes: number
}

/** Runtime schema; Schemastery validates and fills defaults before `apply`. */
export const Config: z<Config> = z.object({
  url: z.string().required(),
  nodeId: z.string().required(),
  token: z.string().required(),
  label: z.string(),
  reconnectBaseMs: z.natural().min(100).default(1_000),
  reconnectMaxMs: z.natural().min(1_000).default(30_000),
  maxChunkBytes: z.natural().min(4_096).default(262_144),
  localWebUrl: z.string().default('http://127.0.0.1:3080'),
  fileRoots: z.array(z.string()).default([]),
  maxReadBytes: z.natural().min(1_024).default(1_048_576),
})

/**
 * Read this package's own version for the handshake descriptor.
 * @returns the manifest version, or `0.0.0` when it cannot be resolved.
 */
function pluginVersion(): string {
  try {
    const require = createRequire(import.meta.url)
    return (require('../package.json') as { version?: string }).version ?? '0.0.0'
  } catch {
    // A bundled build may have no adjacent manifest; the version is cosmetic.
    return '0.0.0'
  }
}

/**
 * Reject configuration that cannot work, at load, with an actionable message.
 * @param config - the validated configuration.
 * @throws when the endpoint, identity, or backoff window is unusable.
 */
function assertUsable(config: Config): void {
  let parsed: URL
  try {
    parsed = new URL(config.url)
  } catch {
    throw new Error(`dsh-fleet-node: url is not a valid URL: ${config.url}`)
  }
  if (parsed.protocol !== 'ws:' && parsed.protocol !== 'wss:') {
    throw new Error(`dsh-fleet-node: url must be ws:// or wss://, got ${parsed.protocol}`)
  }
  if (config.nodeId.trim().length === 0) throw new Error('dsh-fleet-node: nodeId must not be blank')
  if (config.token.trim().length === 0) {
    throw new Error('dsh-fleet-node: token must not be blank (set DSH_FLEET_TOKEN, or mint one with `dshf node add`)')
  }
  if (config.reconnectMaxMs < config.reconnectBaseMs) {
    throw new Error('dsh-fleet-node: reconnectMaxMs must be at least reconnectBaseMs')
  }
  let local: URL
  try {
    local = new URL(config.localWebUrl)
  } catch {
    throw new Error(`dsh-fleet-node: localWebUrl is not a valid URL: ${config.localWebUrl}`)
  }
  if (local.protocol !== 'http:' && local.protocol !== 'https:') {
    throw new Error(`dsh-fleet-node: localWebUrl must be http or https, got ${local.protocol}`)
  }
  for (const root of config.fileRoots) {
    if (root.trim().length === 0) throw new Error('dsh-fleet-node: fileRoots must not contain blank entries')
  }
}

/**
 * Mount the uplink for this node.
 * @param ctx - the plugin context, with `apiProxy` ready.
 * @param config - validated configuration.
 */
export function apply(ctx: Context, config: Config): void {
  assertUsable(config)

  ctx.effect(() => {
    const uplink = new Uplink(ctx, {
      url: config.url,
      nodeId: config.nodeId,
      token: config.token,
      label: config.label,
      pluginVersion: pluginVersion(),
      reconnectBaseMs: config.reconnectBaseMs,
      reconnectMaxMs: config.reconnectMaxMs,
      maxChunkBytes: config.maxChunkBytes,
      localWebUrl: config.localWebUrl,
      fileAccess: { roots: config.fileRoots, maxReadBytes: config.maxReadBytes },
    })
    uplink.start()
    return () => uplink.stop()
  }, 'fleet-node.uplink()')
}
