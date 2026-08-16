/**
 * The `fleet` namespace: this project's own node methods.
 *
 * These exist because the dsh `/api` surface deliberately has no file-content
 * domain — reading files is the model's capability (`ctx.fs` behind
 * `tool-fs`), not the UI's. A phone that wants to look at a file therefore has
 * to ask either the agent or us, and asking us is cheaper.
 *
 * Being our own namespace has a second benefit: these methods do not move when
 * dsh's wire does, so the control plane's file browser is not coupled to a
 * harness release.
 *
 * @module @dsh-fleet/node/fleet
 */

import type { Context } from '@deepseek-ai/cordis'
import type { FsTarget } from '@deepseek-ai/dsh-fs'
import type { ErrorCode } from './protocol.ts'

/** A `fleet` method's answer: a JSON value plus the HTTP-ish status to report. */
export interface FleetResult {
  status: number
  value: unknown
}

/** A refusal a `fleet` method raises; the uplink maps it onto an `err` frame. */
export class FleetError extends Error {
  constructor(readonly code: ErrorCode, message: string) {
    super(message)
    this.name = 'FleetError'
  }
}

/** Confinement and size policy for the `fleet.file.*` family. */
export interface FileAccessPolicy {
  /** Absolute roots a request may reach. Empty disables the family entirely. */
  roots: readonly string[]
  /** Inclusive cap on one `fleet.file.read` response, in bytes. */
  maxReadBytes: number
}

/**
 * Resolve a request path and prove it sits inside one configured root.
 *
 * `ctx.fs`'s sandbox provider confines MUTATIONS, not reads — dsh's own CLI
 * reference is explicit that under `workspace-write` "reads, network access,
 * and process visibility are not confined". A remote file browser therefore
 * needs its own containment or it is an arbitrary-file-read hole, so this
 * check is load-bearing rather than defence in depth.
 *
 * @param ctx - a context with `fs` composed.
 * @param policy - the configured roots and caps.
 * @param path - the caller-supplied path.
 * @param signal - aborts the backend round-trips.
 * @returns the resolved target.
 * @throws {FleetError} `denied` when no configured root contains the target.
 */
async function resolveInsideRoot(
  ctx: Context,
  policy: FileAccessPolicy,
  path: string,
  signal: AbortSignal,
): Promise<FsTarget> {
  if (policy.roots.length === 0) {
    throw new FleetError('denied', 'fleet.file: no roots configured on this node')
  }
  const target = await ctx.fs.resolve(path, { signal })
  for (const root of policy.roots) {
    const rootTarget = await ctx.fs.resolve(root, { signal })
    if (ctx.fs.contains(rootTarget, target)) return target
  }
  throw new FleetError('denied', `fleet.file: "${target.displayPath}" is outside every configured root`)
}

/**
 * Dispatch one `fleet` method.
 * @param ctx - any context in the node's tree.
 * @param policy - file-family confinement policy.
 * @param method - the method name carried in the request `path`.
 * @param body - the decoded JSON request body, or `undefined`.
 * @param signal - request cancellation.
 * @returns the method's result.
 * @throws {FleetError} for unknown methods, absent services, and refusals.
 */
export async function dispatchFleet(
  ctx: Context,
  policy: FileAccessPolicy,
  method: string,
  body: unknown,
  signal: AbortSignal,
): Promise<FleetResult> {
  switch (method) {
    case 'fleet.file.read': {
      const { path } = requireObject(body, ['path'])
      requireFs(ctx)
      const target = await resolveInsideRoot(ctx, policy, path, signal)
      const info = await ctx.fs.stat(target, signal)
      if (info === undefined) throw new FleetError('denied', `not found: ${target.displayPath}`)
      if (info.type !== 'file') throw new FleetError('denied', `not a regular file: ${target.displayPath}`)
      if (info.size !== undefined && info.size > policy.maxReadBytes) {
        throw new FleetError('denied', `file exceeds maxReadBytes (${String(info.size)} > ${String(policy.maxReadBytes)})`)
      }
      // readText owns UTF-8 decoding and binary rejection at the seam, so a
      // binary file fails here as FS_NOT_TEXT rather than reaching the browser.
      const text = await ctx.fs.readText(target, signal)
      return {
        status: 200,
        value: { path: target.displayPath, size: info.size ?? null, version: info.version, text },
      }
    }

    case 'fleet.file.list': {
      const { path } = requireObject(body, ['path'])
      requireFs(ctx)
      const target = await resolveInsideRoot(ctx, policy, path, signal)
      const entries = await ctx.fs.listDir(target, signal)
      return {
        status: 200,
        value: {
          path: target.displayPath,
          entries: entries.map(entry => ({
            name: entry.name,
            type: entry.type,
            path: entry.target.displayPath,
            size: entry.size ?? null,
          })),
        },
      }
    }

    case 'fleet.file.roots':
      return { status: 200, value: { roots: policy.roots, maxReadBytes: policy.maxReadBytes } }

    default:
      throw new FleetError('unsupported', `unknown fleet method: ${method}`)
  }
}

/**
 * Assert the filesystem seam is composed on this node.
 * @param ctx - any context in the node's tree.
 * @throws {FleetError} `unavailable` when no provider is mounted.
 */
function requireFs(ctx: Context): void {
  if (ctx.get('fs') === undefined) {
    throw new FleetError('unavailable', 'fleet.file: this node composes no ctx.fs provider')
  }
}

/**
 * Validate a JSON request body carries the required string fields.
 * @param body - the decoded body.
 * @param keys - required field names.
 * @returns the body narrowed to those fields.
 * @throws {FleetError} `denied` when a field is missing or not a string.
 */
function requireObject<K extends string>(body: unknown, keys: readonly K[]): Record<K, string> {
  if (typeof body !== 'object' || body === null) {
    throw new FleetError('denied', 'fleet: request body must be a JSON object')
  }
  const record = body as Record<string, unknown>
  const out = {} as Record<K, string>
  for (const key of keys) {
    const value = record[key]
    if (typeof value !== 'string' || value.length === 0) {
      throw new FleetError('denied', `fleet: field "${key}" must be a non-empty string`)
    }
    out[key] = value
  }
  return out
}
