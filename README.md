# dsh-fleet

One console for all your [DeepSeek Harness](https://github.com/deepseek-ai/deepseek-harness) (`dsh`) machines — reachable from your phone.

Each machine runs an ordinary `dsh web` plus one small plugin that dials **out** to the control plane. The control plane authenticates you, routes opaque frames, and serves each node's own frontend. Nodes need no inbound port and work behind NAT.

```
        phone / laptop browser
                 │ HTTPS + WS
                 ▼
        ┌──────────────────┐
        │  dshf  (Go)      │   auth · node registry · frame router
        │  PostgreSQL      │
        └──────────────────┘
                 ▲ WSS (node dials out)
      ┌──────────┼──────────┐
   ┌──┴──┐    ┌──┴──┐    ┌──┴──┐
   │ dsh │    │ dsh │    │ dsh │   + @…/dsh-fleet-node
   └─────┘    └─────┘    └─────┘
```

## Why not just expose `dsh web`

`dsh web` binds loopback on purpose. Its own docs are explicit:

> The fence is a reachability policy, **not authentication**; the Web carrier provides no authentication layer.

`dsh-fleet` supplies the missing layer and the multi-machine view, without forking dsh: the node plugin is an ordinary Cordis plugin over the transport-agnostic `ctx.apiProxy`, which dsh already designed for exactly this (`toFetchHandler` is its HTTP carrier; this is a second one).

## Status

Pre-alpha. The protocol in [`api/envelope.md`](api/envelope.md) is versioned but not yet stable.

## What works today

The uplink is live: a node authenticates, is tracked, reports telemetry, and
serves proxied requests. What is missing is the console itself — there is no
user authentication and no UI, so `DSHF_BIND` keeps the browser plane on
loopback and must stay there until that lands.

| | |
|---|---|
| Node registration, one-time tokens, revocation | ✅ `dshf node add/ls/revoke` |
| Uplink handshake, auth, duplicate refusal, heartbeat | ✅ |
| Telemetry ingest (history + denormalised latest) | ✅ |
| Request proxy with streaming and per-chunk flush | ✅ |
| Privilege gate over dsh's loopback-pinned methods | ✅ audited, deny by default |
| Console user accounts and sessions | ❌ |
| Frontend asset pass-through | ❌ |

## Development (local, no container)

pixi provides Node, pnpm, and PostgreSQL, so the inner loop never waits on a
daemon. Go is expected on `PATH` and is deliberately not managed by pixi.

```sh
pixi install

# once
pixi run pg-init
pixi run pg-start
pixi run pg-create

# the node plugin
pixi run typecheck          # or: pixi run build

# the control plane
cp .env.local.example .env.local     # then export it, see the file header
go run ./cmd/dshf serve
curl localhost:8080/healthz
```

`pg-start` / `pg-stop` are all you need afterwards. The cluster lives in
`.devdata/` on port **5433**, chosen so it never collides with a system
PostgreSQL, and `dshf serve` applies migrations at boot.

## Quick start (Docker — the supported deployment)

```sh
cp deploy/.env.example deploy/.env   # set DSHF_ADMIN_PASSWORD, POSTGRES_PASSWORD
docker compose -f deploy/docker-compose.yml up -d
```

Then register a node and mint its token:

```sh
docker compose -f deploy/docker-compose.yml exec dshf \
  dshf node add laptop --label "ThinkPad"
# prints: node id + one-time token
```

On the machine you want to control:

```sh
dsh plugin --profile web add @your-scope/dsh-fleet-node
```

and add to `$DSH_HOME/profiles/web/cordis.patch.yml`:

```yaml
- insert:
    - id: fleet-node
      name: '@your-scope/dsh-fleet-node'
      config:
        url: 'wss://fleet.example.com/uplink'
        nodeId: 'laptop'
        token: !!js process.env.DSH_FLEET_TOKEN
```

Then `DSH_FLEET_TOKEN=… dsh web` as usual. The node appears in the console.

## Layout

| Path | What |
|---|---|
| [`api/envelope.md`](api/envelope.md) | The wire contract. Single source of truth for both languages. |
| `cmd/dshf/` | Control-plane binary (CLI + `dshf serve`). |
| `internal/` | Control-plane implementation. |
| `pkg/envelope/` | Go mirror of the wire contract. |
| [`dsh/`](dsh/) | The dsh node plugin (TypeScript). |
| `deploy/` | Dockerfile, compose, migrations. |
| `pixi.toml` | Node, pnpm, and a local PostgreSQL for development. |

The plugin builds against a **local harness checkout**, expected beside this
repository as `../deepseek-harness` and already built. The published
`@deepseek-ai` set is currently stale — `dsh-host-apiproxy` resolves to
`0.0.1-rc.1`, whose own dependency `@deepseek-ai/dsh-user-interaction` is not
in the registry at all — so npm is not yet a usable build source. Runtime
resolution is unaffected: an installed profile gets these packages from the
dsh installation's own module farm, which is why they are peer dependencies.

## Two namespaces, one channel

The uplink carries two kinds of request, and the distinction is the whole design:

- **`dsh`** — tunnelled HTTP to the node's `/api` fetch handler. The control plane **never parses these bodies**; it moves bytes and correlates ids. This is why the control plane is version-independent from dsh.
- **`fleet`** — this project's own methods, implemented by the node plugin against ordinary Cordis services (`ctx.fs`, `ctx.loader`, `ctx.agents`). Node telemetry and file reads live here. This namespace is ours, so it does not move when dsh does.

## License

MIT
