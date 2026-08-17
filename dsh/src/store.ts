/**
 * Where the setup page keeps what it was told.
 *
 * The loader cannot persist this on its own. An entry mounted from a patch
 * overlay — which is how a bundled plugin arrives — has no writable config
 * file behind it: `entry.update()` reloads the plugin, so a save takes effect
 * immediately, and then the value is gone at the next start. That was measured,
 * not assumed: after a save the machine connected, and after a restart the page
 * read "not configured" again.
 *
 * So the page writes here as well. Small, boring, and entirely ours.
 *
 * @module @dsh-fleet/node/store
 */

import { mkdirSync, readFileSync, writeFileSync } from 'node:fs'
import { homedir } from 'node:os'
import { dirname, join } from 'node:path'

/**
 * dsh's user-data home.
 *
 * The variable name and the directory name are dsh's, restated rather than
 * imported: two constants are not worth a dependency, and they are part of its
 * documented surface. If dsh ever moves its home, this follows by hand.
 */
function dshHome(): string {
  const override = (process.env.DSH_HOME ?? '').trim()
  return override !== '' ? override : join(homedir(), '.dsh')
}

/** The file. One per dsh home, which is one per machine in every ordinary setup. */
function storePath(): string {
  return join(dshHome(), 'fleet-node.json')
}

/** The subset of configuration the setup page owns. */
export interface Saved {
  url?: string
  nodeId?: string
  username?: string
  token?: string
  label?: string
}

/**
 * Read what was saved, if anything.
 *
 * Never throws: an unreadable or corrupt store means "nothing saved", because
 * refusing to start over a stray file would take the machine down for a reason
 * nobody could see.
 */
export function readSaved(): Saved {
  try {
    const parsed: unknown = JSON.parse(readFileSync(storePath(), 'utf8'))
    if (typeof parsed !== 'object' || parsed === null) return {}
    const saved: Saved = {}
    for (const key of ['url', 'nodeId', 'username', 'token', 'label'] as const) {
      const value = (parsed as Record<string, unknown>)[key]
      if (typeof value === 'string') saved[key] = value
    }
    return saved
  } catch {
    return {}
  }
}

/**
 * Replace the store.
 *
 * Written with owner-only permission because it holds a token. The mode is a
 * no-op on Windows, where the file inherits the profile directory's ACL — the
 * same protection dsh's own credentials get.
 *
 * @throws when the file cannot be written, which the page reports rather than
 *   claiming a save that did not happen.
 */
export function writeSaved(saved: Saved): void {
  const path = storePath()
  mkdirSync(dirname(path), { recursive: true })
  writeFileSync(path, JSON.stringify(saved, null, 2) + '\n', { encoding: 'utf8', mode: 0o600 })
}
