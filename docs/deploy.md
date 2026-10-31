# 部署指南

决策见 ADR-0023，升级顺序和备份责任见 ADR-0014，部署配置见 ADR-0024，所有配置项见 `docs/spec/defaults.md`。

部署是 pull 式：在服务器上从 release tag 取 `deploy/install.sh` 执行，GitHub 不持有服务器凭据。`--version` 可选，缺省时从 GitHub 的最新 release 解析出确切 tag，脚本只从该 tag 下载，不会跟随分支。自动更新是可选功能（`--auto-update`），默认关闭。

## 前置条件

脚本只检查、不安装：

- Control Plane 主机：root、Docker 与 compose 插件、`openssl`、`curl`、`flock`（util-linux）。
- Node：root、systemd、`curl`、`tar`、`sha256sum`、`flock`；`--runtime containerd`（默认）另需 rootful containerd 2.x（`ctr`）、`/opt/cni/bin` 下的 CNI 插件、`nft` 和 `ip`，见下文“containerd Node 的准备”；`local` 不需要这些。
- 能访问 `github.com`、`raw.githubusercontent.com` 和 `ghcr.io`。
- 两个域名解析到 Control Plane 主机：公开入口（如 `cp.example.com`）和对象存储（默认 `s3.cp.example.com`）。对象存储需要独立域名，因为 presigned URL 的 SigV4 签的是 `Host` 和完整路径，不能挂在子路径下。

只有**一个公开端口**（默认 443，可自定义）。脚本内置的 nginx 在这个端口上终止 TLS，按固定路径分流：

| 路径 | 去向 |
|---|---|
| `/v2/` | registry（OCI 分发规范固定） |
| `/orrery.node.v1.Node/` | Node Agent gRPC（proto 全限定名固定） |
| `/` | api、Web console、文件上传、PTY、端口隧道 |
| 对象存储域名 | RustFS |

api、registry、node 面和 RustFS 都只绑回环（`18080`、`15000`、`18081`、`19000`），不对外。

## Control Plane 主机

直接对外（版本由脚本解析为最新 release）：

```sh
curl -fsSL https://raw.githubusercontent.com/zibyn/orrery/main/deploy/install.sh | sudo bash -s -- server \
  --public-host cp.example.com
```

固定版本时从 tag 取脚本并显式给出 `--version vX.Y.Z`；不带版本时脚本只读取 `releases/latest` 的 tag，然后从该 tag 下载全部产物，不会跟随分支。

公开入口是 `https://cp.example.com`，对象存储是 `https://s3.cp.example.com`。自定义端口：

```sh
... server --public-host cp.example.com --public-port 9443
```

公开入口变成 `https://cp.example.com:9443`，对象存储是 `https://s3.cp.example.com:9443`。端口必须同时出现在 registry 地址和 Node 的 `--server` 里，脚本会自动推导。

对象存储用别的域名：

```sh
... server --public-host cp.example.com --public-port 9443 \
  --s3-public-endpoint https://storage.example.com:9443
```

`--s3-public-endpoint` 的端口必须和公开端口一致：registry 用这个地址签 blob 重定向，Node 会直连它。

对象存储如果改指外部服务，release compose 里 registry 的 `extra_hosts` 会把 `ORRERY_S3_PUBLIC_HOST` 固定解析到 `127.0.0.1`；这时删掉该条目，或用你自己的 compose override。

### 前面还有一层反向代理

本机再套一层代理时，把公开地址留给它，nginx 只绑回环：

```sh
... server --public-host cp.example.com --public-port 9443 \
  --public-bind 127.0.0.1
```

外层代理把 `https://cp.example.com:9443` 转发到 `127.0.0.1:9443`，保留 `Host`（含端口）、SNI 和 `X-Forwarded-Proto: https`。gRPC 需要 HTTP/2 到本机 nginx；外层代理如果终结 TLS，再向本机发 HTTPS 即可（自签时信任 `ca/ca.crt` 或关闭上游校验）。公开端口必须保持同一个端口号，因为 registry 和对象存储用它签名 URL。

如果外层也是 nginx：`/` 和 `/v2/` 用 `proxy_pass https://127.0.0.1:9443`，加上 `proxy_ssl_server_name on; proxy_ssl_name cp.example.com; proxy_ssl_verify off;` 并保留 `Host`；`/orrery.node.v1.Node/` 必须用 `grpc_pass grpcs://127.0.0.1:9443`（同样配 `grpc_ssl_name`）。普通 `proxy_pass` 会把 h2 降成 HTTP/1.1，gRPC 无法工作。外层监听公网 IP，本机 nginx 监听 `127.0.0.1`，同端口不冲突。对象存储域名（`s3.cp.example.com:9443`）也用同样的方式转发到本机同一个端口。

### TLS

