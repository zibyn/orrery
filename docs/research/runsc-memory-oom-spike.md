# runsc 的内存上限与 OOM：客户机能不能自己 OOM，谁被宿主杀掉（spike #177 / #173）

只回答 `#177` 的四个问题，给 `#173` 选方案用，不改 spec。每条结论都给出处与在 `xzai-a800-8` 上跑出来的输出。

**可信度分档**：
- **【源码读出】** gVisor `release-20261026.0`（tag 对象 `61bae0ec`，提交 `e8a2a28891ba`）的源码文件 + 行号。
- **【真机实测】** 在 `xzai-a800-8` 上实际跑出来的输出（2026-10-31）。

**实测环境**：同 `containerd-cdi-runsc-cni-spike.md`（Ubuntu 22.04.5，kernel `5.15.0-119-generic`，cgroup v2，containerd `2.2.1`，runsc `release-20261026.0`，平台 systrap）。namespace `orrery-spike177`，镜像 `alpine:3.22`。每个 Sandbox 用 `ctr run -d --runtime io.containerd.runsc.v1 --memory-limit 134217728 --mount type=cgroup,src=cgroup,dst=/sys/fs/cgroup,options=ro`，启动后在宿主上把 `/sys/fs/cgroup/orrery-spike177/<id>/memory.swap.max` 写成 0（与 Agent 的 `oci.WithMemorySwap(memory)` 等效）。两种吃内存的负载：

- `dbl`：`#168` e2e `TestExecOOMKilled` 的命令，`sh -c 'a=xxxxxxxxxxxxxxxx; while :; do a="$a$a"; done'`。内存由客户机在用户态写入。
- `pipe`：`#173` 分诊用的 `head -c 600m /dev/zero | tail`。内存由 Sentry 在 `read()` 里拷进客户机缓冲区。

两种进程树：
- **ctr 形状**：PID 1 为 `sleep 86400`，负载用 `ctr task exec` 起。
- **Agent 形状**：用 `origin/containerd-160`（`e7d5bc69`）编出的 `orrery-agent`，挂成 `/.orrery/bin/supervisor`（ext4 上的 bind 挂载，只读），PID 1 为 `supervisor init`；`ctr task exec -d … supervisor run hog`，再从宿主经 `ctl.sock` 发 `{"argv":[…]}`（runsc 选项 `host-uds = "create"`，与 Agent 一样经 `--runtime-config-path` 传 TOML）。另挂 `/.orrery/run` 和 `/workspace`。结果读 `run/hog/exit`。没有起真正的 Agent 和 Control Plane。

谁被杀，看 `journalctl -k` 里的 `Killed process <pid>`，和启动时 `ctr task ls` 给出的 task PID（就是 `runsc-sandbox`，即 Sentry）比对。

## 0. 结论（先读这里）

