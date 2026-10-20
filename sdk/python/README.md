# Orrery Python SDK

同步客户端，手写于 `httpx` 之上，对应 `docs/spec/api.md` §9。只依赖 httpx，Python ≥ 3.10，模型是 `dataclasses`。

不发布到 PyPI，直接用 git 安装：

```bash
pip install "git+https://github.com/zibyn/orrery#subdirectory=sdk/python"
```

版本号跟仓库 tag 锁步。

## 用法

```python
from orrery import Orrery, OrreryError

with Orrery("http://127.0.0.1:8080", token) as client:
    sandbox = client.create_sandbox(cpu=0.5, memory=128 << 20, idle_stop=0)
    sandbox = client.wait_for_sandbox(sandbox.id, "ready")

    exec_ = client.create_exec(sandbox.id, ["sh", "-c", "printf hello; exit 7"])
    for frame in client.follow_output(exec_.id):
        print(frame.stream, frame.data_bytes)
    finished = client.wait_exec(exec_.id)
    print(finished.result.cause, finished.result.exit_code)  # exited 7

    client.write_file(sandbox.id, "src/hello.txt", b"hello workspace\n")
    data = client.read_file(sandbox.id, "src/hello.txt", offset=6, limit=9)  # workspa
    entries = client.list_files(sandbox.id, "src")
    client.move_file(sandbox.id, "src/hello.txt", "src/moved.txt")
    client.delete_file(sandbox.id, "src/moved.txt")

    archive = client.download_archive(sandbox.id, "src")   # tar bytes
    client.upload_archive(sandbox.id, "unpacked", archive)

    client.put_secret("API_TOKEN", "value")
    client.list_secrets()
    client.delete_secret("API_TOKEN")

    client.delete_sandbox(sandbox.id)
```

`OrreryError` 带 `status`、`code`（`api.md` §2 的封闭目录）和 `detail`：

```python
try:
    client.get_sandbox("missing")
except OrreryError as err:
    print(err.status, err.code, err.detail)  # 404 not_found ...
```

## 覆盖范围

- Sandbox 生命周期：create、list、get（含 `wait_for`）、wait_for、start、stop、delete
- Exec：create、get、list、wait、按 `after` 跟随输出、cancel
- 文件（§5 全部）：list、stat、读（Range）、写、mkdir、move、delete、tar 下载与上传
- Secret：put、list、delete

不覆盖 Resource 上传、Workspace publish、端口转发和 PTY。

## 开发

```bash
python -m unittest discover -s sdk/python/tests
```

`tests/parity.py` 对真实部署跑 M1 场景，由环境变量提供地址与 token：

```bash
ORRERY_SANDBOX_API_URL=http://127.0.0.1:18080 \
ORRERY_SANDBOX_TOKEN=<admin token> \
python sdk/python/tests/parity.py
```

GPU 主机上再加 `ORRERY_PARITY_GPU=1`（可用 `ORRERY_E2E_GPU_SNAPSHOT` 指定平台 Snapshot 名，默认 `cuda`）跑 `gpu-sandbox-ready`。