首次安装在 `/etc/orrery/` 生成 `config/`、`tls/`、`ca/`、`registry/`、`nginx/orrery.conf` 和 `.env`，此后不再重新生成。

- **`--tls self-signed`（默认）**：生成 CA 和一张证书，SAN 同时覆盖公开入口和对象存储域名。Node 安装时带 `--ca`，并用 `--trust-registry` 把 CA 放进 Node 的系统信任库；Control Plane 和 registry 也信任它。证书有效期十年，没有自动轮换。
- **`--tls cert`**：用 `--tls-cert FILE --tls-key FILE` 提供证书；私有 CA 再加 `--ca FILE`。同一张证书必须覆盖两个域名，脚本会检查并警告。

两种模式下 nginx 都是 TLS 终点；registry 在回环上是明文，Node 看到的 registry 是 HTTPS。

使用私有 CA 时，沙箱内的 CLI 只信任镜像自带的 CA（`defaults.md` 的 `access.url`），需要访问 HTTPS api 的镜像里要装上它。宿主机上的 CLI 和 SDK 也要信任 `ca/ca.crt`（例如设置 `SSL_CERT_FILE` 或 `REQUESTS_CA_BUNDLE`）。

端口转发（`access.apps_domain`）走 HTTPS 时需要覆盖 `*.apps` 的通配符证书；生成的证书只覆盖上面两个域名，所以用 `--tls cert` 提供通配符证书，或在 `nginx/orrery.conf` 里为应用域名加 server block。

### 自动更新

`--auto-update` 默认关闭。Control Plane 的更新器是一个独立的 Docker Compose 项目 `orrery-updater`，容器里运行该 release 的 `install.sh`；Node 的更新器是 `orrery-update.service`（Node 本来就是 systemd 部署）。`--no-auto-update` 删除它；不传则保持现状。

- Control Plane：`docker compose -p orrery-updater logs -f` 看日志；每 6 小时检查一次，`.env` 里的 `ORRERY_UPDATE_INTERVAL`（秒）可改，改完用 `docker compose -p orrery-updater -f /etc/orrery/updater.yaml up -d --force-recreate` 重建容器。
- Node：`systemctl status orrery-update` / `journalctl -u orrery-update`；脚本在 `/etc/orrery-agent/updater`。
- 仍走 PostgreSQL 备份、降级拒绝和健康检查；Control Plane 与手动安装共用 `/etc/orrery/.install.lock`，Node 共用 `/etc/orrery-agent/.install.lock`。
- 不跨主版本：`v1.x` → `v2.0` 需要手动执行。rc 在 `sort -V` 下的顺序是已知限制；CI 会把带 `-` 的 tag 标为 prerelease，`releases/latest` 会跳过它们。
- 失败不会自动回滚；更新器按间隔重试。
- 更新器文件由安装脚本生成，手动 `--auto-update` 会重写它们。Control Plane 的更新容器是独立项目，主栈的 `up -d --remove-orphans` 不会碰它。
- Control Plane 的更新容器挂载 `/var/run/docker.sock`，在主机上等价于 root；它不发布端口，但这是自动更新能操作主栈的前提。
- 安全代价：GitHub 账号被攻破等于在所有开启自动更新的主机上执行代码；SHA256SUMS 只覆盖传输损坏，不覆盖账号失窃。
- Node 也可以开启，但必须等 Control Plane 先升级；独立更新器可能让 Node 先更新，Agent 会被拒绝直到 Control Plane 跟上。

### 首次安装之后

1. 配置平台 Snapshot，否则无法创建 Sandbox。在 `/etc/orrery/config/config.yaml` 里按 digest 写：

   ```yaml
   platform_snapshots:
     - name: default
       ref: docker.io/library/ubuntu:24.04@sha256:...
       default: true
   ```

   OIDC、Git provider、端口转发域名等也在同一个文件里配置（`defaults.md`）。改完执行 `docker compose up -d`。
2. 创建第一个 admin：`cd /etc/orrery && docker compose exec server orrery-server bootstrap`。
3. 签发 Enrollment Packet（1 小时有效）：脚本结束时会打印用 `POST /sessions` 取 admin token、再 `POST /enrollment-packets` 的命令，走公开入口。

`config.yaml` 首次安装时生成，升级不覆盖；`.env` 只保留 Compose 需要的插值值，应用配置不要再写进 `.env`。

改地址或证书时编辑 `.env`、`config.yaml` 的 `access.url` 和 `nginx/orrery.conf`，然后：

```sh
cd /etc/orrery && docker compose up -d && docker compose restart nginx
```

`nginx/orrery.conf` 由脚本首次生成，升级不会覆盖；release 若改变了路径或配置结构，需要按新版本的 `deploy/release/nginx.conf` 手工更新。

## Node