| # | 问题 | 结论 | 机制 |
|---|---|---|---|
| 1 | runsc 能否把客户机内存限在宿主上限之下，让 Sentry 自己在客户机里 OOM 杀进程 | **否** | runsc 没有客户机内存上限。`--total-memory` 是 runsc 从 cgroup 的 `memory.limit` 推出来的内部 boot 参数，只决定 `/proc/meminfo` 的 `MemTotal` 和模拟 cgroupfs 里 `memory.limit_in_bytes` 显示的数，不执行。没有配置项或 annotation 能单独设它。Sentry 里没有 OOM killer；模拟的 v2 `memory.events` 是写死的全 0。实测：spec 上限 128 MiB、宿主 `memory.max` 改成 512 MiB 后，客户机照样建出 128 MiB 的字符串（拷贝时约 256 MiB），客户机里什么都没发生。客户机进程被杀时看到的是宿主 OOM killer 杀了它的 systrap stub 进程，Sentry 随后以 SIGKILL 结束该线程组（退出码 137）。客户机里看不到任何 OOM 信号，宿主上 `memory.events` 的 `oom_kill` 和内核日志能看到。 |
| 2 | 宿主上限需要比客户机内存多留多少给 Sentry/gofer | **与客户机内存无关，约 20 MiB 固定 + 每个客户机进程约 0.5 MiB**；但对方案 B 没有意义 | 宿主 `memory.current` 减客户机 `MemTotal-MemFree`：客户机 64/256/1024 MiB 时分别多 19/22/26 MiB。每增加 100 个客户机进程，宿主多约 47 MiB（客户机自己只记了约 6 MiB），宿主任务数多约 215 个。Agent 形状空闲时宿主约 29 MiB（Supervisor 二进制的文件页进了 Sentry 的 memfd）。因为客户机侧没有上限（Q1），留多少余量都不会换来客户机内 OOM，只会让客户机长到宿主上限再被宿主杀。 |
| 3 | 为什么 #168 的 e2e 3 次里 2 次是 `exited` + 信号，而纯复现总是丢 Sandbox | **宿主 OOM killer 在 Sentry 和负载的 stub 之间二选一，看谁的 RSS 大** | cgroup 里所有进程的 `oom_score_adj` 都是 -999（继承自 shim），选 RSS 最大的。客户机在用户态写的页记在负载 stub 的 `shmem-rss` 上（`dbl`）；Sentry 替客户机拷进去的页记在 Sentry 自己身上（`pipe`）。ctr 形状：`dbl` 5/5 杀 stub，Exec 退出 137，Sandbox 存活；`pipe` 5/5 杀 Sentry，Sandbox 丢失。Agent 形状下 `dbl` 6/6 杀 Sentry：Go 写的 Supervisor 的文件页和 stub 占了 cgroup 里一截，负载 stub 只长到约 35 MiB，比 Sentry 的约 42 MiB 小。两者差距很小，e2e 里出现 2:1 符合这种近乎平局的情况。 |
| 4 | 宿主 cgroup 上的 `memory.oom.group` 有没有影响 | **有，但只会更糟：每次都丢 Sandbox** | `memory.oom.group=1` 时内核杀掉 cgroup 里所有 `oom_score_adj` 不为 -1000 的进程，包括 Sentry 和 gofer。原本能保住 Sandbox 的 `dbl`（ctr 形状）3/3 变成丢 Sandbox，`oom_kill` 一次涨 4 到 6（每杀一个进程记一次）。它让结果变确定，不改变内存记账。 |

**对 #173 的建议：方案 A**，但要改一下表述：runsc 下撞到内存上限，结果是「那个 Exec 被 SIGKILL」或「整个 Sandbox 丢失」，二者取决于负载，平台不保证是哪一个；两种情况的原因都由 Agent 读宿主 `memory.events` 的 `oom_kill` 来判定。方案 B 不成立：runsc 没有低于宿主上限的客户机内存上限（Q1）。不要设 `memory.oom.group`。§5 记了一个能把多数情况变成「只杀 Exec」的旋钮（Sentry 和 gofer 的 `oom_score_adj = -1000`），它不是 B，也不能消除丢 Sandbox。

## 1. 客户机内存上限与客户机内 OOM（Q1）

### 1.1 `--total-memory` 只是报数

**【源码读出】** runsc 起 Sentry 时自己算 `--total-memory`：取宿主总内存，若 cgroup 有内存上限且更小就取上限（`runsc/sandbox/sandbox.go:1496-1525`）：

```go
	mem := totalSysMem
	if s.CgroupJSON.Cgroup != nil {
		...
		memLimit, err := s.CgroupJSON.Cgroup.MemoryLimit()
		...
		if memLimit < mem {
			mem = memLimit
		}
	}
	cmd.Args = append(cmd.Args, "--total-memory", strconv.FormatUint(mem, 10))
```

这是 `runsc boot` 的内部参数（`runsc/cmd/sentry/sentrycmd/boot.go:285`），不在 `runsc/config/flags.go` 里，所以不能经 `[runsc_config]` 设。`runsc/specutils` 和 shim 里也没有内存相关的 annotation（只有 `dev.gvisor.spec.cgroup-parent`、`dev.gvisor.flag.host-uds` 等）。

Sentry 拿它做什么（`runsc/boot/loader.go:861-866`）：

