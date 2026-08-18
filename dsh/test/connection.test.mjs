// Which settings bring the uplink up, and which leave it deliberately down.
//
// The plugin loads unconfigured on purpose, so that it shows up in Settings →
// Plugins where someone can fill it in. That makes "configured enough to
// connect" a real decision with a wrong answer on either side: connect with a
// half-filled form and the node fails obscurely, refuse a fully-set
// environment and a container never joins the fleet.
import assert from 'node:assert/strict'
import { test } from 'node:test'

import { Config } from '../lib/index.js'

/** Validate through the real schema, so defaults land the way they do at load. */
const validate = input => new Config(input)

const ENV = ['DSH_FLEET_URL', 'DSH_FLEET_NODE_ID', 'DSH_FLEET_TOKEN']
function withEnv(values, body) {
  const saved = ENV.map(key => [key, process.env[key]])
  try {
    for (const key of ENV) delete process.env[key]
    Object.assign(process.env, values)
    body()
  } finally {
    for (const key of ENV) delete process.env[key]
    for (const [key, value] of saved) if (value !== undefined) process.env[key] = value
  }
}

test('an empty config validates rather than throwing', () => {
  // If this threw, the plugin could not load, so it could not be configured.
  const config = validate({})
  assert.equal(config.url, '')
  assert.equal(config.nodeId, '')
  assert.equal(config.token, '')
})

test('the connection fields are optional but the tuning still has defaults', () => {
  const config = validate({})
  assert.equal(config.localWebUrl, 'http://127.0.0.1:3080')
  assert.equal(config.reconnectBaseMs, 1_000)
  assert.equal(config.reconnectMaxMs, 30_000)
})

test('the token is marked secret so a settings form masks it', () => {
  // The role travels on the schema, which is what the form reads.
  assert.equal(Config.dict.token.meta.role, 'secret', 'token must carry role=secret')
})

test('every field carries a description, because the form is the only manual', () => {
  for (const [key, schema] of Object.entries(Config.dict)) {
    assert.ok(
      typeof schema.meta.description === 'string' && schema.meta.description.length > 0,
      `${key} needs a description for the settings form`,
    )
  }
})

test('a partial config is still not a connection', () => {
  withEnv({}, () => {
    // url alone, nodeId alone, and url+nodeId all mean "not configured".
    for (const partial of [{ url: 'wss://x/uplink' }, { nodeId: 'devbox' },
      { url: 'wss://x/uplink', nodeId: 'devbox' }]) {
      const config = validate(partial)
      const filled = ['url', 'nodeId', 'token'].filter(key => config[key] !== '')
      assert.ok(filled.length < 3, `${JSON.stringify(partial)} must not read as complete`)
    }
  })
})
