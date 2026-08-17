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

每个节点上的 agent 都是**真正本地**的——git、编译、测试、语言服务器全都跑在那台机器上。控制面只做转发，不代替谁执行。浏览器加载到的是那个节点自己的 dsh 界面，经由上行通道送达。

## 为什么不直接把 `dsh web` 暴露出去

因为它没有认证。dsh 自己的文档写得很直白：

> 这道围栏是可达性策略，**不是认证**；Web 载体不提供任何认证层。

`dsh-fleet` 补上这一层，并顺带给你多机视图。它不 fork dsh——节点插件是一个普通的 Cordis 插件，挂在 dsh 本来就设计成传输无关的网关上。

Pre-alpha，还没有限流。

## HTTPS 是必需的，不是建议

浏览器把 `crypto.randomUUID` 等安全上下文 API 卡在 HTTPS 之后，只豁免 loopback。dsh 客户端要用它们，所以用 `http://192.168.x.x` 打开的控制台，设置页会直接报错——而同一份构建在本机走 `127.0.0.1` 一切正常，这正是它容易被漏掉的原因。

推荐用反向代理终止 TLS，dshf 自己就不需要证书了。[`deploy/Caddyfile.example`](deploy/Caddyfile.example) 已端到端验证过（含节点上行和两条事件下行），[`deploy/nginx.conf.example`](deploy/nginx.conf.example) 覆盖同样的点但**未经验证**。

```caddy
fleet.example.com {
	reverse_proxy 127.0.0.1:8080 {
		transport http { read_timeout 0 }
	}
}
```

`DSHF_PUBLIC_URL` 要设成**浏览器实际输入的地址**（它决定 cookie 作用域和 Secure 标志），`DSHF_LISTEN` 留在 loopback。

局域网没有公网域名时，`dshf cert` 会签一张覆盖本机各地址的自签名证书，配 `DSHF_TLS_CERT` / `DSHF_TLS_KEY` 直接用。手机首次会弹一次警告，点继续之后 origin 就是安全上下文了。

## 快速开始

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

其他命令：`dshf node ls`、`dshf node revoke <id>`、`dshf user add <name>`。

## 接入一台机器

在要被管理的机器上装插件，设三个环境变量，照常启动 `dsh web`：

```sh
dsh plugin --profile web add <这个包>

export DSH_FLEET_URL=wss://fleet.example.com/uplink
export DSH_FLEET_NODE_ID=laptop
export DSH_FLEET_TOKEN=nt_...

dsh web
```

没有配置 `DSH_FLEET_URL` 时插件完全不生效，所以装上它不会改变 `dsh web` 原本的行为。

从选择页打开一台机器时，它会拿到 origin 根目录——因为 dsh 客户端用绝对路径寻址 `/api` 和静态资源，除此之外没有别的办法。所以一个浏览器同时只驱动一台机器，而**回到选择页的入口是 `/_fleet/`**，手机上建议加个书签。

> 如果你打算从手机操作某台机器，建议把它的目录选择器固定为浏览模式——原生选择器只能在那台机器自己的桌面上点。插件的配置层里有现成的一行覆盖。

## 本地开发

pixi 负责 Node、pnpm 和一个本地 PostgreSQL，所以开发时不需要容器。Go 用你自己装的。

```sh
pixi install
pixi run pg-init && pixi run pg-start && pixi run pg-create   # 首次
pixi run typecheck && pixi run test                           # 节点插件
go run ./cmd/dshf serve                                       # 控制面
```

先把 `.env.local.example` 复制成 `.env.local` 并导出，导出方式见文件头部注释。之后只需要 `pixi run pg-start` / `pg-stop`。

节点插件目前**对着本地的 harness 源码构建**，需要 `deepseek-harness` 检出在本仓库旁边并已构建。npm 上已发布的 `@deepseek-ai` 包目前还不完整，暂时不能作为构建来源。

## 设计要点

控制面**不解析任何 dsh 的业务数据**。它转发不透明的帧、关联请求 id、执行自己的访问策略，仅此而已。正因为它不认识 dsh 的接口，dsh 升级时它不需要跟着发版。

这条策略只有一个开关。dsh 在自己的浏览器载体里把一批方法钉死在 loopback 上，换成别的载体就得自己决定放行到哪一步：`DSHF_PRIVILEGED_ACCESS` 默认 `read`，设置和预设读得到，但远端浏览器改不动任何东西。`full` 才额外放开凭据写入、设置修改，以及那些会动到机器本机桌面的方法。

协议里另有一个 `fleet` 命名空间，是本项目自己的方法（节点遥测、文件浏览等），由节点插件直接调用 Cordis 服务实现。这一半是我们自己的，不随 dsh 变动。

通信协议见 [`docs/envelope.md`](docs/envelope.md)。

## 许可

[MIT](LICENSE)
