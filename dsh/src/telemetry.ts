/**
 * Node telemetry: facts the `/api` surface has no method for.
 *
 * Everything here is read from ordinary Cordis services, which is the whole
 * point — a plugin sees more than the Web UI does. The composed plugin tree
 * with each row's live fiber phase is the headline: a node whose `tool-bash`
 * row sits in `failed` should be visible in the console before anyone opens a
 * session on it.
 *
 * Every service read is optional. A composition missing one degrades the
 * snapshot rather than failing it, because telemetry must never be the reason
 * a node looks offline.
 *
 * @module @dsh-fleet/node/telemetry
 */

import { createRequire } from 'node:module'
import { FiberState, type Context } from '@deepseek-ai/cordis'

/** One row of the composed Loader tree, projected for the console. */
export interface EntrySnapshot {
  /** Loader entry id, the patch-addressable name. */
  id: string
  /** Module specifier the row mounts. */
  name: string
  /** Effective enablement after `!!js disabled` evaluation. */
  enabled: boolean
  /** Root fiber phase, or `null` when the entry has no live root fiber. */
  phase: 'pending' | 'loading' | 'active' | 'failed' | 'unloading' | 'disposed' | null
}

/** One periodic node snapshot. Deliberately open: new keys need no wire change. */
export interface Snapshot extends Record<string, unknown> {
  dshVersion: string
  uptimeMs: number
  entries: EntrySnapshot[]
  agents: { total: number; running: number }
  tools: number
}

/**
 * Structural shape this module reads off the Loader. Declared locally rather
 * than imported so a Loader upgrade that adds fields cannot break the build of
 * a plugin that only projects four of them.
 */
interface LoaderEntryLike {
  id: string
  disabled: boolean
  options: { name?: string; group?: boolean | null }
  fiber?: { state: FiberState } | undefined
}

const FIBER_PHASE: Readonly<Record<FiberState, EntrySnapshot['phase']>> = {
  [FiberState.PENDING]: 'pending',
  [FiberState.LOADING]: 'loading',
  [FiberState.ACTIVE]: 'active',
  [FiberState.FAILED]: 'failed',
  [FiberState.DISPOSED]: 'disposed',
  [FiberState.UNLOADING]: 'unloading',
}

/**
 * Read the harness version from an installed peer package.
 *
 * There is no runtime service that reports it, and every `@deepseek-ai/dsh-*`
 * package shares the release version, so a required peer is an accurate proxy.
 *
 * @returns the resolved version, or `unknown` when resolution fails.
 */
function readDshVersion(): string {
  try {
    const require = createRequire(import.meta.url)
    const manifest = require('@deepseek-ai/dsh-host-apiproxy/package.json') as { version?: string }
    return manifest.version ?? 'unknown'
  } catch {
    // A closed runtime may not expose the manifest through its exports map.
    return 'unknown'
  }
}

const DSH_VERSION = readDshVersion()

/** The harness version this node reports at handshake and in every snapshot. */
export function dshVersion(): string {
  return DSH_VERSION
}

/**
 * Project the composed Loader tree, skipping structural group rows.
 * @param ctx - any context in the node's tree.
 * @returns one row per mountable entry in Loader order; empty without a Loader.
 */
function projectEntries(ctx: Context): EntrySnapshot[] {
  const loader = ctx.get('loader') as { entries?: () => Iterable<LoaderEntryLike> } | undefined
  if (loader?.entries === undefined) return []
  const rows: EntrySnapshot[] = []
  for (const entry of loader.entries()) {
    if (entry.options.group === true) continue
    const state = entry.fiber?.state
    rows.push({
      id: entry.id,
      name: entry.options.name ?? '',
      enabled: !entry.disabled,
      phase: state === undefined ? null : FIBER_PHASE[state],
    })
  }
  return rows
}

/**
 * Count known sessions and how many of their agents are currently running.
 * @param ctx - any context in the node's tree.
 * @returns totals; zeroes when the session or agent registry is absent.
 */
function projectAgents(ctx: Context): { total: number; running: number } {
  const sessions = ctx.get('sessions') as { list?: () => Iterable<{ id: unknown }> } | undefined
  const agents = ctx.get('agents') as { get?: (id: never) => { status?: string } | undefined } | undefined
  if (sessions?.list === undefined) return { total: 0, running: 0 }
  let total = 0
  let running = 0
  for (const session of sessions.list()) {
    total += 1
    if (agents?.get?.(session.id as never)?.status === 'running') running += 1
  }
  return { total, running }
}

/**
 * Build one node snapshot.
 * @param ctx - any context in the node's tree.
 * @param startedAt - `Date.now()` captured when the plugin applied.
 * @returns the snapshot to send as a `tlm` frame.
 */
export function buildSnapshot(ctx: Context, startedAt: number): Snapshot {
  const tools = ctx.get('tools') as { schemas?: () => readonly unknown[] } | undefined
  return {
    dshVersion: DSH_VERSION,
    uptimeMs: Date.now() - startedAt,
    entries: projectEntries(ctx),
    agents: projectAgents(ctx),
    tools: tools?.schemas?.().length ?? 0,
  }
}
