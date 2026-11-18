#!/usr/bin/env python3
"""AI-native coding workloads on one directory. Usage: bench.py <dir> [phase ...]

Every phase runs under a probe (open+write+close of a tiny file every 10 ms),
so each RESULT line also carries how responsive the mount stayed meanwhile.
"""
import os, sys, time, subprocess, threading, multiprocessing as mp, sqlite3

MiB = 1 << 20
D = sys.argv[1]
REPO = os.path.join(D, "repo")
TIMEOUT = int(os.environ.get("BENCH_TIMEOUT", "1200"))


def sh(*a, cwd=None):
    """Run a command; returns seconds, or raises Timeout/CalledProcessError."""
    t0 = time.perf_counter()
    subprocess.run(a, cwd=cwd or REPO, check=True, timeout=TIMEOUT,
                   stdout=subprocess.DEVNULL, stderr=subprocess.PIPE)
    return round(time.perf_counter() - t0, 2)


def drop_caches():
    os.sync()
    with open("/proc/sys/vm/drop_caches", "w") as f:
        f.write("3")


class Probe:
    def __init__(self):
        self.path, self.lat, self.stop = os.path.join(D, ".probe"), [], False
        self.t = threading.Thread(target=self.run, daemon=True)

    def run(self):
        while not self.stop:
            t0 = time.perf_counter()
            with open(self.path, "w") as f:
                f.write("x")
            self.lat.append(time.perf_counter() - t0)
            time.sleep(0.01)

    def __enter__(self):
        self.t.start()
        return self

    def __exit__(self, *a):
        self.stop = True
        self.t.join()

    def summary(self):
        l = sorted(self.lat)
        if not l:
            return {}
        return dict(probe_p50_ms=f"{l[len(l)//2]*1e3:.2f}", probe_p99_ms=f"{l[int(len(l)*.99)]*1e3:.1f}",
                    probe_max_ms=f"{l[-1]*1e3:.0f}")


def pct(xs, p):
    xs = sorted(xs)
    return xs[min(len(xs) - 1, int(len(xs) * p))]


# ---- 1. 获取代码 -----------------------------------------------------------

def clone():
    return dict(clone_s=sh("git", "clone", "-q", "--no-hardlinks", "/src", REPO, cwd=D))


# ---- 2. 安装依赖 -----------------------------------------------------------

def install_node():
    # The real node_modules of web/ (22.6k files, 514 MiB), as npm would lay it down.
    return dict(files=22622, extract_s=sh("tar", "-xf", "/bench/nm.tar", cwd=os.path.join(REPO, "web")))


def venv():
    return dict(venv_with_pip_s=sh("python3", "-m", "venv", os.path.join(REPO, ".venv")))


# ---- 3. 代码检索 -----------------------------------------------------------

def search():
    drop_caches()
    cold = sh("rg", "-c", "--no-ignore", "--hidden", "useState", ".")
    sh("rg", "-c", "--no-ignore", "--hidden", "useState", ".")
    warm = sh("rg", "-c", "--no-ignore", "--hidden", "useState", ".")
    src = sh("rg", "-c", "func ", "internal")  # what an agent greps most: the repo's own code
    drop_caches()
    t0 = time.perf_counter()
    n = sum(len(fs) for _, _, fs in os.walk(REPO))
    walk = round(time.perf_counter() - t0, 2)
    return dict(rg_all_cold_s=cold, rg_all_warm_s=warm, rg_src_warm_s=src, files=n, walk_cold_s=walk)


# ---- 4. 构建与测试 ---------------------------------------------------------

def go_build():
    cold = sh("go", "build", "./...")
    with open(os.path.join(REPO, "internal/manifest/manifest.go"), "a") as f:
        f.write("\n// bench\n")
    incr = sh("go", "build", "./...")
    test = sh("go", "test", "-count=1", "./internal/manifest/...", "./internal/generation/...")
    return dict(build_cold_s=cold, build_incr_s=incr, test_s=test)


def web_build():
    web = os.path.join(REPO, "web")
    return dict(tsc_s=sh("node_modules/.bin/tsc", "--noEmit", cwd=web),
                vite_build_s=sh("node_modules/.bin/vite", "build", cwd=web))


# ---- 5. Agent 编辑循环 -----------------------------------------------------

def agent_edit(iters=200):
    """Read a file, write it back through tmp+rename (how agent edit tools save), then git status."""
    files = subprocess.run(["git", "ls-files", "*.go"], cwd=REPO, capture_output=True, text=True, check=True).stdout.split()
    lat = []
    for i in range(iters):
        t0 = time.perf_counter()
        p = os.path.join(REPO, files[i % len(files)])
        with open(p) as f:
            body = f.read()
        tmp = p + ".tmp"
        with open(tmp, "w") as f:
            f.write(body + f"\n// edit {i}\n")
        os.rename(tmp, p)
        subprocess.run(["git", "status", "--porcelain"], cwd=REPO, check=True, stdout=subprocess.DEVNULL)
        if i % 10 == 9:
            subprocess.run(["git", "diff", "--stat"], cwd=REPO, check=True, stdout=subprocess.DEVNULL)
        lat.append(time.perf_counter() - t0)
    commit = sh("bash", "-c", "git add -A && git commit -qm bench")
    return dict(edit_p50_ms=f"{pct(lat, .5)*1e3:.0f}", edit_p99_ms=f"{pct(lat, .99)*1e3:.0f}",
                edit_total_s=f"{sum(lat):.1f}", commit_s=commit)


# ---- 6. Git 操作 -----------------------------------------------------------

