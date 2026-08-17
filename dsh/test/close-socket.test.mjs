// `closeSocket` has one job: close a socket and never throw while doing it.
//
// It is called from close handlers and timers, where nothing can catch — a
// throw there becomes an unhandled rejection that ends the whole `dsh web`
// process. That happened: closing a bridge with 1001 killed the node on every
// uplink drop. These run against the built `lib/`, so `pnpm build` first.
import assert from 'node:assert/strict'
import { test } from 'node:test'

import { closeSocket } from '../lib/uplink.js'

/** Records what reached the underlying socket, and rejects illegal input the
 *  way a real WebSocket does. */
function fakeSocket() {
  const calls = []
  return {
    calls,
    close(code, reason) {
      // Mirrors the runtime check in validateCloseCodeAndReason.
      if (code !== 1000 && (code < 3000 || code > 4999)) {
        throw new DOMException('invalid code', 'InvalidAccessError')
      }
      if (new TextEncoder().encode(reason).length > 123) {
        throw new DOMException('reason too long', 'SyntaxError')
      }
      calls.push({ code, reason })
    },
  }
}

test('a legal code passes through untouched', () => {
  const socket = fakeSocket()
  closeSocket(socket, 4006, 'heartbeat lost')
  assert.deepEqual(socket.calls, [{ code: 4006, reason: 'heartbeat lost' }])
})

test('reserved codes are rewritten rather than thrown on', () => {
  // 1001 was the one that killed the process; 1006 is set by the runtime only.
  for (const reserved of [1001, 1006, 1005, 0, 999, 2999, 5000, -1]) {
    const socket = fakeSocket()
    closeSocket(socket, reserved, 'x')
    assert.deepEqual(socket.calls, [{ code: 1000, reason: 'x' }], `code ${reserved}`)
  }
})

test('the whole application range survives', () => {
  for (const code of [1000, 3000, 4001, 4006, 4999]) {
    const socket = fakeSocket()
    closeSocket(socket, code, '')
    assert.equal(socket.calls[0].code, code)
  }
})

test('an over-long reason is trimmed to the 123-byte ceiling', () => {
  const socket = fakeSocket()
  closeSocket(socket, 1000, 'a'.repeat(500))
  assert.equal(socket.calls.length, 1)
  assert.ok(new TextEncoder().encode(socket.calls[0].reason).length <= 123)
})

test('trimming multi-byte text does not leave a broken character', () => {
  const socket = fakeSocket()
  // 200 three-byte characters: the 123-byte cut lands mid-character.
  closeSocket(socket, 1000, '节'.repeat(200))
  const [{ reason }] = socket.calls
  assert.ok(new TextEncoder().encode(reason).length <= 123)
  assert.ok(!reason.includes('�'), 'a truncated character must be dropped, not mangled')
})

test('a socket that refuses anyway does not propagate', () => {
  const hostile = { close() { throw new Error('already closing') } }
  assert.doesNotThrow(() => { closeSocket(hostile, 1000, 'bye') })
})

test('no socket is a no-op', () => {
  assert.doesNotThrow(() => { closeSocket(undefined, 1000, 'bye') })
})
