/**
 * `@dsh-fleet/node` — serves this machine's whole dsh web surface to a
 * dsh-fleet control plane over one outbound WebSocket.
 *
 * Requests are replayed against this node's own HTTP server rather than
 * against `ctx.apiProxy` directly. `/api` is a composite handler: the Typert
 * gateway claims its Remote endpoints and only the rest fall through to the
 * proxy, so a carrier wired straight to the proxy silently loses that surface.
 * Replaying the request keeps the plugin ignorant of every dsh route, which is
 * what stops it moving when the harness does.
 *
 * One consequence deserves stating plainly: those requests reach dsh's `/api`
 * fence as loopback, so its pin on the privileged method set (`settings.*`,
 * `credentials.*`, `host.pickDirectory`, `host.openPath`, agent-preset
 * authoring) does not restrain a remote caller. Re-gating them is the control
 * plane's responsibility, and it is a requirement, not a nicety.
 *
 * This module uses NAMED EXPORTS ONLY. A default export hides loader metadata
 * such as `inject` — see dsh's `docs/postmortem/0001-acp-default-export-drops-inject.md`.
 *
 * @module @dsh-fleet/node
 */

import { createRequire } from 'node:module'
import { hostname } from 'node:os'
import type { Context } from '@deepseek-ai/cordis'
import z from '@deepseek-ai/schemastery'
import { Uplink } from './uplink.ts'
import { mountSetup, type SetupOptions, type SetupStatus } from './setup.ts'
import { readSaved, writeSaved } from './store.ts'

export { PROTOCOL_VERSION } from './protocol.ts'
export type { Snapshot, EntrySnapshot } from './telemetry.ts'

/** Loader display name for this plugin. */
export const name = 'dsh-fleet-node'

/**
 * `webRuntime` is the readiness signal: the web surface provides it only after
 * its HTTP server has bound. Waiting on it means the uplink never announces a
 * node whose own server would refuse the first request, and it keeps this
 * plugin off a headless composition, which has no web UI to remote in the
 * first place.
 *
 * `fs` is read through `ctx.get()` at the use site instead, so a node without
 * a filesystem provider still serves everything but `fleet.file.*`.
 */
export const inject = ['webRuntime']

/**
 * Plugin configuration.
 *
 * The three connection fields are optional so this plugin can be installed and
 * then configured, rather than the other way round. Unconfigured it loads,
 * appears in Settings → Plugins, and connects to nothing; fill them in and the
 * loader reloads the plugin, which brings the uplink up. Each also falls back
 * to an environment variable, which is how a container configures it with no
 * UI at all.
 */