```go
	if args.TotalMem > 0 {
		// Adjust the total memory returned by the Sentry so that applications that
		// use /proc/meminfo can make allocations based on this limit.
		usage.MinimumTotalMemoryBytes = args.TotalMem
		usage.MaximumTotalMemoryBytes = args.TotalMem
```

`MaximumTotalMemoryBytes` 只在 `pkg/sentry/usage/memory.go:470-487 TotalMemory()` 里给「报给应用的总内存」封顶，以及 `runsc/boot/vfs.go:1530-1535` 给模拟 cgroupfs 的 `memory.limit_in_bytes` 设默认值。整个源码里没有别的读取者，也就是说没有任何分配路径拿它比较。

### 1.2 模拟的 cgroup 都是桩

**【源码读出】**
- v1（默认，`in-sandbox-cgroup = "v1"`）：`pkg/sentry/fsimpl/cgroupfs/memory.go:87-89`，`memory.limit_in_bytes`、`memory.soft_limit_in_bytes` 都是 `newStubControllerFile`，写进去只是存一个数。没有 `memory.oom_control`。
- v2（`in-sandbox-cgroup = "v2"`）：`pkg/sentry/fsimpl/cgroup2fs/memory.go:99-101`：

```go
func (me *memoryEvents) Generate(ctx context.Context, buf *bytes.Buffer) error {
	buf.WriteString("low 0\nhigh 0\nmax 0\noom 0\noom_kill 0\noom_group_kill 0\nsock_throttled 0\n")
```

- Sentry 的内存文件不读宿主 memcg 压力（`runsc/boot/loader.go:1236-1238`：进了挂载命名空间后看不到宿主 cgroupfs）。`pgalloc` 的 `ENOMEM`（`pkg/sentry/pgalloc/pgalloc.go:883-885`）只在 memfd 的 chunk 数超出地址空间上限时出现，与 cgroup 无关。
- `pkg/sentry` 里没有 OOM killer。`oom_score_adj`（`pkg/sentry/kernel/task.go:959-971`）只存不用。

### 1.3 实测：客户机侧什么都不执行

**【真机实测】** spec 上限 128 MiB，启动后把宿主 `memory.max` 改成 512 MiB：

```
MemTotal:         131072 kB                      # /proc/meminfo，来自 --total-memory
134217728                                        # /sys/fs/cgroup/memory/memory.limit_in_bytes（桩）
string_bytes=134217728                           # a="$a$a" 23 次，拷贝时客户机约 256 MiB
rc=0
host qa: max 0 oom_kill 0
```

`in-sandbox-cgroup = "v2"`，`--mount type=cgroup2,…`，在 `/sys/fs/cgroup/<id>/memory.events` 上读，`dbl` 负载被宿主杀掉（`rc=137`，宿主 `oom_kill 1`）前后都是：

```
low 0 high 0 max 0 oom 0 oom_kill 0 oom_group_kill 0 sock_throttled 0
```

同一子树下 `memory.max` 读出 `max`，连上限都不反映。所以换成 v2 也给不了 Supervisor 一个会涨的计数。

（顺带：同一 Sandbox 里 `head -c 300m /dev/zero > /dev/shm/f` 报 `I/O error`，是客户机 tmpfs 自己的大小上限，不是内存上限。）

### 1.4 被杀的客户机进程看到什么

**【源码读出】** systrap 平台上每个客户机地址空间对应宿主上的 stub 进程，客户机内存是 Sentry memfd 映射进 stub 的。宿主杀掉 stub 后，Sentry 切回应用时拿到 `errDeadSubprocessContext`（`pkg/sentry/platform/systrap/subprocess.go:897-901`、`shared_context.go:256-260`），落到 `pkg/sentry/kernel/task_run.go:395-399`：

```go
	default:
		// What happened? Can't continue.
		t.Warningf("Unexpected SwitchToApp error: %v", err)
		t.PrepareGroupExit(linux.WaitStatusTerminationSignal(linux.SIGKILL))
```

即只结束这一个客户机线程组，状态是「被 SIGKILL 终止」。父进程（Supervisor）`wait` 得到 SIGKILL，与 runc 下被 OOM 杀的样子一样。另有一条路径会让 Sentry 自杀：stub 在 ptrace 等待中被 SIGKILL 时走 `unexpectedStubExit()`（`subprocess.go:679-692`），调 `sighandling.KillItself()`。本次的 14 次「杀 stub」都没有走到它，但不能排除。

