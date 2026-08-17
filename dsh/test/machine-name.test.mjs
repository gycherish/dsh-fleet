// The machine names itself, so the reduction from hostname to id has to be
// right without anyone looking at it.
//
// The id lands in a URL (`/_fleet/select/<id>`) and in a primary key, so a
// hostname's dots, capitals, and domain suffix all have to go — and quietly
// producing two ids for one machine is the failure that would be hardest to
// notice.
import assert from 'node:assert/strict'
import { test } from 'node:test'

import { machineName, sanitiseName } from '../lib/index.js'

test('a plain hostname passes through lowercased', () => {
  assert.equal(sanitiseName('Laptop'), 'laptop')
  assert.equal(sanitiseName('devbox'), 'devbox')
})

test('a domain suffix is dropped, so one machine is one id', () => {
  assert.equal(sanitiseName('laptop.local'), 'laptop')
  assert.equal(sanitiseName('laptop.corp.example.com'), 'laptop')
  // Which is the point: these are the same machine.
  assert.equal(sanitiseName('LAPTOP.local'), sanitiseName('laptop'))
})

test('characters a URL or a key would fight are replaced', () => {
  assert.equal(sanitiseName('my_box'), 'my-box')
  assert.equal(sanitiseName('box (spare)'), 'box-spare')
  assert.equal(sanitiseName('WIN-ABC123'), 'win-abc123')
})

test('no leading or trailing dashes survive the replacement', () => {
  assert.equal(sanitiseName('_box_'), 'box')
  assert.equal(sanitiseName('---'), '')
  assert.equal(sanitiseName('中文主机'), '', 'nothing usable is honest about being nothing')
})

test('an absurd hostname is capped to one DNS label', () => {
  const name = sanitiseName('a'.repeat(200))
  assert.equal(name.length, 63)
})

test('this machine yields a usable id', () => {
  const name = machineName()
  assert.match(name, /^[a-z0-9-]+$/, `machineName() gave ${JSON.stringify(name)}`)
  assert.ok(name.length > 0 && name.length <= 63)
})