export interface Config {
  /** Control-plane uplink endpoint, `ws://` or `wss://`. Empty stays offline. */
  url: string
  /** This machine's name in the console. Empty takes the hostname. */
  nodeId: string
  /**
   * Console account this token belongs to.
   *
   * Set it and the machine enrols itself under that account on first
   * connection, with a token that person minted for themselves — no step on the
   * control plane. Leave it empty and `token` is this machine's own, from
   * `dshf node add`, which is the shape a container wants.
   */
  username?: string
  /** User token (`ut_`) with a username, or machine token (`nt_`) without one. */
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

/**
 * Runtime schema; Schemastery validates and fills defaults before `apply`.
 *
 * Descriptions are not decoration here: this schema is the plugin's settings
 * form, and it is the only instructions anyone configuring a node from the UI
 * will see.
 */
export const Config: z<Config> = z.object({
  url: z.string()
    .description('Control-plane uplink, e.g. wss://fleet.example.com/uplink. Leave empty to stay offline.')
    .default(''),
  nodeId: z.string()
    .description('This machine\'s name in the console. Leave empty to use its hostname.')
    .default(''),
  username: z.string()
    .description('Your console account. With it the machine enrols itself; leave empty to use a machine token from `dshf node add`.')
    .default(''),
  token: z.string().role('secret')
    .description('Your token from the console (ut_…), or the machine token `dshf node add` printed (nt_…).')
    .default(''),
  label: z.string()
    .description('Display name in the console. Defaults to this machine\'s id.'),
  reconnectBaseMs: z.natural().min(100)
    .description('Backoff floor between reconnect attempts, in milliseconds.')
    .default(1_000),
  reconnectMaxMs: z.natural().min(1_000)
    .description('Backoff ceiling between reconnect attempts, in milliseconds.')
    .default(30_000),
  maxChunkBytes: z.natural().min(4_096)
    .description('Largest response chunk put on the wire, in bytes.')
    .default(262_144),
  localWebUrl: z.string()
    .description('This machine\'s own dsh web origin. Change it only if the surface binds a non-default port.')
    .default('http://127.0.0.1:3080'),
  fileRoots: z.array(z.string())
    .description('Absolute directories the console may browse. EMPTY DISABLES remote file access, which is the safe default.')
    .default([]),
  maxReadBytes: z.natural().min(1_024)
    .description('Cap on one remote file read, in bytes.')
    .default(1_048_576),
})

/**
 * One connection setting, from the three places it can come from.
 *
 * Declared configuration first, then what the setup page saved, then the
 * environment. Declared beats saved so that a value written into a config file
 * on purpose is not silently overridden by an old save — and when it does
 * shadow one, the page says so rather than appearing to ignore the form.
 * Empty counts as absent at every level, which is what makes an unconfigured
 * bundled install fall through to the page in the first place.
 */
function setting(value: string | undefined, saved: string | undefined, envVar: string): string {
  for (const candidate of [value, saved, process.env[envVar]]) {
    const trimmed = (candidate ?? '').trim()
    if (trimmed !== '') return trimmed
  }
  return ''
}

/**
 * This machine's name, taken from its hostname.
 *
 * Asking someone to invent a name for the machine they are sitting at is a
 * field they have to think about for no gain — the machine already knows what
 * it is called. Configuration still wins, because a hostname is not always the
 * name a fleet wants to see.
 *
 * The id reaches a URL (`/_fleet/select/<id>`) and a primary key, so it is
 * reduced to the characters both handle: a hostname may carry dots, a domain
 * suffix, and capitals that would otherwise make two spellings of one machine.
 *
 * @returns a usable id, or an empty string if the hostname yields nothing.
 */
export function machineName(): string {
  let raw: string
  try {
    raw = hostname()
  } catch {
    // Some sandboxes refuse it; the caller then reports the field as missing.
    return ''
  }
  return sanitiseName(raw)
}

/** Reduce a hostname to an id a URL and a primary key both accept. */
export function sanitiseName(raw: string): string {
  return raw
    .split('.')[0]                    // `laptop.local` is the same machine as `laptop`
    ?.toLowerCase()
    .replace(/[^a-z0-9-]+/g, '-')
    .replace(/^-+|-+$/g, '')
    .slice(0, 63)                     // one DNS label, which is more than enough
    ?? ''
}

/** What the uplink needs to exist, resolved across both sources. */
interface Connection {
  url: string
  nodeId: string
  token: string
  /** Empty when the token is this machine's own. */
  username: string
}

/**
 * Resolve the connection across all three sources.
 *
 * Three rather than one because they serve different people: a container sets
 * variables and never opens a UI, an operator declares a config file, and a
 * person at the machine uses the setup page. The store is read fresh on every
 * call so a save is visible to the reload it triggers.
 *
 * @returns the connection, or undefined when it is not configured at all.
 */
function connection(config: Config): Connection | undefined {
  const saved = readSaved()
  const resolved = {
    url: setting(config.url, saved.url, 'DSH_FLEET_URL'),
    // Falls back to the hostname: the machine already knows its own name.
    nodeId: setting(config.nodeId, saved.nodeId, 'DSH_FLEET_NODE_ID') || machineName(),
    token: setting(config.token, saved.token, 'DSH_FLEET_TOKEN'),
    // Optional: its presence chooses self-enrolment over a machine token.
    username: setting(config.username, saved.username, 'DSH_FLEET_USERNAME'),
  }
  const required: (keyof Connection)[] = ['url', 'nodeId', 'token']
  return required.every(key => resolved[key] !== '') ? resolved : undefined
}

/**
 * What the setup page reads and writes.
 *
 * Saving goes through this plugin's own loader entry, which is what makes the
 * change take effect: Cordis writes it to the config file and reloads the
 * plugin, so the uplink comes up without restarting dsh.
 */
function setupHooks(ctx: Context, config: Config): SetupOptions {
  return {
    read: () => {
      const saved = readSaved()
      const settings = connection(config)
      const status: SetupStatus = settings === undefined
        ? { state: 'offline', detail: 'not configured' }
        : { state: 'connecting', detail: `to ${settings.url}` }
      return {
        fields: {
          url: setting(config.url, saved.url, 'DSH_FLEET_URL'),
          // Shown resolved, so the placeholder is the name that would be used.
          nodeId: setting(config.nodeId, saved.nodeId, 'DSH_FLEET_NODE_ID') || machineName(),
          username: setting(config.username, saved.username, 'DSH_FLEET_USERNAME'),
          token: '',
          label: config.label ?? '',
        },
        // Never the value: a masked token round-tripping through a form is how
        // a real secret gets overwritten by asterisks.
        tokenSet: setting(config.token, saved.token, 'DSH_FLEET_TOKEN') !== '',
        status,
      }
    },

    save: async (fields) => {
      if (fields.url !== '') assertEndpoint(fields.url)

      // The store is what survives a restart. The loader cannot do it: an entry
      // mounted from a patch overlay has no writable config behind it, so
      // `entry.update()` reloads the plugin and then loses the value — measured,
      // not assumed.
      const saved = readSaved()
      // An empty token field means "keep what is stored", which is the only way
      // a form showing a mask can be saved without destroying the secret.
      const token = fields.token !== '' ? fields.token : (saved.token ?? '')
      writeSaved({
        url: fields.url,
        nodeId: fields.nodeId,
        username: fields.username,
        label: fields.label,
        token,
      })

      // And this is what makes it take effect now rather than at the next start.
      // A declared config value beats the store, so say so instead of leaving
      // someone staring at a form that appears to have been ignored.
      const shadowed = (['url', 'nodeId', 'username'] as const)
        .filter(key => (config[key] ?? '').trim() !== '')
      if (shadowed.length > 0) {
        throw new Error(
          `saved, but ${shadowed.join(', ')} ${shadowed.length === 1 ? 'is' : 'are'} also set in this `
          + 'deployment\'s config file, which takes precedence. Remove it there for this page to take effect.',
        )
      }
      await restart(ctx)
    },
  }
}

/**
 * Reload this plugin so a saved change takes effect without restarting dsh.
 *
 * Done by touching the entry's own config rather than restarting the fiber
 * directly: the loader watches for a config diff, and a no-op update would not
 * produce one. The values it writes are the ones already resolved, so a
 * writable config file ends up agreeing with the store rather than fighting it.
 */
async function restart(ctx: Context): Promise<void> {
  const entry = ctx.fiber.entry
  if (entry === undefined) return
  const saved = readSaved()
  await entry.update({
    config: {
      ...(entry.options.config as object),
      url: saved.url ?? '',
      nodeId: saved.nodeId ?? '',
      username: saved.username ?? '',
      token: saved.token ?? '',
      label: saved.label ?? '',
    },
  })
}

/**
 * Reject an endpoint the uplink could never dial, before it is written.
 *
 * Saving an unusable value would reload the plugin into a failure the page
 * cannot then explain, so the refusal belongs at the form.
 */
function assertEndpoint(url: string): void {
  let parsed: URL
  try {
    parsed = new URL(url)
  } catch {
    throw new Error(`"${url}" is not a URL`)
  }
  if (parsed.protocol !== 'ws:' && parsed.protocol !== 'wss:') {
    throw new Error(`the address must start with ws:// or wss://, not ${parsed.protocol}`)
  }
}

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
    throw new Error("dsh-fleet-node: token must not be blank")
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
 * Warn when this node's directory picker cannot be operated remotely.
 *
 * The `native` backend opens a chooser on THIS machine's desktop, which a
 * remote browser can neither see nor click, and the fleet carrier refuses
 * `host.pickDirectory` as a privileged method besides. The result in the
 * browser is an opaque failure on "add workspace", far from its cause — so the
 * node says so at load instead.
 *
 * The service is read through an injected child fiber rather than this
 * plugin's own `inject`, because a headless node composes no picker at all and
 * must not be held back by one.
 *
 * @param ctx - the plugin context.
 */
function warnOnUnreachableDirectoryPicker(ctx: Context): void {
  ctx.inject(['directoryPicker'], (child: Context) => {
    const picker = child.get('directoryPicker') as { capability?: () => { kind?: string } } | undefined
    const kind = picker?.capability?.().kind
    if (kind === undefined || kind === 'browse') return
    child.logger('fleet-node').warn(
      `directory picker is "${kind}": a remote browser cannot select a workspace on this node. `
      + 'Pin @deepseek-ai/dsh-host-directory-picker-browse on the `directory-picker` row to fix it.',
    )
  })
}

/**
 * Mount the uplink for this node.
 * @param ctx - the plugin context, with the web surface already bound.
 * @param config - validated configuration.
 */
export function apply(ctx: Context, config: Config): void {
  // Mounted before the connection is resolved, and left mounted either way:
  // its whole reason to exist is being reachable while this plugin is not
  // connected.
  ctx.effect(() => mountSetup(ctx, setupHooks(ctx, config)) ?? (() => {}), 'fleet-node.setup()')

  const settings = connection(config)
  if (settings === undefined) {
    // Loaded but idle, on purpose. Refusing to load would keep this plugin out
    // of Settings → Plugins, which is the one place someone can configure it —
    // an installed plugin that hides until it is configured cannot be.
    ctx.logger('fleet-node').info(
      'not connected: set url, nodeId and token in Settings → Plugins '
      + '(or DSH_FLEET_URL / DSH_FLEET_NODE_ID / DSH_FLEET_TOKEN). '
      + 'Mint a token on the control plane with `dshf node add <id>`.',
    )
    return
  }

  assertUsable({ ...config, ...settings })
  warnOnUnreachableDirectoryPicker(ctx)

  ctx.effect(() => {
    const uplink = new Uplink(ctx, {
      url: settings.url,
      nodeId: settings.nodeId,
      token: settings.token,
      username: settings.username === '' ? undefined : settings.username,
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
