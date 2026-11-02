# 验收用的 harness 改造件

这个目录里的东西**不是产品代码**，是让「现成的 Playwright 测试套件」可以直接驱动 Go 后端
的最小工程件。目标：**472 个 `tests/e2e_ui` 用例零改动**打在新后端上。

## 为什么需要它

`tests/e2e_ui/conftest.py` 自己 spawn server 子进程，命令行是：

```
<exe> -c "import omnigent.server.presence as _p; _p._LEAVE_GRACE_S = 1.0;
         from omnigent.cli import main; main()"
      server --host 127.0.0.1 --port <p>
             --database-uri sqlite:///<tmp>/test.db
             --artifact-location <tmp>/artifacts
             --agent <tmp>/hello_world.yaml
```

`tests/_helpers/compat.py` 提供了 `OMNIGENT_COMPAT_SERVER_PYTHON` / `OMNIGENT_COMPAT_RUNNER_PYTHON`
两个旋钮来换掉 `<exe>`，并且在 compat 模式下会**删掉 `PYTHONPATH`**、**切到中立 CWD**，
所以只能用「一个会转发 argv 的可执行文件」来接。

## `main.go`：server wrapper

- 丢掉 harness 前置的 `-c <patch>`（或 `-m <module>`）两段；
- 起 `GO_SERVER_BIN`，把余下 argv 原样传下去；
- 子进程退出码透传（harness 会检查 `poll()`）。

环境变量：

| 变量 | 含义 |
|---|---|
| `GO_SERVER_BIN` | 要运行的 server。Go 版直接指向编译出的二进制 |
| `GO_SERVER_MODULE_ENTRY=1` | 把目标当作解释器，用 `-m omnigent` 方式跑（**只在验证旋钮本身时用**，即用基线 Python 冒充） |

> 为什么要这个开关：`GO_SERVER_BIN=python.exe` 时，`python server --host ...` 会把 `server`
> 当成脚本文件名，报 `can't open file 'server'`。原型踩过。

## 用法

### 验证旋钮本身可用（基线冒充 Go）

```bash
cd designs/go-rewrite/harness && go build -o /tmp/server-wrapper .
cd ../../..
SERVER_WRAPPER_BIN=/tmp/server-wrapper \
  bash designs/go-rewrite/harness/verify-compat-knob.sh
# 实测：1 passed in 15.92s
```

### 真正打 Go 后端

```bash
cd designs/go-rewrite/harness && go build -o /tmp/server-wrapper .
cd ../../..

export OMNIGENT_COMPAT_SERVER_PYTHON=/tmp/server-wrapper.exe
export OMNIGENT_COMPAT_SERVER_VERSION=0.21.0   # 与 Go server 的 /api/version 对齐
export GO_SERVER_BIN=/path/to/go-omnigent-server
unset GO_SERVER_MODULE_ENTRY                    # 直接跑二进制

export PYTHONUTF8=1 NO_PROXY=127.0.0.1,localhost
unset HTTP_PROXY HTTPS_PROXY ALL_PROXY http_proxy https_proxy all_proxy

.venv/Scripts/python.exe -m pytest tests/e2e_ui \
  -q --ui-skip-build --browser-channel msedge -p no:randomly
```

### 连 Go runner 一起换

```bash
export OMNIGENT_COMPAT_RUNNER_PYTHON=/tmp/runner-wrapper.exe
export GO_RUNNER_BIN=/path/to/go-omnigent-runner
```

要另写一个只改 `GO_*_BIN` 默认值的 wrapper（或复用同一份 + 不同环境变量），并把
`surface` 记成 `go-server+go-runner`，与 `go-server+py-runner` 分开取证 —— 两者不可互相替代。

## 硬性约束（都是实测撞出来的）

1. **必须是原生 exe**。harness 用 `Popen([exe, "-c", ...])` 直接 CreateProcess；
   bash / `.cmd` shim 会 `WinError 2`。
2. **必须吃掉前两个 argv**。`-c` 后面那段 Python 源码在 Go 侧无意义，只做了
   `presence._LEAVE_GRACE_S = 1.0`；Go server 应把它做成自己的 flag（否则 presence 离开
   断言会等 15s）。
3. **退出码必须透传**。harness 用 `process.poll()` 判断「server 提前退出」，返回 0 会让它
   一直等健康检查直到 30s 超时。
4. **不要设 `PYTHONPATH`**。compat 模式会主动删掉它，想靠它注入会失效。

## 目录里还有什么

| 文件 | 用途 |
|---|---|
| `main.go` / `go.mod` | server wrapper 源码（~60 行） |
| `verify-compat-knob.sh` | 一键验证旋钮可用（基线冒充），是验收体系的「自检」 |