**【真机实测】** ctr 形状 `dbl`：`ctr task exec` 返回 `137`，task 仍是 `RUNNING`，宿主 `memory.events` 为 `oom 1 oom_kill 1`。Agent 形状（§5 的设置下）`run/hog/exit` 为 `{"cause":"exited","signal":9,…}`。

**可见的 OOM 信号**：客户机里没有（§1.2）。宿主上有两处：`/sys/fs/cgroup/<cgroupsPath>/memory.events` 的 `oom_kill` 计数，和内核日志 `Memory cgroup out of memory: Killed process <pid> …`。日志里的进程名都是 `3`（Sentry 和 stub 同名，gofer 叫 `exe`），要靠 PID 区分。

## 2. Sentry/gofer 的开销（Q2）

**【真机实测】** 宿主上限 4 GiB（不触发 OOM）。`host_current` 是 `memory.current`，`guest_used` 是客户机 `/proc/meminfo` 的 `MemTotal - MemFree`，`anon/shmem` 取自宿主 `memory.stat`，单位 MiB：

```
idle:                         host_current=18   anon=16 shmem=0    guest_used=1    host_tasks=54
anon string 64MiB:            host_current=84   anon=17 shmem=64   guest_used=65   host_tasks=58
anon string 256MiB:           host_current=279  anon=19 shmem=256  guest_used=257  host_tasks=63
anon string 1024MiB:          host_current=1051 anon=21 shmem=1024 guest_used=1025 host_tasks=68
procs=0:                      host_current=19   anon=17 shmem=0    guest_used=1    host_tasks=53
procs=100 (sleep):            host_current=70   anon=37 shmem=12   guest_used=7    host_tasks=270
procs=200:                    host_current=115  anon=52 shmem=24   guest_used=13   host_tasks=481
procs=300:                    host_current=165  anon=71 shmem=36   guest_used=19   host_tasks=694
procs=400:                    host_current=212  anon=88 shmem=47   guest_used=25   host_tasks=909
supervisor init idle:         host_current=29   anon=18 shmem=8    guest_used=32   host_tasks=62
```

- 客户机内存全部以 `shmem`（Sentry 的 memfd）记在宿主 cgroup 上，1:1。
- 宿主比客户机多出的部分：随客户机内存几乎不变（19 → 26 MiB，增量是 Sentry 自己的 anon 和页表）；随进程数线性增长，每个客户机进程约 0.5 MiB、约 2.1 个宿主任务（stub 和 Sentry 的线程）。
- 线程数没有单独测（busybox 起不了多线程程序）。按 systrap 每个客户机线程一个 stub 线程、每个地址空间一个 stub 进程的结构推断，线程的开销小于进程。

如果有客户机侧的上限，宿主上限应留「约 20 MiB + 0.5 MiB × 预计进程数」。但 Q1 说明没有这样的上限，余量只会被客户机吃掉。

## 3. #168 e2e 里谁被杀（Q3）

### 3.1 选谁：RSS 最大的那个

cgroup 里每个进程的 `oom_score_adj` 都是 -999（`runsc-gofer`、`runsc-sandbox` 和所有 stub 都继承自 shim），所以内核按 RSS（anon + file + shmem）挑最大的。共享的 memfd 页在宿主 memcg 里只记一次，但在每个映射它的进程的 RSS 里都算：
- 客户机在用户态写的页，是在 stub 的地址空间里缺页，记在 **stub** 的 `shmem-rss` 上。
- Sentry 在系统调用里替客户机写的页（`read()` 的 copyout、pipe、tmpfs、文件缓存），走 Sentry 自己对 memfd 的映射，记在 **Sentry** 上。

### 3.2 ctr 形状

**【真机实测】** 每种负载 5 次，全部一致：

