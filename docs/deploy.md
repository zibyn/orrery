# 部署指南

决策见 ADR-0023，升级顺序和备份责任见 ADR-0014，部署配置见 ADR-0024，所有配置项见 `docs/spec/defaults.md`。

部署是 pull 式：在服务器上从 release tag 取 `deploy/install.sh` 执行，GitHub 不持有服务器凭据。`--version` 可选，缺省时从 GitHub 的最新 release 解析出确切 tag，脚本只从该 tag 下载，不会跟随分支。不提供自动更新（ADR-0028），升级就是重跑 `install.sh`。

## 前置条件

脚本只检查、不安装：

- Control Plane 主机：root、Docker 与 compose 插件、`openssl`、`curl`、`flock`（util-linux）。
- Node：root、systemd、`curl`、`tar`、`sha256sum`、`flock`、`cmp`，cgroup v2。containerd、runc、buildkitd 和 runsc 都在 node 包里，不用另装（ADR-0028），见下文“containerd Node 的准备”。
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

### 自动更新已移除

ADR-0028 移除了自动更新（取代 ADR-0023 的 `--auto-update`）。升级就是运维重跑 `install.sh`；现在传 `--auto-update` 或 `--no-auto-update` 会报错。旧版本留下的更新器（Control Plane 的 Compose 项目 `orrery-updater`、Node 的 `orrery-update.service`）在下一次成功运行 `install.sh` 后自动删除。

### Polaris

Polaris 是唯一的登录方式（ADR-0029），`config.yaml` 的 `oidc` 段必填，缺了 Control Plane 不启动。在 Polaris 控制台里按顺序建：

1. **API**：标识 `api://orrery`（即 `oidc.audience`），在它下面加 Permission `orrery:admin`。
2. **Role**：例如「Orrery 管理员」，挂在这个 API 上，包含 `orrery:admin`。持有它的人下次登录时成为 Orrery admin；收回后经 Webhook 立即失去 admin。
3. **Web Application**（confidential）：
   - default API 设为 `api://orrery`。Polaris 不支持 `resource` 参数，access token 的 `aud` 只由 default API 决定；设错了登录会失败。
   - callback URL 填 `https://cp.example.com/api/v1/oidc/callback`（公开地址加这个路径）。
   - Back-Channel Logout URI 填 `https://cp.example.com/api/v1/polaris/backchannel-logout`。在 Polaris 登出、被管理员结束 Session、被禁用或删除时，对应的 Orrery Session 随之失效。
   - 只让指定的人用 Orrery 时，打开 require Role。
   - Webhook URL 填 `https://cp.example.com/api/v1/polaris/webhook`，再生成一个 Webhook secret（`whsec_...`）。在 Polaris 停用、启用、删除一个人或改他的 Role 时，Orrery 据此立即生效：停用拒绝他的全部凭据，改 Role 收回 admin，直到下次登录重新判定。
4. 把 issuer、client id、client secret、audience 和 Webhook secret 写进 `config.yaml`：

```yaml
oidc:
  issuer_url: https://polaris.example.com
  client_id: ...
  client_secret: ...
  audience: api://orrery
  webhook_secret: whsec_...
```

旧的 `oidc.allow_signups` 和 `oidc.email_domains` 已移除，配置里还有它们时拒绝启动。

### 第一个 admin 和 Machine User

Orrery 没有密码，也不能在 API 里建 User。`orrery-server user-token` 直接连 PostgreSQL（读同一个 `config.yaml`），打印一枚 API Token 的 JSON，Polaris 或 API 挂了也能用：

```sh
cd /etc/orrery
# 给已登录过的 Polaris User 签 token（不改 is_admin；Polaris 不可用时的应急入口）
docker compose exec server orrery-server user-token --sub <Polaris sub> --scope admin
# 找到或新建 Machine User（CI、bot），--admin 让它成为 admin
docker compose exec server orrery-server user-token --name ci --admin --scope admin
```

全新部署时还没人有 Polaris Role，用 `--name ops --admin` 建一个 admin Machine User 起步。

## Node

把 `/etc/orrery/ca/ca.crt` 复制到 Node 上（公共 CA 证书不需要），然后：

```sh
curl -fsSL https://raw.githubusercontent.com/zibyn/orrery/main/deploy/install.sh | sudo bash -s -- node \
  --server cp.example.com:9443 --packet ore_... \
  --ca ca.crt \
  --trust-registry cp.example.com:9443 \
  --trust-registry s3.cp.example.com:9443
```

