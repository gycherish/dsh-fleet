// The setup page's store, which is the only thing that makes a save survive.
//
// The loader cannot persist a patch-mounted entry's config, so without this
// file a save would take effect and then vanish at the next start — which is
// exactly what happened before it existed.
import assert from 'node:assert/strict'
import { mkdtempSync, writeFileSync, mkdirSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { test } from 'node:test'

import { readSaved, writeSaved } from '../lib/store.js'

/** Point DSH_HOME at a throwaway directory for one body. */
function inHome(body) {
  const previous = process.env.DSH_HOME
  const home = mkdtempSync(join(tmpdir(), 'dshf-store-'))
  process.env.DSH_HOME = home
  try {
    body(home)
  } finally {
    if (previous === undefined) delete process.env.DSH_HOME
    else process.env.DSH_HOME = previous
    rmSync(home, { recursive: true, force: true })
  }
}

test('what is written comes back', () => {
  inHome(() => {
    writeSaved({ url: 'wss://x/uplink', nodeId: 'box', username: 'admin', token: 'ut_abc', label: 'Box' })
    assert.deepEqual(readSaved(), {
      url: 'wss://x/uplink', nodeId: 'box', username: 'admin', token: 'ut_abc', label: 'Box',
    })
  })
})

test('nothing saved reads as nothing, not as a failure', () => {
  inHome(() => { assert.deepEqual(readSaved(), {}) })
})

test('a corrupt store reads as nothing rather than taking the node down', () => {
  // Refusing to start over a stray file would strand the machine for a reason
  // nobody could see from the outside.
  inHome(home => {
    mkdirSync(home, { recursive: true })
    writeFileSync(join(home, 'fleet-node.json'), '{ this is not json')
    assert.deepEqual(readSaved(), {})
  })
})

test('a store holding the wrong types keeps only the strings', () => {
  inHome(home => {
    mkdirSync(home, { recursive: true })
    writeFileSync(join(home, 'fleet-node.json'), JSON.stringify({ url: 'wss://x/uplink', nodeId: 42, token: null }))
    assert.deepEqual(readSaved(), { url: 'wss://x/uplink' })
  })
})

test('a later save replaces the earlier one entirely', () => {
  inHome(() => {
    writeSaved({ url: 'wss://one/uplink', token: 'ut_one', username: 'a' })
    writeSaved({ url: 'wss://two/uplink', token: 'ut_two' })
    const saved = readSaved()
    assert.equal(saved.url, 'wss://two/uplink')
    assert.equal(saved.token, 'ut_two')
    // Not merged: the page always submits the whole form, so a stale username
    // surviving a save would be a value nobody chose.
    assert.equal(saved.username, undefined)
  })
})

test('DSH_HOME is honoured, so a relocated dsh home keeps its own store', () => {
  inHome(home => {
    writeSaved({ url: 'wss://here/uplink' })
    assert.equal(readSaved().url, 'wss://here/uplink')
    // A different home sees nothing.
    const other = mkdtempSync(join(tmpdir(), 'dshf-other-'))
    process.env.DSH_HOME = other
    assert.deepEqual(readSaved(), {})
    process.env.DSH_HOME = home
    rmSync(other, { recursive: true, force: true })
  })
})