```
d1 wl=dbl  exec_rc=137 task=RUNNING sentry=3024479 killed=[3024993] role=[other]   oom_kill 1    （d2–d5 相同）
p1 wl=pipe exec_rc=128 task=STOPPED sentry=3028454 killed=[3028454] role=[sentry]  oom_kill 1    （p2–p5 相同）
```

`dbl` 被杀的 stub：`total-vm:133004kB, anon-rss:28kB, file-rss:836kB, shmem-rss:75948kB`。
`pipe` 被杀的 Sentry：`total-vm:3608468kB, anon-rss:18608kB, file-rss:25800kB, shmem-rss:102936kB`；同一时刻 stub 的 RSS 都只有几页到 261 页。`pipe` 时 `ctr` 报 `urpc method "containerManager.WaitPID" failed: EOF`，就是 `runtime_lost`。

### 3.3 Agent 形状

**【真机实测】** `dbl`，6 次：

```
s1 wl=dbl task=STOPPED exit=none killed=[3041510] role=[sentry] oom_kill 1    （s2–s6 相同）
```

被杀的 Sentry：`anon-rss:13760kB, file-rss:25692kB, shmem-rss:3028kB`（约 42 MiB）。OOM 时的任务表（页数）：Sentry `rss 10556`；`supervisor init` 和 `supervisor run` 的 stub 各有一组、每个 `rss` 约 6000（Go 二进制的文件页约 16 MiB 加 shmem 约 7.5 MiB）；负载 `sh` 的 stub `rss 8786`（约 34 MiB）。也就是说，128 MiB 里有一截被 Supervisor 的文件页、Go 堆和 Sentry 自己占掉，负载只长到约 34 MiB 就撞线，比 Sentry 小一点，于是 Sentry 被选中。

这和 e2e 的 2 次 `exited` + 信号、1 次 `runtime_lost` 是同一个机制：负载 stub 和 Sentry 的 RSS 很接近（34 vs 42 MiB），谁大谁死。真实 Agent 的 Supervisor 二进制、Workspace 挂载和页缓存的冷热和我手搭的不完全一样，结果就会翻转。e2e 里的 `exited` + 信号就是负载 stub 被杀、Sentry 以 SIGKILL 结束 `sh`（§1.4），信号是 9。

## 4. `memory.oom.group`（Q4）

**【真机实测】** 在宿主 cgroup 上 `echo 1 > memory.oom.group`：

```
g1 wl=dbl exec_rc=128 task=STOPPED killed=[stub gofer sentry stub stub stub] oom_kill 6
g2 wl=dbl exec_rc=128 task=STOPPED killed=[…6 个…]                          oom_kill 6
g3 wl=dbl exec_rc=128 task=STOPPED killed=[…4 个…]                          oom_kill 4
gs1 (Agent 形状) task=STOPPED killed=[sentry gofer stub stub stub]          oom_kill 5
gs2 (Agent 形状) task=STOPPED killed=[sentry gofer stub]                    oom_kill 3
```

内核先选出受害者，再杀掉整个 cgroup 里 `oom_score_adj != -1000` 的进程。gofer 和 Sentry 一起死，所以每次都丢 Sandbox，包括没有它时能保住的 `dbl`。`oom_kill` 每杀一个进程加 1，所以「涨了多少」不能当 OOM 次数用，只能用「涨没涨」。它不影响 Q1、Q2。

## 5. 一个能保住多数 Sandbox 的旋钮（不是方案 B）

**【真机实测】** 启动后在宿主上把 Sentry 和 gofer 的 `/proc/<pid>/oom_score_adj` 写成 -1000（OOM killer 不会选它们），stub 保持 -999。之后新建的 stub 是从最早的 stub 克隆出来的，仍是 -999（实测 `ppid` 链和数值都确认了）：

```
xd1–3 (ctr, dbl)    exec_rc=137 task=RUNNING  killed=[负载 stub]           oom_kill 1
xp1–3 (ctr, pipe)   task=RUNNING，杀了 1–2 个 RSS 只有几页的 stub（head 或 tail），Sandbox 存活
xs1–3 (Agent, dbl)  task=RUNNING  exit={"cause":"exited","signal":9,…}   oom_kill 1
xq1–3 (Agent, pipe) task=STOPPED  先后杀了 supervisor run 和 supervisor init 的 stub（各约 24 MiB RSS），init 一死 Sandbox 就停了
```

