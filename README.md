# dsh-fleet

English | [中文](README.zh.md)

One console for every machine you run [DeepSeek Harness](https://github.com/deepseek-ai/deepseek-harness) (`dsh`) on — usable from your phone.

Each machine runs an ordinary `dsh web` plus a small plugin that dials **out** to the control plane. The control plane handles authentication, routing, and sessions. Nodes need no inbound port and work behind NAT.

```
        phone / desktop browser
                 │ HTTPS + WS
                 ▼
        ┌──────────────────┐
        │  dshf  (Go)      │   auth · node registry · frame routing
        │  PostgreSQL      │
        └──────────────────┘
                 ▲ WSS (the node dials out)
      ┌──────────┼──────────┐
   ┌──┴──┐    ┌──┴──┐    ┌──┴──┐
   │ dsh │    │ dsh │    │ dsh │   + the dsh-fleet node plugin
   └─────┘    └─────┘    └─────┘
```

Every node's agent is **genuinely local** — git, builds, tests, and language servers all run on that machine. The control plane forwards; it does not execute on anyone's behalf.

## Why not just expose `dsh web`

Because it has no authentication. dsh's own documentation is explicit:

> The fence is a reachability policy, **not authentication**; the Web carrier provides no authentication layer.

`dsh-fleet` supplies that layer, and the multi-machine view along with it. It does not fork dsh: the node plugin is an ordinary Cordis plugin over the gateway dsh already designed to be transport-agnostic.

## Status

**Pre-alpha.** The node path is built and verified end to end. The console itself is not.

| Capability | State |
|---|---|
| Node registration, one-time tokens, revocation | ✅ |
| Uplink handshake, authentication, heartbeat, reconnect | ✅ |
| Node telemetry (versions, plugin tree, agent counts) | ✅ |
| Request forwarding, including streaming and approvals | ✅ |
| Privileged-method gate with an audit trail | ✅ deny by default |
| Console accounts, sessions, machine chooser | ✅ |
| Frontend pass-through — the node's own UI, end to end | ✅ |

Verified against a real `dsh web`: the browser loads that node's own frontend byte for byte, its assets and client plugin bundles resolve, `/api` calls answer, and both SSE downlinks stream.

There is still no TLS and no rate limiting, so put a terminating proxy in front before exposing this anywhere. `DSHF_BIND` keeps it on loopback until you do.

## Quick start (Docker)

Docker is the supported deployment.

```sh
cp deploy/.env.example deploy/.env    # change both passwords
docker compose -f deploy/docker-compose.yml up -d
```

Register a machine and mint its one-time token:

```sh
docker compose -f deploy/docker-compose.yml exec dshf \
  dshf node add laptop --label "My laptop"
```

```
registered node "laptop"

  DSH_FLEET_NODE_ID=laptop
  DSH_FLEET_TOKEN=nt_...

This token is shown once.
```

Also available: `dshf node ls` for status, `dshf node revoke <id>` to withdraw a token.

## Connecting a machine

Install the plugin on the machine you want to reach:

```sh
dsh plugin --profile web add <this package>
```

Set three environment variables and start `dsh web` as usual:

```sh
export DSH_FLEET_URL=wss://fleet.example.com/uplink
export DSH_FLEET_NODE_ID=laptop
export DSH_FLEET_TOKEN=nt_...

dsh web
```

Without `DSH_FLEET_URL` the plugin stays inert, so installing it never changes how `dsh web` already behaves.

The plugin serves `/api` from the in-process gateway and everything else from this node's own web server (`localWebUrl`, default `http://127.0.0.1:3080`). That is why the browser gets the node's real frontend — boot manifest and client plugin bundles included — rather than an approximation of it.

Opening a machine from the chooser hands it the origin root, because the dsh client addresses `/api` and its assets absolutely and nothing else can work. One browser therefore drives one machine at a time, and switching is a page load rather than an in-app gesture.

> If you plan to drive this machine from a phone, pin its directory picker to browse mode — the native picker can only be clicked on that machine's own desktop. The plugin's config layer ships the one-line override with a comment explaining it.

## Development

pixi provides Node, pnpm, and a local PostgreSQL, so the inner loop needs no container. Go is your own installation.

```sh
pixi install

# once
pixi run pg-init && pixi run pg-start && pixi run pg-create

# the node plugin
pixi run typecheck

# the control plane
cp .env.local.example .env.local     # export it; see the file header
go run ./cmd/dshf serve
curl localhost:8080/healthz
```

Afterwards only `pixi run pg-start` / `pg-stop` are needed. The cluster lives in `.devdata/` on port 5433, chosen so it never collides with a system PostgreSQL. Migrations run when `dshf serve` boots.

The node plugin currently builds **against a local harness checkout**, expected as `deepseek-harness` beside this repository and already built with `pnpm run build`. The published `@deepseek-ai` packages are incomplete for now and cannot serve as a build source.

## Layout

| Path | Contents |
|---|---|
| [`docs/envelope.md`](docs/envelope.md) | The wire protocol; single source of truth for both languages |
| `cmd/dshf/` | Control-plane binary (daemon and operator CLI in one) |
| `internal/` | Control-plane implementation |
| `pkg/envelope/` | Go types for the protocol |
| [`dsh/`](dsh/) | The dsh node plugin (TypeScript) |
| `deploy/` | Dockerfile, compose, database migrations |

## Design note

The control plane **never parses dsh business data**. It forwards opaque frames, correlates request ids, and applies its own access policy. That restraint is deliberate: because it does not understand dsh's API, a dsh upgrade does not require a control-plane release.

The protocol carries a second `fleet` namespace for this project's own methods — node telemetry, file browsing — implemented by the plugin directly against Cordis services. That half is ours, so it does not move when dsh does.

Details in [`docs/envelope.md`](docs/envelope.md).

## License

[MIT](LICENSE)