把 `/etc/orrery/ca/ca.crt` 复制到 Node 上（公共 CA 证书不需要），然后：

```sh
curl -fsSL https://raw.githubusercontent.com/zibyn/orrery/main/deploy/install.sh | sudo bash -s -- node \
  --server cp.example.com:9443 --packet ore_... \
  --ca ca.crt \
  --trust-registry cp.example.com:9443 \
  --trust-registry s3.cp.example.com:9443 \
  --runtime containerd -- --buildkit unix:///run/buildkit/buildkitd.sock
```

版本同样可省略（默认最新 release）；要自动跟进加 `--auto-update`，固定版本则从 tag 取脚本并加 `--version vX.Y.Z`。

- 端口是 443 时去掉 `--server` 和 `--trust-registry` 里的端口。
- 用公共 CA 证书时去掉 `--ca` 和两个 `--trust-registry`。
- 对象存储也要信任，因为 blob 下载会从 registry 307 到它。
- `--trust-registry` 可重复；Agent 用宿主的系统信任库拉镜像，脚本把 CA 装进去（`update-ca-certificates` 或 `update-ca-trust`），Agent 重启后生效。buildkitd 有自己的 registry 配置，见下文。
- `--runtime containerd|local` 之后可用 `--` 追加 `orrery-agent run` 的其它参数，例如 `-- --buildkit unix:///run/buildkit/buildkitd.sock`、`-- --isolation runsc`、`-- --sandbox-subnet 10.213.1.0/24` 或 `-- --local-user sandbox`。
- 不带 `--buildkit` 的 Node 不做 Snapshot 构建（`snapshot_build` 为 `unsupported`）。

### containerd Node 的准备

脚本只检查、不安装。`--runtime containerd` 的 Node 需要：

- **containerd 2.x**，rootful，socket 默认 `/run/containerd/containerd.sock`（`-- --containerd SOCKET` 可改）。Agent 只用自己的 namespace `orrery`，不改 containerd 配置，可以和同一台宿主上的 Docker 共用一个 containerd。
- **CNI 插件**装在 `/opt/cni/bin`（至少 `bridge`、`host-local`、`loopback`），以及 `nft` 和 `ip`。每个 Sandbox 一个 netns，挂在 Agent 自建的网桥 `orrery0` 上，地址取自 `--sandbox-subnet`（默认 `10.213.0.0/24`）。多台 Node 的子网不能重叠，Egress Rules 靠它们区分 Sandbox。宿主防火墙要放行 `orrery0` 的转发：例如 Docker 把 FORWARD 默认策略设成 drop，Sandbox 就出不了网。
- **buildkitd**（做 Snapshot 构建时）：独立、rootful，以 `--buildkit` 传给 Agent，例如 `buildkitd --addr unix:///run/buildkit/buildkitd.sock --oci-worker=true --containerd-worker=false`，并作为 systemd 服务常驻。私有 CA 时在它的 `buildkitd.toml` 里给 registry 和对象存储两个主机都配上 CA：`[registry."cp.example.com:9443"] ca = ["/etc/orrery-agent/ca.crt"]`。
- **gVisor**（可选，`-- --isolation runsc`）：`runsc` 和 `containerd-shim-runsc-v1` 放在 containerd 的 PATH 上；data root 不能在 tmpfs 上。runsc Node 不挂 GPU。
- **GPU**（可选）：NVIDIA Container Toolkit 生成的 CDI spec（`nvidia-ctk cdi generate`），只在 runc Node 上生效。

Egress Rules 写在 Control Plane 的 `config.yaml`（`defaults.md`）。**Rules 为空时 Sandbox 的出站完全放开，包括云 metadata 地址和 Control Plane 所在的内网**；`defaults.md` 有一份推荐的 Rules。Local Runtime 不受 Egress Rules 约束。

### 从 Docker 或 Podman Node 迁移

Docker 和 Podman Runtime Provider 已删除（ADR-0026），不能双栈并行。升级后的 Agent 遇到 `--runtime docker`、`--runtime podman` 或 `--docker` 会在启动时报错退出，`install.sh` 也会拒绝保留这样的 unit。按 Node 逐台迁移，Control Plane 先升级：

1. **排空**：`orrery node pause <node_id>`，再停掉这台 Node 上仍在运行的 Sandbox（或等它们空闲停止），直到这台 Node 上没有运行中的 Sandbox（paused 且没有运行中的 Sandbox 即 drained，ADR-0014）。Paused Node 不再接收新的 Workspace 和构建，已绑定的 Workspace 留在宿主上。
2. **安装 containerd、buildkitd 和 CNI 插件**，见上一节。Docker 可以留着，也可以在确认无用后卸载；旧 Agent 留下的 `orrery-*` 容器和镜像可以用 `docker` 清掉，containerd Agent 不会去碰 `moby` namespace。
3. **升级 Agent**：用 `--runtime containerd` 重跑 Node 安装命令（带上需要的 `-- --buildkit ...` 等参数），脚本会重写 systemd unit。不要传 `--packet`，也不要删 `/var/lib/orrery-agent/identity.json` 和 data root（默认 `/var/lib/orrery`）：Workspace 还在 data root 里。
4. **重新注册**：Agent 以原来的 identity 重连，重新跑自检，Capabilities 里的 `runtime_provider` 变成 `containerd`。确认 Node 是 `ready` 后 `orrery node resume <node_id>`。