def git_ops():
    old = subprocess.run(["git", "rev-list", "HEAD"], cwd=REPO, capture_output=True, text=True, check=True).stdout.split()[300]
    sh("git", "status")
    return dict(status_warm_s=sh("git", "status"),
                checkout_old_s=sh("git", "checkout", "-q", old),
                checkout_back_s=sh("git", "checkout", "-q", "-"),
                log_p_s=sh("git", "log", "-p", "-n", "300"),
                gc_s=sh("git", "gc", "-q"))


# ---- 7. 小文件元数据 -------------------------------------------------------

def smallfiles(n=20000, size=4096):
    root = os.path.join(D, "small")
    data = os.urandom(size)
    t0 = time.perf_counter()
    for i in range(n):
        sub = os.path.join(root, str(i % 100))
        if i < 100:
            os.makedirs(sub, exist_ok=True)
        with open(os.path.join(sub, f"f{i}"), "wb") as f:
            f.write(data)
    create = time.perf_counter() - t0
    t0 = time.perf_counter()
    for dp, _, fs in os.walk(root):
        for f in fs:
            os.stat(os.path.join(dp, f))
    stat = time.perf_counter() - t0
    t0 = time.perf_counter()
    for dp, _, fs in os.walk(root):
        for f in fs:
            with open(os.path.join(dp, f), "rb") as fh:
                fh.read()
    read = time.perf_counter() - t0
    rm = sh("rm", "-rf", root, cwd=D)
    return dict(create_per_s=int(n / create), stat_per_s=int(n / stat), read_per_s=int(n / read), rm_s=rm)


# ---- 8. 应用数据库 ---------------------------------------------------------

def sqlite_tx(n=1000):
    out = {}
    for mode in ("delete", "wal"):
        p = os.path.join(D, f"app-{mode}.db")
        db = sqlite3.connect(p, isolation_level=None)
        db.execute(f"pragma journal_mode={mode}")
        db.execute("create table t(id integer primary key, v text)")
        t0 = time.perf_counter()
        for i in range(n):
            db.execute("insert into t(v) values (?)", ("x" * 200,))
        out[f"{mode}_tx_per_s"] = int(n / (time.perf_counter() - t0))
        db.close()
        for s in ("", "-wal", "-shm", "-journal"):
            if os.path.exists(p + s):
                os.remove(p + s)
    return out


# ---- 9. 大文件与检查点 -----------------------------------------------------

def _write(path, total, bs=MiB):
    buf = os.urandom(bs)
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o644)
    for _ in range(total // bs):
        os.write(fd, buf)
    os.fsync(fd)
    os.close(fd)


def _read(path, bs=MiB):
    with open(path, "rb", buffering=0) as f:
        while f.read(bs):
            pass


def seqio(total=4096 * MiB):
    p = os.path.join(D, "seq.bin")
    t0 = time.perf_counter()
    _write(p, total)
    w = int(total / MiB / (time.perf_counter() - t0))
    drop_caches()
    t0 = time.perf_counter()
    _read(p)
    r = int(total / MiB / (time.perf_counter() - t0))
    os.remove(p)
    return dict(write_MiB_s=w, read_MiB_s=r)


def parallel_io(jobs=8, each=512 * MiB):
    paths = [os.path.join(D, f"par{i}.bin") for i in range(jobs)]
    t0 = time.perf_counter()
    with mp.Pool(jobs) as pool:
        pool.starmap(_write, [(p, each) for p in paths])
    w = int(jobs * each / MiB / (time.perf_counter() - t0))
    drop_caches()
    t0 = time.perf_counter()
    with mp.Pool(jobs) as pool:
        pool.map(_read, paths)
    r = int(jobs * each / MiB / (time.perf_counter() - t0))
    for p in paths:
        os.remove(p)
    return dict(jobs=jobs, write_MiB_s=w, read_MiB_s=r)


def append(rounds=6, chunk=1024 * MiB, pause=4):
    """A training checkpoint or log that keeps growing across commits."""
    p = os.path.join(D, "grow.bin")
    buf = os.urandom(MiB)
    times = []
    for _ in range(rounds):
        t0 = time.perf_counter()
        fd = os.open(p, os.O_WRONLY | os.O_CREAT | os.O_APPEND, 0o644)
        for _ in range(chunk // MiB):
            os.write(fd, buf)
        os.fsync(fd)
        os.close(fd)
        times.append(time.perf_counter() - t0)
        time.sleep(pause)
    os.remove(p)
    return dict(round_s="/".join(f"{t:.1f}" for t in times))


# ---- 10. 清理 --------------------------------------------------------------

def cleanup():
    return dict(rm_repo_s=sh("rm", "-rf", REPO, cwd=D))


PHASES = ["clone", "install_node", "venv", "search", "go_build", "web_build", "agent_edit", "git_ops",
          "smallfiles", "sqlite_tx", "seqio", "parallel_io", "append", "cleanup"]

if __name__ == "__main__":
    for name in sys.argv[2:] or PHASES:
        with Probe() as pr:
            t0 = time.perf_counter()
            try:
                kv = globals()[name]()
            except subprocess.TimeoutExpired:
                kv = dict(status="TIMEOUT")
            except Exception as e:  # keep going: one broken phase must not hide the rest
                err = getattr(e, "stderr", b"") or b""
                kv = dict(status="ERROR", error=repr(e)[:120], stderr=err[-200:].decode(errors="replace").replace("\n", " "))
            wall = time.perf_counter() - t0
        kv.update(pr.summary(), wall_s=f"{wall:.1f}")
        print("RESULT", name, " ".join(f"{k}={v}" for k, v in kv.items()), flush=True)
