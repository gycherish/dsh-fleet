# dsh-fleet uplink protocol

**Protocol version: 1**

The single source of truth for the node ⇄ control-plane channel. `pkg/envelope` (Go) and `dsh/src/protocol.ts` (TypeScript) both derive from this document; when they disagree, this document is right.

## Transport

One WebSocket per node, **dialled by the node**. The control plane never connects to a node, so a node needs no inbound port, no public address, and no NAT traversal.

- URL: deployment-configured, e.g. `wss://fleet.example.com/uplink`
- Sub-protocol: `dshf.v1`
- Authentication is **in-band**, in the first frame (see [`hello`](#hello-node--control-plane)), not a handshake header.
- Every frame is one WebSocket **text** message containing one compact JSON object.
- The control plane closes a socket that sends a frame it cannot parse. It never guesses.

Binary frames are unused: payload bytes ride inside JSON (see [Payload encoding](#payload-encoding)) so one framing rule covers text and binary alike.

## Frame envelope

Every frame carries a discriminant `t`. Unknown `t` values are **ignored with a warning**, never fatal — this is the only forward-compatibility affordance in the protocol, and it exists so a newer node can add telemetry fields to an older control plane without dropping the connection.

```
Frame =
  │ node → control plane
  ├─ hello     first frame after connect; identity + capabilities
  ├─ head      response status line for one request
  ├─ data      one response body chunk
  ├─ end       response body complete
  ├─ err       request failed; terminal, no end follows
  ├─ tlm       unsolicited node telemetry snapshot
  ├─ ws.up     a bridged socket connected
  └─ pong
  │ control plane → node
  ├─ welcome   accepted; carries negotiated runtime parameters
  ├─ req       one request
  ├─ cancel    abort an in-flight request
  ├─ ws.open   dial a socket on the node's own server
  └─ ping
  │ either direction
  ├─ ws.msg    one message on a bridged socket
  └─ ws.close  either end hung up
```

## Handshake

### `hello` (node → control plane)

First frame on every connection, including reconnects. It carries a credential.

The credential rides in-band rather than in an `Authorization` header because the WHATWG `WebSocket` constructor cannot set request headers, and the plugin uses Node's built-in client to avoid a dependency. Putting it in the URL was the other option and is worse: query strings reach proxy access logs.

The consequence is that a socket is anonymous between `open` and `hello`. The control plane gives it **five seconds** to authenticate, accepts no other frame type in that window, and closes with `4005` when the deadline passes.

```json
{
  "t": "hello",
  "protocol": 1,
  "nodeId": "laptop",
  "username": "admin",
  "token": "ut_…",
  "node": {
    "label": "ThinkPad X1",
    "platform": "win32",
    "arch": "x64",
    "dshVersion": "0.1.0-rc.7",
    "pluginVersion": "0.1.0",
    "cwd": "D:/repo"
  },
  "caps": ["dsh", "fleet.telemetry"]
}
```

**`username` selects which kind of credential `token` is**, and it is the only optional field here.

| `username` | `token` | Meaning |
|---|---|---|
| present | `ut_…` | A person's token. The control plane registers `nodeId` to that account on first connection, so the machine enrols itself. |
| absent | `nt_…` | The machine's own token, from `dshf node add`. The machine must already be registered. |

Two kinds because they suit different situations: a container is handed a machine token and never opens a UI, while a person types their own username and token into a plugin and expects the machine to appear without a second step on the control plane.

A machine holds exactly one kind. Presenting a username for a `nodeId` already registered with a machine token is refused, and so is claiming one that belongs to another account — those answer `4002` with different reasons, because the fix differs: use that machine's own token, versus pick a different name.

Revoking a user token disconnects every machine enrolled with it at each machine's next handshake. That blast radius is the reason the two prefixes are distinct on sight.

`caps` declares which namespaces and optional `fleet` families this node serves. A control plane must not send a `req` for a capability the node did not declare; a node answers one it never declared with `err` / `unsupported`.

### `welcome` (control plane → node)

```json
{
  "t": "welcome",
  "protocol": 1,
  "heartbeatMs": 20000,
  "maxChunkBytes": 262144,
  "telemetryIntervalMs": 30000
}
```

The node adopts these values for the lifetime of the connection. A control plane that rejects the node closes with a WebSocket close code instead:

| Code | Meaning |
|---|---|
| `4001` | bad or revoked token |
| `4002` | unknown `nodeId`, wrong token, or a machine name this account may not claim |
| `4003` | protocol version unsupported |
| `4004` | this `nodeId` already has a live connection |
| `4005` | no `hello` within the authentication deadline |

`4004` is deliberately not a takeover: a flapping node must not repeatedly evict a healthy one. The newcomer backs off and retries.

One code travels the other way. The node closes with `4006` when two heartbeats go unanswered — the socket is open but the peer is gone, and TCP will not say so for minutes. It is not a rejection: the node reconnects on its normal backoff, and the control plane should log it rather than hold the disconnect against the node.

| Code | Meaning |
|---|---|
| `4006` | node lost the heartbeat and is reconnecting |

Both directions must stay inside `1000` or `3000`–`4999`. Those are the only codes a WebSocket application may send; a browser or Node runtime throws on anything else, `1001` and `1006` included, which is why the reserved-sounding codes never appear on this wire.

## Requests

### `req` (control plane → node)

```json
{
  "t": "req",
  "id": "01J9…",
  "ns": "dsh",
  "method": "POST",
  "path": "/api/session.prompt",
  "headers": { "content-type": "application/json" },
  "body": { "enc": "u", "d": "{\"rpcId\":…}" }
}
```

| Field | Meaning |
|---|---|
| `id` | Correlation id, minted by the control plane, unique per connection. |
| `ns` | `"dsh"` or `"fleet"`. |
| `method` | HTTP verb. `dsh` uses `GET`, `HEAD`, `POST`; `fleet` always `POST`. |
| `path` | For `dsh`, the exact `/api/…` path **including query string**. For `fleet`, a method name. |
| `headers` | Optional. Lower-cased names. |
| `body` | Optional. See [Payload encoding](#payload-encoding). |

**`ns: "dsh"` handling is mechanical**: the node replays the request against **its own `dsh web` HTTP server** over loopback. It does not inspect `path`, does not parse `body`, and does not know which dsh methods exist. That is what keeps the control plane and this plugin independent of the dsh version.

Replayed against the server rather than handed to `ctx.apiProxy` directly, because `/api` is a composite handler: the Typert gateway claims its Remote endpoints and only the rest fall through to the proxy. A carrier wired straight to the proxy silently loses that surface — which it did, until the missing endpoints showed up as 404s in the browser. One loopback hop buys the complete surface, plus SPA fallback, the boot manifest injected into `index.html`, and `/plugins/<id>/client.js`, none of which the proxy serves.

The node refuses exactly one path family: its own setup page. That page can change which control plane the machine answers to, and both a local request and a forwarded one arrive at the server as loopback, so the node is the only place that can tell them apart. A `req` for it answers `err` / `denied`.

**`ns: "fleet"` handling is ours**: the node dispatches on `path` against its own method table.

That table is currently **empty**, and the namespace is kept as the seam rather than the feature. It held one family, `fleet.file.*`, for reading and listing files on a node under a configured root allowlist. It was removed after the proxied dsh UI turned out to deliver the same thing — a machine's files and workspaces, through that machine's own interface — leaving ours advertised in `caps`, implemented, and called by nobody. Anything asked of `fleet` today answers `err` / `unsupported`.

### `cancel` (control plane → node)

```json
{ "t": "cancel", "id": "01J9…" }
```

Aborts the `AbortSignal` of that request. The node still answers — with `err` / `cancelled` — so the control plane can free its correlation slot on one code path. A `cancel` for an unknown or settled id is a no-op.

## Responses

Every request produces exactly one of:

- `head` → zero or more `data` → `end`
- `err` (terminal on its own)

### `head`

```json
{
  "t": "head",
  "id": "01J9…",
  "status": 200,
  "headers": { "content-type": "text/event-stream" }
}
```

### `data`

```json
{ "t": "data", "id": "01J9…", "seq": 0, "body": { "enc": "u", "d": "event: mux\ndata: …\n\n" } }
```

`seq` starts at 0 and increments per request. The control plane **must** reject an out-of-order or duplicated `seq` by closing the connection: silent reordering of an SSE stream would corrupt the browser's event sequence in ways that surface much later as UI desync.

A chunk's payload never exceeds `maxChunkBytes` after encoding.

### `end`

```json
{ "t": "end", "id": "01J9…", "chunks": 42 }
```

`chunks` is the total `data` count, so the control plane can assert it received them all.

### `err`

```json
{ "t": "err", "id": "01J9…", "code": "unsupported", "message": "no such fleet method" }
```

| `code` | When |
|---|---|
| `cancelled` | answered a `cancel`, or the node is shutting down |
| `unsupported` | unknown namespace, or a `fleet` method this node does not serve |
| `unavailable` | the required Cordis service is not composed on this node |
| `denied` | the node's own policy refused (e.g. path outside the sandbox root) |
| `internal` | anything else; `message` is diagnostic text, never a payload |

`err` after `head` is legal: a stream can fail mid-body. The control plane then terminates the browser response however its carrier allows.

## Bridged sockets

dsh's event downlinks — `/api/events.mux` and `/api/events.host` — are **WebSocket upgrades, not streamed responses**. A plain `GET` answers `426` with no fallback, so a control plane that forwards only `req`/`head`/`data` produces a UI that loads, renders once, and then never updates: the worst kind of broken, because it looks fine.

So an upgrade gets its own frame family. Two hops, because neither end can reach the other directly: the browser talks only to the control plane, and the node accepts no inbound connections at all.

```
browser ──upgrade──▶ control plane ──ws.open──▶ node ──dial──▶ node's own server
        ◀──messages──               ◀──ws.msg──      ◀──messages──
```

`ws.open` and `ws.close` correlate by `id` exactly as requests do, drawn from the same per-connection id space.

### `ws.open` (control plane → node)

```json
{ "t": "ws.open", "id": "w1", "path": "/api/events.mux", "headers": {} }
```

The node dials that path on its own server, switching the scheme to `ws:`/`wss:`. A failure to open answers `err`, so the control plane can still refuse the browser's upgrade with an HTTP status it can read.

### `ws.up` (node → control plane)

```json
{ "t": "ws.up", "id": "w1", "protocol": "" }
```

Sent once the far socket is open. The control plane accepts the browser's upgrade only after this, so a failure is still an HTTP status rather than an immediate close. `protocol` carries the negotiated sub-protocol when there is one.

### `ws.msg` (either direction)

```json
{ "t": "ws.msg", "id": "w1", "body": { "enc": "u", "d": "event: mux\ndata: …" } }
```

One message, same `body` encoding as everywhere else. The downlinks are declared downlink-only, but the frame is defined in both directions so a later endpoint that talks back needs no protocol change.

### `ws.close` (either direction)

```json
{ "t": "ws.close", "id": "w1", "code": 1000, "reason": "" }
```

Either end hanging up. `code` must be `1000` or `3000`–`4999` like every other close on this wire; a receiver **sanitises rather than trusts it**, because passing a peer-supplied code to `WebSocket.close()` throws on anything else — which would otherwise make the field a remote kill switch.

Correlation ids are per-connection, so every bridge dies with the uplink that addressed it. The node closes its bridged sockets when the uplink drops, and does not try to re-establish them: the browser reopens its own downlink after the control plane comes back.

## Payload encoding

```json
{ "enc": "u", "d": "…" }   // d is the UTF-8 text itself
{ "enc": "b", "d": "…" }   // d is standard base64 of the raw bytes
```

The node picks `u` when the bytes are valid UTF-8 **and** the response content type is textual (`application/json`, `text/event-stream`, `text/*`), otherwise `b`. The control plane does not care which: it decodes to bytes and forwards.

`u` exists because the hot path — `events.mux` carrying every assistant token — would otherwise pay base64's 33% inflation on every chunk.

## Backpressure

A node must not let an unbounded response outrun the socket. Before writing each `data` frame it checks the socket's buffered amount against `maxChunkBytes × 8`; above that it pauses reading the response body until the buffer drains. `session.export` (a whole ZIP) is the case this exists for.

## Telemetry

### `tlm` (node → control plane, unsolicited)

Sent once after `welcome` and then every `telemetryIntervalMs`. This is **not** part of the `dsh` namespace: it reads Cordis services directly, so it exposes facts the `/api` surface has no method for.

```json
{
  "t": "tlm",
  "ts": 1755300000000,
  "snapshot": {
    "dshVersion": "0.1.0-rc.7",
    "uptimeMs": 812000,
    "entries": [
      { "id": "tool-bash", "name": "@deepseek-ai/dsh-tool-bash", "enabled": true, "phase": "active" }
    ],
    "agents": { "total": 3, "running": 1 },
    "workspaces": ["D:/repo/dsh-plugin"],
    "tools": 24
  }
}
```

`entries` comes from `ctx.loader.entries()` — the whole composed plugin tree with each row's live fiber phase (`pending`/`loading`/`active`/`failed`/`unloading`). A node whose `tool-bash` row sits in `failed` is visible in the console before anyone opens a session on it.

The control plane stores snapshots verbatim as `jsonb`. It **must not** require any particular field: a newer node adds keys freely.

## Heartbeat

The control plane sends `ping` every `heartbeatMs`; the node answers `pong` echoing `ts`. A node that misses two consecutive pings is closed and marked offline. The node applies the same rule in reverse and reconnects.

## Reconnect

The node reconnects with exponential backoff and full jitter: `min(cap, base × 2^n) × random(0.5, 1.0)`, defaults `base = 1s`, `cap = 30s`, unlimited attempts.

On reconnect the node sends a fresh `hello`. **Every in-flight request from the previous connection is dead**: correlation ids are per-connection, so the control plane fails all pending browser calls for that node rather than hoping a reply arrives. Browsers retry at the application layer, which the dsh client already does for its own connection generations.

## What the control plane must parse

Exactly these fields: `t`, `id`, `seq`, `ns`, `status`, `code`, `nodeId`, `username`, `token`, `protocol`, `caps`, and the `enc`/`d` pair. Everything else — request bodies, response bodies, telemetry snapshots, bridged socket messages — is opaque and forwarded or stored as received.

Bridged sockets do not widen this. A `ws.msg` body is relayed byte for byte; the control plane never learns that `events.mux` carries assistant tokens, only that some socket had something to say.

This list is the load-bearing constraint of the whole project. Any change that requires the control plane to understand a dsh business payload should be treated as a design error until proven otherwise.

## Known gaps

- **No resume across reconnect.** A dropped socket fails in-flight requests. Making `session.export` resumable would need range support the dsh endpoint does not offer.
- **One connection per node.** `4004` refuses a second; there is no failover pair.
- **No compression.** WebSocket `permessage-deflate` is left to the deployment's proxy.
- **`fleet` methods are unversioned within protocol 1.** They are ours, so a breaking change bumps the protocol version rather than negotiating per method.
