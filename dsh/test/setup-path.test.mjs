// The setup page must never be forwarded over the uplink.
//
// It can change which control plane this machine answers to, so reaching it
// through the current one would let whoever is signed in there hand the machine
// to a different fleet. Both a local request and a forwarded one arrive at
// loopback, so this path filter is the only thing that separates them — which
// makes an off-by-one here a security hole rather than a cosmetic bug.
import assert from 'node:assert/strict'
import { test } from 'node:test'

import { isSetupPath } from '../lib/uplink.js'
import { SETUP_PATH } from '../lib/setup.js'

test('the page and everything under it is refused', () => {
  assert.ok(isSetupPath(SETUP_PATH))
  assert.ok(isSetupPath(`${SETUP_PATH}/save`))
  assert.ok(isSetupPath(`${SETUP_PATH}/anything/deeper`))
})

test('a query string does not slip past it', () => {
  assert.ok(isSetupPath(`${SETUP_PATH}?saved`))
  assert.ok(isSetupPath(`${SETUP_PATH}/save?x=1`))
})

test('a trailing slash does not slip past it', () => {
  assert.ok(isSetupPath(`${SETUP_PATH}/`))
  assert.ok(isSetupPath(`${SETUP_PATH}///`))
  assert.ok(isSetupPath(`${SETUP_PATH}/?saved`))
})

test('ordinary dsh paths are still forwarded', () => {
  for (const path of ['/', '/api/session.list', '/assets/index-abc.js', '/plugins/x/client.js']) {
    assert.equal(isSetupPath(path), false, path)
  }
})

test('a path that merely starts with the same letters is not the page', () => {
  // `/_dshf-setupx` is a different route, and refusing it would be a silent
  // hole in the proxied surface rather than a protection.
  assert.equal(isSetupPath(`${SETUP_PATH}x`), false)
  assert.equal(isSetupPath(`${SETUP_PATH}-other`), false)
})