版本同样可省略（默认最新 release）；固定版本则从 tag 取脚本并加 `--version vX.Y.Z`。

- 端口是 443 时去掉 `--server` 和 `--trust-registry` 里的端口。
- 用公共 CA 证书时去掉 `--ca` 和两个 `--trust-registry`。
- 对象存储也要信任，因为 blob 下载会从 registry 307 到它。
- `--trust-registry` 可重复；Agent 用宿主的系统信任库拉镜像，脚本把 CA 装进去（`update-ca-certificates` 或 `update-ca-trust`），Agent 重启后生效。同样这几个主机也写进捆绑 buildkitd 的 `buildkitd.toml`；之后重跑不带 `--trust-registry` 时沿用上次的主机。
- `--runtime containerd|local` 之后可用 `--` 追加 `orrery-agent run` 的其它参数，例如 `-- --isolation runsc`、`-- --sandbox-subnet 10.213.1.0/24` 或 `-- --local-user sandbox`。
- Agent 默认连捆绑的 containerd 和 buildkitd；`-- --buildkit=` 关掉 Snapshot 构建（`snapshot_build` 为 `unsupported`）。脚本没有用宿主 containerd 或 buildkitd 的方式：`--` 后的 `--containerd` 和带值的 `--buildkit` 会被去掉并打印出来。

### containerd Node 的准备

containerd、buildkitd 和 runsc 不用运维准备：脚本把包里的运行时装到 `/opt/orrery/bin/`，并起三个 systemd 服务（ADR-0028）：

- `orrery-containerd`：Agent 私有的 containerd，socket `/run/orrery/containerd/containerd.sock`，root `/var/lib/orrery-agent/containerd`，配置 `/etc/orrery-agent/containerd.toml`。它不读宿主的 `/etc/containerd/conf.d`、不开 NRI 和 CRI，所以可以和宿主上的 Docker 及其 containerd 并存，互不影响。
- `orrery-buildkitd`：Snapshot 构建用，socket `/run/orrery/buildkit/buildkitd.sock`，配置 `/etc/orrery-agent/buildkitd.toml`。`--runtime local` 的 Node 也装它（构建与 Runtime 无关），但不装 `orrery-containerd`。
- `orrery-agent`：`Wants=` 加 `After=orrery-containerd`，containerd 重启时 Agent 自己重连，不跟着重启。

两份配置每次运行都重新生成，手改会被覆盖。重跑时只有 `containerd` 或 `buildkitd` 的二进制、配置或 unit 变了，才重启对应服务；containerd 重启不影响运行中的 Exec。Agent 每次都重启。

宿主仍要准备：

- **Sandbox 网络**不需要 CNI 插件、`nft` 或 `ip`：Agent 自己经 netlink 建网和下发 nftables 规则。每个 Sandbox 一个 netns，挂在 Agent 自建的网桥 `orrery0` 上，地址取自 `--sandbox-subnet`（默认 `10.213.0.0/24`）。多台 Node 的子网不能重叠，Egress Rules 靠它们区分 Sandbox。宿主防火墙要放行 `orrery0` 的转发：例如 Docker 把 FORWARD 默认策略设成 drop，Sandbox 就出不了网，可以 `iptables -I DOCKER-USER -i orrery0 -j ACCEPT` 并放行 `-o orrery0` 的 `RELATED,ESTABLISHED`。
- **Node Mesh**（跨 Node 的 Sandbox Link，ADR-0027）：内核要带 WireGuard（Linux 5.6+ 主线自带，`modprobe wireguard` 能加载即可），不需要 `wireguard-tools`。没有时脚本只警告，Agent 照常运行，但这台 Node 不加入 Mesh。至少一方要用 `-- --mesh-endpoint host:port` 给出其他 Node 能直接访问的地址，并在防火墙放行 `--mesh-port`（默认 `51820`）的 UDP；两台都没有端点的 Node 之间没有通路（`defaults.md`）。
- **gVisor**（可选，`-- --isolation runsc`）：runsc 已捆绑；data root 不能在 tmpfs 上。runsc Node 不挂 GPU。
- **GPU**（可选）：NVIDIA 驱动和 NVIDIA Container Toolkit 生成的 CDI spec（`nvidia-ctk cdi generate`）仍在宿主上，只在 runc Node 上生效。
- **探针镜像**：私有 containerd 的镜像存储一开始是空的，Agent 自检要拉 `docker.io/library/alpine:3.22`。拉不到 Docker Hub 的 Node 停在 `unavailable`，直到导入这个镜像并重启 Agent（Agent 只在连上 Control Plane 时跑自检），例如 `docker save docker.io/library/alpine:3.22 | ctr -a /run/orrery/containerd/containerd.sock -n orrery images import -` 再 `systemctl restart orrery-agent`（`ctr` 不在包里，用宿主上 containerd 2.x 的）。