- 用户态吃内存的负载（e2e 的 `dbl`，以及大多数解释器、编译器）变成「只杀这个 Exec，结果是 `exited` + 9」，宿主 `oom_kill` 涨 1。这正好是 `runtime.md` §4 第 7 步的判定条件，只是计数要在宿主上读。
- 内存由 Sentry 替客户机持有的负载（大量 `read()`、pipe、tmpfs），stub 都很小，OOM killer 会挑 RSS 最大的 stub，往往是 Go 写的 Supervisor 或 PID 1，结果仍是丢 Sandbox；中途还可能误杀与此无关的客户机进程。
- 设置方式（**源码读出，没实测这条路径**）：runsc 会把 OCI spec 的 `process.oomScoreAdj` 写给 Sentry（`runsc/container/container.go:649-656`、`2077-2140 adjustSandboxOOMScoreAdj`）和 gofer（`2068-2075 adjustGoferOOMScoreAdj`），所以 Agent 在 spec 里设 `oomScoreAdj = -1000` 应该等效于上面手写 `/proc`。stub 是否也会拿到 -1000（那样就什么都杀不了）要实测：用 `ctr run --config` 自写 spec 在本机没跑起来（runsc 报 `cannot read client sync file`，与这个字段无关），留给实现时用 Go 客户端验证。客户机里看到的 `oom_score_adj` 是 Sentry 自己的，不受影响。
- 代价：整个 Sentry 不可被 OOM 杀。如果 cgroup 里只剩 Sentry 持有的内存（tmpfs 写满）且 stub 都杀光，PID 1 的 stub 也在其中，init 死了 Sandbox 照样结束，没有观察到卡死。

这不是方案 B（没有余量，也没有客户机内 OOM），而是方案 A 里一个可选的改进：把「Exec 被杀」从约一半变成用户态负载的常态，「丢 Sandbox」仍然存在。要不要做，是 spec 层面的取舍，留给 `#173`。

## 6. 复现命令

辅助函数（宿主上，`NS=orrery-spike177`，`CG=/sys/fs/cgroup/$NS`）：

```sh
start() { id=$1; mem=$2; shift 2
  ctr -n $NS run -d --runtime io.containerd.runsc.v1 --memory-limit $mem \
    --mount type=cgroup,src=cgroup,dst=/sys/fs/cgroup,options=ro "$@" docker.io/library/alpine:3.22 $id sleep 86400
  echo 0 > $CG/$id/memory.swap.max; }
# Q3：谁被杀
start d1 134217728; sentry=$(ctr -n $NS task ls | awk '$1=="d1"{print $2}'); t0=$(date +%s)
ctr -n $NS task exec --exec-id hog d1 sh -c 'a=xxxxxxxxxxxxxxxx; while :; do a="$a$a"; done'; echo rc=$?
journalctl -k --since @$t0 | grep 'Killed process'; echo sentry=$sentry; ctr -n $NS task ls
# Q4：再加一行
echo 1 > $CG/<id>/memory.oom.group
# §5：Sentry 与 gofer 不可被选
echo -1000 > /proc/$sentry/oom_score_adj; echo -1000 > /proc/<runsc-gofer pid>/oom_score_adj
```

Agent 形状在此基础上加 `--runtime-config-path <TOML，host-uds = "create">`，三个 bind 挂载（`/.orrery/bin` 只读、`/.orrery/run`、`/workspace`），PID 1 为 `/.orrery/bin/supervisor init`，然后：

```sh
ctr -n $NS task exec -d --exec-id hog <id> /.orrery/bin/supervisor run hog
echo '{"argv":["sh","-c","a=xxxxxxxxxxxxxxxx; while :; do a=\"$a$a\"; done"]}' | socat - UNIX-CONNECT:<run>/hog/ctl.sock
cat <run>/hog/exit
```

注意 busybox `sh -c '…; sleep 600'` 会把最后一条命令 exec 掉、释放 shell 的内存，量 Q2 时要在末尾加 `; true`。
