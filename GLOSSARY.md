# Stars Sandbox

智能体的远端工作环境：控制层下发任务，沙箱侧在隔离运行时里准备工作区、执行命令、操作 Git 与对象存储、调度 GPU。

## Language

**Sandbox（沙箱）**:
一个隔离运行时实例，其后端可替换，是执行发生的边界。
_Avoid_: 容器、环境、机器

**Workspace（工作区）**:
沙箱内一次任务的工作环境：代码仓库、检出状态与生命周期。智能体看见的是 Workspace，不是 Sandbox。
_Avoid_: 项目、目录、工作目录

**Runtime Provider（运行时后端）**:
把 Sandbox 落到具体隔离技术上的适配器：local、docker、podman。
_Avoid_: 调度器、驱动、driver

**Sandbox Scheduler（沙箱调度器）**:
决定一个 Workspace 落到哪个 Sandbox、哪台机器上的放置决策。
_Avoid_: 调度、Schedule、编排

**Control Plane（控制层）**:
向沙箱下发任务、换发凭证、做放置决策的一侧。沙箱主动拨号连接它。
_Avoid_: 服务端、后端、master

**Credential Broker（凭证代理）**:
按"谁授权了哪个仓库"换发短期、限定范围的凭据，使智能体与工作区代码都接触不到长期密钥。
_Avoid_: 密钥管理、Vault、token 服务

**Capability Surface（能力面）**:
智能体触达沙箱的方式：MCP 工具、CLI、Skill 文档。SDK 是面向定制智能体框架的可后置入口。
_Avoid_: API、接口、插件