### 从宿主 containerd 迁移

ADR-0028 之前的 Agent 用宿主 containerd 的 `orrery` namespace。迁移是一次性切换：

1. drain 这台 Node，等它上面的 Sandbox 停下。
2. 用新版本重跑 install.sh（同一条命令）。以前按本文加的 `-- --containerd ...`、`-- --buildkit unix:///run/buildkit/buildkitd.sock` 可以留着：脚本从参数和保留的 `ExecStart` 里去掉它们并打印去掉了什么，Agent 改连捆绑的 containerd 和 buildkitd。Node 保留 identity，不重新注册。镜像在私有 containerd 里重新拉；stopped 的 Sandbox 下次启动时在私有 containerd 里重新 `Create`，Workspace 还在宿主上。
3. Node `ready` 后 undrain。

脚本发现宿主 containerd 上还有 `orrery` namespace 时，打印删除它的命令，但不执行，因为那个 containerd 可能还在给 Docker 用。宿主没有 `ctr` 时脚本跳过这项检查并提示，可手动 `ctr -a /run/containerd/containerd.sock namespaces ls` 查看。

Egress Rules 写在 Control Plane 的 `config.yaml`（`defaults.md`）。**Rules 为空时 Sandbox 的出站完全放开，包括云 metadata 地址和 Control Plane 所在的内网**；`defaults.md` 有一份推荐的 Rules。Local Runtime 不受 Egress Rules 约束。

脚本校验 `SHA256SUMS` 后把整个包（Agent、CLI 和运行时）装到 `/opt/orrery/bin/`，首次运行用 Packet 注册（identity 在 `/var/lib/orrery-agent/`），再以 systemd 服务 `orrery-agent` 运行。服务使用 `KillMode=process`，重启 Agent 不会停掉 Exec。

## 升级与回滚

先 Control Plane，后 Node（ADR-0014）。升级就是重跑同一条命令（版本省略即最新 release）：

- Control Plane：先拉新镜像，再停 server、`pg_dump` 到 `/var/lib/orrery-server/backups/<时间>-<旧版本>.sql.gz`，然后启动新版本。迁移是单向的，脚本拒绝降级；回滚就是恢复备份：

  ```sh
  cd /etc/orrery && docker compose stop server
  docker compose exec -T postgres dropdb -U orrery orrery
  docker compose exec -T postgres createdb -U orrery orrery
  gunzip -c /var/lib/orrery-server/backups/<文件> | docker compose exec -T postgres psql -U orrery orrery
  sed -i 's/^ORRERY_VERSION=.*/ORRERY_VERSION=<旧版本>/' .env && docker compose up -d
  ```

- Node：下载、校验、替换二进制后重启 `orrery-agent`，`orrery-containerd`、`orrery-buildkitd` 只在变了时重启；不带 `--runtime` 或 `--` 参数时保留 Agent 现有的启动参数。允许装旧版本，Control Plane 会把它标为 `outdated`。

从自带用户系统切到 Polaris（ADR-0029）的那次升级前，先按 `docs/polaris-migration.md` 把现有 User 迁到 Polaris。去掉密码的那个版本在还有没关联 Polaris 的 `password` User 时拒绝迁移数据库，并列出这些 User。

对象存储（RustFS 卷 `orrery_rustfs-data`）的备份由部署方负责，脚本只备份 PostgreSQL。

`registry/config.yml` 同样只在首次安装时生成，升级不覆盖；其中开启了 `storage.delete`，purge 靠它删掉已删除 Snapshot 和 SnapshotVersion 的 registry manifest（#151）。只删 manifest，不回收 blob；blob GC 仍是运维流程（ADR-0014）。

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
- 创建 GitHub Release，资产为 `orrery-node_vX.Y.Z_linux_{amd64,arm64}.tar.gz`（`bin/` 下的 Agent、CLI 与捆绑的 containerd、runc、buildkitd、runsc，ADR-0028）、`orrery_vX.Y.Z_darwin_{amd64,arm64}.tar.gz`（CLI）和 `SHA256SUMS`。带 `-` 的 tag（如 `v1.2.0-rc.1`）标记为 prerelease，`releases/latest` 会跳过它。

GHCR 包首次推送后默认是私有的，需要在 GitHub 的 package 设置里手动改为 public 一次，服务器才能免登录拉取。
