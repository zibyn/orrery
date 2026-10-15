# Stars Sandbox

智能体的远端工作环境：控制层下发任务，沙箱侧在隔离运行时里准备工作区、执行命令、操作 Git 与对象存储、调度 GPU。

## Language

**Sandbox（沙箱）**:
一个隔离运行时实例，其后端可替换，是执行发生的边界。
_Avoid_: 容器、环境、机器

**Workspace（工作区）**:
一份代码检出与它的生命周期，是智能体的工作场所。它在存续期内承载一个或多个 Task（同一时刻至多一个），并**比承载它的 Sandbox 活得久**：空闲时沙箱可销毁而目录留在宿主上，下次任务挂回同一台机器——智能体看见的是 Workspace，不是 Sandbox。
_Avoid_: 项目、目录、工作目录

**保留（Retention）**:
Workspace 在没有任务时的存续：**热**是沙箱仍活着，**冷**是沙箱已销毁、只剩宿主上的目录。两档的长度由控制层裁决。
_Avoid_: 缓存、暂停、休眠、快照、standby

**Task（任务）**:
下发到一个 Workspace 里的一次执行：一条命令、它的工作目录与环境、它的超时，以及它要回收的产物。它是控制层持有并仲裁的、可取消、可观测的执行单位；它不参与任何授权判定。
_Avoid_: 作业、job、run、build

**Runtime Provider（运行时后端）**:
把 Sandbox 落到具体隔离技术上的适配器：local、docker、podman。
_Avoid_: 调度器、驱动、driver

**Runtime Capabilities（运行时能力）**:
某个运行时后端在**这台机器上当下真能做到什么**：隔离强度、资源限制是否真的生效、可用的加速器、网络形态。它是后端自检后上报的**事实**，不是它承诺能做到的；Sandbox Scheduler 据此匹配需求。
_Avoid_: 能力清单、features、规格

**Sandbox Scheduler（沙箱调度器）**:
决定一个**新** Workspace 落到哪台 Worker、哪个 Sandbox 上的放置决策。已存在的 Workspace 不参与放置——它只能回到归属 Worker。
_Avoid_: 调度、Schedule、编排

**设备分配（Device Allocation）**:
把请求里的加速器**型号选择器**（`A100-40GB`）解析成**具体设备标识**（`nvidia.com/gpu=3`）的那一步。请求说的是"要什么样的卡"，放置记录说的是"哪张卡"。
_Avoid_: 卡号分配、GPU 调度

**Control Plane（控制层）**:
向沙箱下发任务、换发凭证、做放置决策的一侧。沙箱主动拨号连接它。
_Avoid_: 服务端、后端、master

**Worker（执行侧守护进程）**:
沙箱机器上主动拨号、登记进控制层的长时间运行进程。它是沙箱侧**唯一的认证主体**：承载任务、代表任务向 Credential Broker 取凭据。
_Avoid_: 节点、agent、runner

**Machine（机器）**:
一台物理或虚拟宿主，以 `machine_id` 作为**事实**被记录（一台机器上可以有多个 Worker）。它不参与任何授权判定。
_Avoid_: 节点、host

**归属 Worker（Origin Worker）**:
Workspace 目录所在的那台机器上的 Worker。Workspace 只能由它重建，它不在就没有这个 Workspace。
_Avoid_: 主 Worker、宿主 Worker

**授权标签（Authorization Labels）**:
控制层签发给 Worker 的标签，说明它**被允许**服务谁、载什么。它在登记时由控制层写入 Worker 身份，Worker 自己不能声明。它管的是"这台机器被允许承接什么"，与资源侧的**授权**不是一回事。
_Avoid_: 能力标签、tag

**放置标签（Placement Labels）**:
运维给的键值对，说明一台 Worker **希望**承载什么（分区、池、灰度批次）。它是**意志**；运行时能力是自检出来的**事实**，两者不可互相推断。
_Avoid_: 标签、亲和性、affinity

**登记包（Enrollment Package）**:
控制层签发的一次性、单次使用、短时有效的入网凭证，携带预生成的 Worker 身份、授权标签、runtime 后端与控制层 CA。Worker 用它换一份短期身份证书，此后只凭证书存续。
_Avoid_: join token、注册 token、bootstrap token

**授权（Grant）**:
某主体把某个资源（仓库、对象前缀）按一组操作与期限交给本系统使用。它是**人的意志**：长期存在、可撤销，凭据由它派生。
_Avoid_: 权限、ACL、授权标签（那说的是"这台 Worker 被允许服务谁"）

**租约（Lease）**:
从一份**授权**派生出的、**一次操作一次**的短期凭据（一次远端 git 动词，或一轮资源物化/收集）。它只交付给可信侧，用完即撤——不存在"活着的凭据"，因此没有续期。
_Avoid_: token、密钥、会话密钥

**Credential Broker（凭证代理）**:
按"谁**授权**了哪个仓库"，从授权派生**租约**，使资源被动用。原则是**凭据只交付给可信侧（Worker）**：不可信侧（Sandbox 里的代码）通过平台能力拿资源，不拿凭据。
_Avoid_: 密钥管理、Vault、token 服务

**Capability Surface（能力面）**:
智能体触达沙箱的方式：MCP 工具、CLI、Skill 文档。SDK 是面向定制智能体框架的可后置入口。
_Avoid_: API、接口、插件；运行时后端能做什么叫 `Runtime Capabilities`（运行时能力），不是本词。
