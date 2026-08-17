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

Every node's agent is **genuinely local** — git, builds, tests, and language servers all run on that machine. The control plane forwards; it does not execute on anyone's behalf. What the browser loads is that node's own dsh UI, served through the uplink.

## Why not just expose `dsh web`

Because it has no authentication. dsh's own documentation is explicit:

> The fence is a reachability policy, **not authentication**; the Web carrier provides no authentication layer.

`dsh-fleet` supplies that layer, and the multi-machine view along with it. It does not fork dsh: the node plugin is an ordinary Cordis plugin over the gateway dsh already designed to be transport-agnostic.

Pre-alpha, and no rate limiting yet.

## HTTPS is required, not recommended

Browsers gate `crypto.randomUUID` and the rest of the secure-context APIs on HTTPS, exempting only loopback. The dsh client calls them, so a console reached at `http://192.168.x.x` serves a UI whose settings pages fail outright — while the same build works from the host over `127.0.0.1`, which is how this gets missed.

Terminating TLS at a reverse proxy is the recommended arrangement; dshf then needs no certificate of its own. [`deploy/Caddyfile.example`](deploy/Caddyfile.example) is verified end to end, including the node uplink and both event downlinks. [`deploy/nginx.conf.example`](deploy/nginx.conf.example) covers the same ground and is not.

```caddy
fleet.example.com {
	reverse_proxy 127.0.0.1:8080 {
		transport http { read_timeout 0 }
	}
}
```

Set `DSHF_PUBLIC_URL` to the address the browser types — it decides cookie scope and the Secure flag — and leave `DSHF_LISTEN` on loopback.

For a LAN with no public name, `dshf cert` mints a self-signed certificate covering this host's addresses and `DSHF_TLS_CERT` / `DSHF_TLS_KEY` serve with it. A phone shows a warning once; accepting it is enough to make the origin secure.

## Quick start

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

Also available: `dshf node ls`, `dshf node revoke <id>`, `dshf user add <name>`.

## Connecting a machine

Install the plugin on the machine you want to reach, then set three environment variables and start `dsh web` as usual:

```sh
dsh plugin --profile web add <this package>

export DSH_FLEET_URL=wss://fleet.example.com/uplink
export DSH_FLEET_NODE_ID=laptop
export DSH_FLEET_TOKEN=nt_...

dsh web
```

Without `DSH_FLEET_URL` the plugin stays inert, so installing it never changes how `dsh web` already behaves.

Opening a machine from the chooser hands it the origin root, because the dsh client addresses `/api` and its assets absolutely and nothing else can work. One browser therefore drives one machine at a time, and **`/_fleet/` is the way back** to the chooser — worth a bookmark on a phone.

> If you plan to drive a machine from a phone, pin its directory picker to browse mode; the native picker can only be clicked on that machine's own desktop. The plugin's config layer ships the one-line override.

## Development

pixi provides Node, pnpm, and a local PostgreSQL, so the inner loop needs no container. Go is your own installation.

```sh
pixi install
pixi run pg-init && pixi run pg-start && pixi run pg-create   # once
pixi run typecheck                                            # the node plugin
go run ./cmd/dshf serve                                       # the control plane
```

Copy `.env.local.example` to `.env.local` and export it first; the file header shows how. Afterwards only `pixi run pg-start` / `pg-stop` are needed.

The node plugin builds **against a local harness checkout**, expected as `deepseek-harness` beside this repository and already built. The published `@deepseek-ai` packages are incomplete for now and cannot serve as a build source.

## Design note

The control plane **never parses dsh business data**. It forwards opaque frames, correlates request ids, and applies its own access policy. Because it does not understand dsh's API, a dsh upgrade does not require a control-plane release.

A second `fleet` namespace carries this project's own methods — node telemetry, file browsing — implemented by the plugin directly against Cordis services. That half is ours, so it does not move when dsh does.

The wire contract is [`docs/envelope.md`](docs/envelope.md).

## License

[MIT](LICENSE)
