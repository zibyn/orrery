# 迁移到 Polaris

已有部署切换到 Polaris（ADR-0029）前，用 `orrery-polaris-migrate` 把现有 User 关联到 Polaris User，这样没人要重设密码，每个人的 Sandbox、Resource、Secret 都还在。脚本一次性、可重跑，不属于 server。

**顺序不能乱**：脚本必须在 Orrery 配置切到 Polaris 之前跑完、报告里没有 `failed`。先切配置的话，人们首次登录会被建成新的空 User。

## 1. 配置 Polaris

按 `docs/deploy.md` 在 Polaris 里建好 Orrery 的 API、Permission、Role 和 Web Application。另外建一个迁移用的 M2M Application：

1. `settings.defaultApi` 设为 `urn:polaris:management-api`，记下 client id 和只显示一次的 secret。
2. 给它一个只含 `users:read`、`users:write` 的 Management API Role（没有 Role 时所有调用都是 403）。
3. 确认实例开着密码登录，否则导入密码哈希会被 422 拒绝。

## 2. 跑脚本，直到没有失败

在 Control Plane 主机上，数据库密码取自 `/etc/orrery/config/config.yaml` 的 `database.password`：

```sh
cd /etc/orrery
export POLARIS_CLIENT_SECRET=<M2M secret>
docker compose exec -T -e POLARIS_CLIENT_SECRET server orrery-polaris-migrate \
  --database-url "postgres://orrery:<密码>@postgres:5432/orrery" \
  --issuer https://polaris.example.com \
  --client-id <M2M client id>
```

`--issuer` 必须和之后的 `oidc.issuer_url` 一字不差，脚本把它写进 User 的 `oidc_issuer`。

脚本对每个未删除的 User：

| User | 做法 | 报告 |
|---|---|---|
| `password` | `POST /users`，带邮箱（没有邮箱时用用户名）和现有的 argon2id 哈希；有邮箱时再设用户名 | `created` |
| `password`，Polaris 已有此邮箱或用户名（409） | 关联那个 Polaris User，不改它 | `linked` |
| 上一个 OIDC issuer 的 `oidc` User | 按邮箱精确匹配 Polaris User（Polaris 的邮箱都已验证） | `linked` |
| 已关联到这个 issuer | 什么都不做 | `already linked` |
| `none` | 不动，切换后是 machine User | `machine` |

关联就是把 Polaris 的 `sub` 和 issuer 写进 `oidc_subject` / `oidc_issuer`。切换前现有的密码登录照常可用。

`failed` 都带原因，常见的有：

- 名字不符合 Polaris 用户名规则 `^[a-z0-9._-]{3,32}$`（大写、空格、过短等）。脚本不会替人改名：用 `PATCH /api/v1/users/{id}` 改名后重跑。
- 422：哈希格式不被接受，或 Polaris 关了密码登录。
- OIDC User 没有邮箱，或 Polaris 里没有恰好一个持有该邮箱的 User。在 Polaris 里建好这个人（或改邮箱）后重跑，脚本不会猜。
- 写关联失败：两个 Orrery User 对上了同一个 Polaris User。

`created` 行后面若有 “username not set”，说明 User 已建好并关联，只是用户名被占用或无效，到 Polaris 里手动设。重跑时它显示为 `already linked`，不会再提示。

有 `failed` 时脚本以非零退出。修好后重跑，已关联的 User 不会再碰 Polaris。

## 3. 分配管理员 Role

报告最后列出当前的 admin 和他们的 `sub`。在 Polaris 里给每个 Polaris User 分配 Orrery 的管理员 Role。machine User 不在 Polaris 里，它们保留 `is_admin`。

## 4. 切换 Orrery 配置

在 `config.yaml` 里配好 `oidc.*`（见 `docs/spec/defaults.md`），升级到切换后的版本，`docker compose up -d`。此后人们只能经 Polaris 登录，按 (issuer, sub) 落到原来的 User 上。已有的 API Token 继续可用。
