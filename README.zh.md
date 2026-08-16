# dsh-fleet

[English](README.md) | 中文

用一个控制台管理你所有跑着 [DeepSeek Harness](https://github.com/deepseek-ai/deepseek-harness)（`dsh`）的机器——手机上也能用。

每台机器照常跑 `dsh web`，外加一个小插件**主动拨出**连到控制面。控制面负责认证、路由和会话管理。节点不需要开放任何入站端口，在 NAT 后面也能用。

```
        手机 / 电脑浏览器
                 │ HTTPS + WS
                 ▼
        ┌──────────────────┐
        │  dshf  (Go)      │   认证 · 节点注册表 · 帧路由
        │  PostgreSQL      │
        └──────────────────┘
                 ▲ WSS（节点主动拨出）
      ┌──────────┼──────────┐
   ┌──┴──┐    ┌──┴──┐    ┌──┴──┐
   │ dsh │    │ dsh │    │ dsh │   + dsh-fleet 节点插件
   └─────┘    └─────┘    └─────┘
```

每个节点上的 agent 都是**真正本地**的——git、编译、测试、语言服务器全都跑在那台机器上。控制面只做转发，不代替谁执行。

## 为什么不直接把 `dsh web` 暴露出去

因为它没有认证。dsh 自己的文档写得很直白：

> 这道围栏是可达性策略，**不是认证**；Web 载体不提供任何认证层。

`dsh-fleet` 补上这一层，并顺带给你多机视图。它不 fork dsh——节点插件是一个普通的 Cordis 插件，挂在 dsh 本来就设计成传输无关的网关上。

## 当前状态

**Pre-alpha。** 节点接入这条链路已经打通并端到端验证过；控制台本身还没有。

| 能力 | 状态 |
|---|---|
| 节点注册、一次性 token、吊销 | ✅ |
| 节点接入、认证、心跳、重连 | ✅ |
| 节点遥测（版本、插件树、agent 数） | ✅ |
| 请求转发（含流式与审批往返） | ✅ |
| 特权方法拦截 + 审计 | ✅ 默认拒绝 |
| 控制台账号、会话、机器选择页 | ✅ |
| 前端透传——节点自己的界面，端到端 | ✅ |

已对着真实的 `dsh web` 验证过：浏览器拿到的是该节点自己的前端（逐字节一致），静态资源与客户端插件包都能解析，`/api` 调用有响应，两条 SSE 下行流也正常。

但还没有 TLS，也没有限流。对外暴露前请在前面放一个终止 TLS 的反向代理；在那之前 `DSHF_BIND` 会把它锁在 loopback 上。

## 快速开始（Docker）

Docker 是推荐的部署方式。

```sh
cp deploy/.env.example deploy/.env    # 改掉两个密码
docker compose -f deploy/docker-compose.yml up -d
```

注册一台机器，拿到它的一次性 token：

```sh
docker compose -f deploy/docker-compose.yml exec dshf \
  dshf node add laptop --label "我的笔记本"
```

```
registered node "laptop"

  DSH_FLEET_NODE_ID=laptop
  DSH_FLEET_TOKEN=nt_...

This token is shown once.
```

其他命令：`dshf node ls` 看状态，`dshf node revoke <id>` 吊销。

## 接入一台机器

在要被管理的机器上装插件：

```sh
dsh plugin --profile web add <这个包>
```

设三个环境变量，照常启动 `dsh web`：

```sh
export DSH_FLEET_URL=wss://fleet.example.com/uplink
export DSH_FLEET_NODE_ID=laptop
export DSH_FLEET_TOKEN=nt_...

dsh web
```

没有配置 `DSH_FLEET_URL` 时插件完全不生效，所以装上它不会改变 `dsh web` 原本的行为。

插件把 `/api` 交给进程内的网关，其余路径交给这台机器自己的 web 服务器（`localWebUrl`，默认 `http://127.0.0.1:3080`）。所以浏览器拿到的是节点**真正的**前端，连 boot manifest 和客户端插件包都是真的，而不是一个近似实现。

从选择页打开一台机器时，它会拿到 origin 根目录——因为 dsh 客户端用绝对路径寻址 `/api` 和静态资源，除此之外没有别的办法。所以一个浏览器同时只驱动一台机器。

既然机器占据了所有地址，**回到选择页的入口是 `/_fleet/`**，手机上建议加个书签。

> 如果你打算从手机操作这台机器，建议把它的目录选择器固定为浏览模式——原生选择器只能在那台机器自己的桌面上点。插件的配置层里有现成的一行覆盖和说明注释。

## 本地开发

pixi 负责 Node、pnpm 和一个本地 PostgreSQL，所以开发时不需要容器。Go 用你自己装的。

```sh
pixi install

# 首次
pixi run pg-init && pixi run pg-start && pixi run pg-create

# 节点插件
pixi run typecheck

# 控制面
cp .env.local.example .env.local     # 导出方式见文件头部注释
go run ./cmd/dshf serve
curl localhost:8080/healthz
```

之后只需要 `pixi run pg-start` / `pg-stop`。数据库在 `.devdata/`，端口 5433（避开系统的 PostgreSQL）。迁移在 `dshf serve` 启动时自动执行。

节点插件目前**对着本地的 harness 源码构建**，需要 `deepseek-harness` 检出在本仓库旁边并已 `pnpm run build`。npm 上已发布的 `@deepseek-ai` 包目前还不完整，暂时不能作为构建来源。

## 目录

| 路径 | 内容 |
|---|---|
| [`docs/envelope.md`](docs/envelope.md) | 通信协议，两种语言的唯一事实来源 |
| `cmd/dshf/` | 控制面二进制（守护进程与运维 CLI 合一） |
| `internal/` | 控制面实现 |
| `pkg/envelope/` | 协议的 Go 侧类型 |
| [`dsh/`](dsh/) | dsh 节点插件（TypeScript） |
| `deploy/` | Dockerfile、compose、数据库迁移 |

## 设计要点

控制面**不解析任何 dsh 的业务数据**。它转发不透明的帧、关联请求 id、执行自己的访问策略，仅此而已。这是刻意的：正因为它不认识 dsh 的接口，dsh 升级时它不需要跟着发版。

协议里另有一个 `fleet` 命名空间，是本项目自己的方法（节点遥测、文件浏览等），由节点插件直接调用 Cordis 服务实现。这一半是我们自己的，不随 dsh 变动。

细节见 [`docs/envelope.md`](docs/envelope.md)。

## 许可

[MIT](LICENSE)
