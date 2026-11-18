# 卷基准测试 harness

`../volume-vs-juicefs-bench.md` 的复测工具。在一台有 Docker 的机器上，把同一套 AI coding 负载分别跑在五个目标上：本地盘、卷（每 10 秒提交、每 60 秒提交）、JuiceFS 默认、JuiceFS `--writeback`。卷和 JuiceFS 共用同一个本机 RustFS。

| 文件 | 作用 |
|---|---|
| `bench.py` | 14 个负载阶段，每个阶段打印一行 `RESULT`，并带上探针延迟（每 10 ms 做一次 open+write+close） |
| `bench.sh` | 在容器里设置 Go、Node 环境，再调用 `bench.py` |
| `drive.sh` | 按 `$TARGETS` 逐个目标起容器运行；`BENCH_PHASES` 可以只跑其中几个阶段 |
| `jfs.sh` | 格式化一个新的 JuiceFS 卷，并在前台挂载 |
| `internal/resourcefs/volume_bench_test.go` | 卷目标的入口 `TestVolumeLocalBench`。按 Agent 的方式提交：seal → 并发上传对象（`castransfer.MaxUploads` 路，和 Agent 同一个常量）→ 上传 Generation manifest → `VolumeCommitted`，读写都走 RustFS。没有 `ORRERY_BENCH_SCRIPT` 时跳过，所以 `go test ./...` 和 CI 不会跑它 |
| `baseline-*.txt` | 2026-11-18 的原始输出（commit `4eecdeb`） |
| `after-*.txt` | spec #295 合并后的原始输出；`after-diag.txt` 是调研文档 §4 的诊断运行 |

## 准备

```bash
B=/var/tmp/orrery-bench; mkdir -p $B/rustfs $B/bin $B/jfsmnt $B/tmp; chmod 777 $B/rustfs
docker run -d --name bench-redis --network host redis:7.4 redis-server --port 6390 --save '' --appendonly no
docker run -d --name bench-rustfs --network host -v $B/rustfs:/data -e RUSTFS_ACCESS_KEY=bench \
  -e RUSTFS_SECRET_KEY=benchbench -e RUSTFS_ADDRESS=:9010 -e RUSTFS_CONSOLE_ENABLE=false rustfs/rustfs:latest
docker pull juicedata/mount:ce-v1.2.3
tar -C web -cf $B/nm.tar node_modules src/paraglide          # pnpm install 之后
go test -c -o $B/bin/resourcefs-s3.test ./internal/resourcefs
```

运行容器默认用 `orqestra-sandbox:q3pkgs` 镜像（Ubuntu 24.04，带 git、python3、rg）。Node 和 Go 工具链从宿主挂进容器。

## 运行

```bash
docs/research/volume-bench/drive.sh | tee results.txt      # 全部五组，约 50 分钟
TARGETS=volume-60s BENCH_PHASES="install_node agent_edit seqio" docs/research/volume-bench/drive.sh
```

`search`、`seqio` 和 `parallel_io` 会清整台宿主的页缓存（`drop_caches`）。运行期间宿主上不要有别的重负载，否则 CPU 密集的阶段和 RustFS 都会变慢。

`clone` 阶段克隆的是 `drive.sh` 所在的仓库（挂进容器的 `/src`）。在 git worktree 里，容器看不到 worktree 的 gitdir，所以先 `git clone` 出一份普通仓库，再运行那份里的 `drive.sh`。本地 `git clone` 会原样复制源仓库的松散对象（基线和复测时都约 4,300 个），它们决定 `clone`、`rg` 和 `git gc` 的工作量，所以复测前不要对源仓库 `git gc`。

## 中途停止

卷的 daemon 如果先于客户端退出，容器会卡在 D 状态的进程上，删不掉。这时找到这次测试自己挂载对应的 `/sys/fs/fuse/connections/<n>`（在 mountinfo 里查它的设备号 `0:<n>`），在特权容器里往 `abort` 写 1。
