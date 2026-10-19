# 部署指南

决策见 ADR-0023，升级顺序和备份责任见 ADR-0014，所有配置项见 `docs/spec/defaults.md`。

部署是 pull 式：在服务器上从 release tag 取 `deploy/install.sh` 执行，GitHub 不持有服务器凭据。版本参数必填，没有 `latest`。

## 前置条件

脚本只检查、不安装：

- Control Plane 主机：root、Docker 与 compose 插件、`openssl`、`curl`。
- Node：root、systemd、`curl`、`tar`、`sha256sum`，以及所选 Runtime（`docker` 或 `podman`；`local` 不需要）。
- 能访问 `github.com`、`raw.githubusercontent.com` 和 `ghcr.io`。

端口：`18080` api、`18081` node 面（gRPC + TLS）、`15000` registry、`19000` 对象存储。Node 要能连到后三者，用户和沙箱要能连到 api。

## Control Plane 主机

```sh
V=v1.0.0
curl -fsSL https://raw.githubusercontent.com/zibyn/stars-sandbox/$V/deploy/install.sh | sudo bash -s -- server \
  --version $V --public-host cp.example.internal
```

`--public-host` 是 Node 连接 node 面用的主机名或 IP，也是自签证书的 SAN。首次安装在 `/etc/stars-sandbox/` 生成 `.env`、TLS 证书、加密密钥、registry 配置与凭据，此后不再重新生成；要改地址或其它配置，编辑 `.env` 后在该目录执行 `docker compose up -d`。

### TLS 的两种模式

远端 Node 的 dockerd/podman 只对 loopback 放行明文 registry，所以 registry 必须是 HTTPS。

- **`--tls self-signed`（默认）**：脚本生成 CA（`/etc/stars-sandbox/ca/ca.crt`）和服务端证书。registry 直接用它提供 TLS；api（`http://HOST:18080`）和对象存储（`http://HOST:19000`）保持 HTTP。Node 安装时用 `--trust-registry` 把 CA 放进 Docker 和 Podman 的 `certs.d`，无需重启守护进程。
- **`--tls proxy`**：HTTPS 由你的反向代理提供。必须同时给出三个公开地址：

  ```sh
  ... server --version $V --public-host cp.example.com --tls proxy \
    --access-url https://stars.example.com \
    --registry-addr registry.example.com \
    --s3-public-endpoint https://s3.example.com
  ```

  api、registry、对象存储只绑 `127.0.0.1`，代理上游分别是 `127.0.0.1:18080`、`127.0.0.1:15000`、`127.0.0.1:19000`。

两种模式下 node 面（`18081`）都由 Agent 直连 TLS，不能走反代；默认用生成的自签证书，也可以用 `--tls-cert FILE --tls-key FILE` 换成自己的证书（registry 同时使用它，此时不生成 CA）。生成的证书有效期十年，没有自动轮换。

使用私有 CA 时，沙箱内的 CLI 只信任镜像自带的 CA（`defaults.md` 的 `STARS_ACCESS_URL`），需要访问 HTTPS api 的镜像里要装上它。

### 首次安装之后

1. 配置平台 Snapshot，否则无法创建 Sandbox。在 `.env` 里按 digest 写：

   ```sh
   STARS_PLATFORM_SNAPSHOTS=default=docker.io/library/ubuntu:24.04@sha256:...
   STARS_DEFAULT_SNAPSHOT=default
   ```

   OIDC、Git provider、端口转发域名等也在 `.env` 配置（`defaults.md`）。改完执行 `docker compose up -d`。
2. 创建第一个 admin：`cd /etc/stars-sandbox && docker compose exec server stars-sandbox-server bootstrap`。
3. 签发 Enrollment Packet（1 小时有效）：脚本结束时会打印用 `POST /sessions` 取 admin token、再 `POST /enrollment-packets` 的命令。

## Node

把 `/etc/stars-sandbox/ca/ca.crt` 复制到 Node 上（用自带受信证书时不需要），然后：

```sh
curl -fsSL https://raw.githubusercontent.com/zibyn/stars-sandbox/$V/deploy/install.sh | sudo bash -s -- node \
  --version $V --server cp.example.internal:18081 --packet sse_... \
  --ca ca.crt --trust-registry cp.example.internal:15000 --runtime docker
```

proxy 模式下去掉 `--trust-registry`。`--runtime podman|local` 之后可用 `--` 追加 `stars-sandbox-agent run` 的其它参数，例如 `-- --local-user sandbox` 或 `-- --buildkit unix:///run/buildkit/buildkitd.sock`。

脚本校验 `SHA256SUMS` 后把 Agent 和 CLI 装到 `/opt/stars-sandbox/bin/`，首次运行用 Packet 注册（identity 在 `/var/lib/stars-sandbox-agent/`），再以 systemd 服务 `stars-sandbox-agent` 运行。服务使用 `KillMode=process`，重启 Agent 不会停掉 Exec。

## 升级与回滚

先 Control Plane，后 Node（ADR-0014）。用新版本重跑同一条命令，升级时只需要 `--version`：

- Control Plane：先拉新镜像，再停 server、`pg_dump` 到 `/var/lib/stars-sandbox-server/backups/<时间>-<旧版本>.sql.gz`，然后启动新版本。迁移是单向的，脚本拒绝降级；回滚就是恢复备份：

  ```sh
  cd /etc/stars-sandbox && docker compose stop server
  docker compose exec -T postgres dropdb -U stars stars
  docker compose exec -T postgres createdb -U stars stars
  gunzip -c /var/lib/stars-sandbox-server/backups/<文件> | docker compose exec -T postgres psql -U stars stars
  sed -i 's/^STARS_VERSION=.*/STARS_VERSION=<旧版本>/' .env && docker compose up -d
  ```

- Node：下载、校验、替换二进制后 `systemctl restart`；不带 `--runtime` 或 `--` 参数时保留现有 systemd unit。允许装旧版本，Control Plane 会把它标为 `outdated`。

对象存储（RustFS 卷 `stars-sandbox_rustfs-data`）的备份由部署方负责，脚本只备份 PostgreSQL。

## 发版

推送 tag `vX.Y.Z` 后 CI 在门禁通过时：

- 推送镜像 `ghcr.io/zibyn/stars-sandbox-server:X.Y.Z` 和 `:latest`（main 上的每次推送对应 `:main`）。
- 创建 GitHub Release，资产为 `stars-sandbox-node_vX.Y.Z_linux_{amd64,arm64}.tar.gz`（Agent + CLI）、`stars-sandbox_vX.Y.Z_darwin_{amd64,arm64}.tar.gz`（CLI）和 `SHA256SUMS`。

GHCR 包首次推送后默认是私有的，需要在 GitHub 的 package 设置里手动改为 public 一次，服务器才能免登录拉取。
