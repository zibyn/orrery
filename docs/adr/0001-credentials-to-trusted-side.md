# 凭据只交付给可信侧

沙箱里的代码是不可信侧，Worker 是可信侧（`#6`）。我们决定**凭据只交付给 Worker**：git 的远端动词（clone/fetch/pull/push）由 Worker 在宿主侧代执行，S3 以**物化/收集**的形式进出（平台把输入对象放进任务级挂载点、把产物收走），沙箱内不出现任何凭据、不出现 S3 客户端。这与 #4 调研出的"所有沙箱先例都把凭据挡在执行边界之外"殊途同归，但走的是**函数调用**版本——因为 Worker 主动拨号、连接由控制层终结，brokering 对我们是一次调用，不是 TLS 中间人（Vercel 为同一件事要布 SNI 匹配 + TLS 拦截 + per-sandbox CA，还留着 domain fronting 的缺口）。

**Status**: accepted（`#12`）——数据面**怎么搬**后来改过两次（`#14` 去掉物化/收集、`#20` 换成按路径的文件面，见 `0006-path-addressed-file-surface.md`），本 ADR 的主张（凭据只交付给可信侧）不变。

## Considered Options

- **交付短期凭据进沙箱**（原 `#5` 的目标形态：STS 三元组 + GitHub App 安装令牌直接给容器）：保留"任务代码自己拿 SDK 直连资源"的自由度，但把凭据放进不可信侧，只能做到**缩小**爆炸半径。
- **出口代理 + 占位符替换**（E2B / Daytona / Blaxel / Cloudflare 那一派）：凭据不出边界，但需要 TLS 终结或按 host 白名单改写，且对 `https://` 上游的注入受 CONNECT 隧道限制。
- **控制层代为读写对象**：控制层沦为数据面瓶颈，且与 `#7`「控制层不中转字节」冲突。

## Consequences

- 沙箱内的任务代码**不能**直接使用 S3 SDK 或直接 `git push`；需要资源时必须走平台能力。这是用**自由度换保证**，是本决定的主要代价。
- 目标只在**可信侧**：宿主上执行的换发必须把凭据留在进程环境里，**绝不进 argv**（`/proc/<pid>/cmdline` 是同机任何用户可读），并且执行前要把 `credential.helper` 重置为空——否则宿主上已有的 `store` 类 helper 会把凭据写进 `~/.git-credentials`。
- 租约变为**一次操作一次**、用完即撤，续期机制不存在；授权撤销的时延因此有界（在飞操作最多活一次操作的时长）。代价是**每次操作都要控制层在场**。
- 契约里保留另一种 delivery（交付短期凭据给沙箱），不实现；将来若有任务确实需要直连资源，是改这一条，而不是重做架构。
- **先例的两侧**（2026-10-16 补，见 `docs/research/container-sandbox-architecture.md` §8.5 与 ⚠️8）：站在我们这边的是 **Argo Workflows**——对象存储凭据只挂给搬运的 init/wait/supervisor 容器，主容器没有，形状与我们同构。反面是四家四个形式，同一个取舍：DevPod 明写「forward git credentials to a remote machine」、Tekton 的 `git-clone` 用 `basic-auth`/`ssh-directory` **工作区**把凭据送进 step 容器、OpenHands 把 `OH_SESSION_API_KEYS_0` 写进容器环境、Agent Sandbox 的 `sandboxd` 在沙箱内校验 bearer token。下次有人问「为什么凭据不给沙箱」，这两组就是答案，不必重新论证。