已停止的 Sandbox 下次 start 时由 `Create` 在 containerd 上重建 Runtime（没有单独的 Start 操作），Workspace 原样挂回。没有迁移的 Docker 或 Podman Node 不再接收容器 Sandbox。Rootless Podman Node 需要改成 rootful containerd 重装。

脚本校验 `SHA256SUMS` 后把 Agent 和 CLI 装到 `/opt/orrery/bin/`，首次运行用 Packet 注册（identity 在 `/var/lib/orrery-agent/`），再以 systemd 服务 `orrery-agent` 运行。服务使用 `KillMode=process`，重启 Agent 不会停掉 Exec。

## 升级与回滚

先 Control Plane，后 Node（ADR-0014）。升级可以重跑同一条命令（版本省略即最新 release），也可以交给 `--auto-update` 更新器：

- Control Plane：先拉新镜像，再停 server、`pg_dump` 到 `/var/lib/orrery-server/backups/<时间>-<旧版本>.sql.gz`，然后启动新版本。迁移是单向的，脚本拒绝降级；回滚就是恢复备份（开启自动更新时先停掉更新器，否则下次会自动升回去）：

  ```sh
  docker compose -p orrery-updater down   # 开启自动更新时
  cd /etc/orrery && docker compose stop server
  docker compose exec -T postgres dropdb -U orrery orrery
  docker compose exec -T postgres createdb -U orrery orrery
  gunzip -c /var/lib/orrery-server/backups/<文件> | docker compose exec -T postgres psql -U orrery orrery
  sed -i 's/^ORRERY_VERSION=.*/ORRERY_VERSION=<旧版本>/' .env && docker compose up -d
  ```

- Node：下载、校验、替换二进制后 `systemctl restart`；不带 `--runtime` 或 `--` 参数时保留现有 systemd unit（Docker 或 Podman 的 unit 除外，见“从 Docker 或 Podman Node 迁移”）。允许装旧版本，Control Plane 会把它标为 `outdated`。回滚前同样先停掉 `orrery-update.service`。

对象存储（RustFS 卷 `orrery_rustfs-data`）的备份由部署方负责，脚本只备份 PostgreSQL。

`registry/config.yml` 同样只在首次安装时生成，升级不覆盖。purge 要删掉已删除 Snapshot 和 SnapshotVersion 的 registry manifest，需要 registry 允许删除（#151）。较早安装的部署缺这一项，purge 会一直收到 `UNSUPPORTED` 并重试，manifest 不会被清理。在 `storage:` 下、与 `s3:` 同级补上，然后重启 registry：

```yaml
storage:
  s3:
    ...
  delete:
    enabled: true
```

```sh
cd /etc/orrery && docker compose restart registry
```

只删 manifest，不回收 blob；blob GC 仍是运维流程（ADR-0014）。

排障：Control Plane 通过公开地址推送 registry（`registry.push_addr` 默认等于 `registry.addr`）。如果容器解析不到公开地址或 NAT 不回环，Platform Snapshot 导入会失败。没有用 `--public-bind` 时（nginx 监听所有接口），可以在 `/etc/orrery/compose.override.yaml` 里给 `server` 加：

```yaml
services:
  server:
    extra_hosts:
      - "cp.example.com:host-gateway"
      - "s3.cp.example.com:host-gateway"
```

外层代理模式下让 server 经外层代理访问，或把 registry 暴露到容器可达的地址并设置 `registry.push_addr`。

## 发版

推送 tag `vX.Y.Z` 后 CI 在门禁通过时：

- 推送镜像 `ghcr.io/zibyn/orrery-server:X.Y.Z` 和 `:latest`（main 上的每次推送对应 `:main`）。
- 创建 GitHub Release，资产为 `orrery-node_vX.Y.Z_linux_{amd64,arm64}.tar.gz`（Agent + CLI）、`orrery_vX.Y.Z_darwin_{amd64,arm64}.tar.gz`（CLI）和 `SHA256SUMS`。带 `-` 的 tag（如 `v1.2.0-rc.1`）标记为 prerelease，`releases/latest` 和自动更新会跳过它。

GHCR 包首次推送后默认是私有的，需要在 GitHub 的 package 设置里手动改为 public 一次，服务器才能免登录拉取。
