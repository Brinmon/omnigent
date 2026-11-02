# Go 后端验收计划（Playwright 实战验收 + 逐条功能对标）

> **核心原则：后端可用不是验收标准。** 一个功能只有在「用 Playwright 驱动真实浏览器 → 打到 Go 后端 → 观察到与基线一致的用户可见行为」并留下可复核证据之后，才可以打勾。
>
> 本计划的每一条判据都来自**已在 Windows 上实测跑通**的原系统（见 §2 实证记录），不是设计推演。

---

## 0. 三条铁律

1. **一条功能 = 一个验收行 = 一次打勾**，行内必须带：功能 ID、入口点、Playwright 用例路径、证据产物路径、Go commit。缺一项不得打勾。
2. **基线先行**：任何功能在重构前必须先在原系统跑出绿证据。基线不绿的功能不允许进入 Go 实现（否则分不清是「Go 写错」还是「本来就不工作」）。
3. **同一条用例、同一套浏览器、同一份 mock 内容，分别打两个后端**；只有两侧都绿且 DOM 断言一致才算通过。禁止为 Go 后端放宽断言。

---

## 1. 验收基础设施（已实测验证）

### 1.1 浏览器：用系统 Chrome / Edge，不下载 Chromium

实测结果（本机 Chrome 156 / Edge 156）：

```
OK   chrome             version=156.0.8078.79 text=hello-pw
OK   msedge             version=156.0.4314.58 text=hello-pw
FAIL bundled-chromium   Executable doesn't exist: chromium_headless_shell-1223
```

- `ms-playwright` 缓存里是 revision **1234**，当前 `playwright` 包要 **1223**，所以默认 chromium **不可用**。
- 但 `channel="chrome"` / `"msedge"` 走系统浏览器，**完全可用**，且 `pytest-playwright` 原生支持：`--browser-channel msedge`。
- **结论**：验收命令统一带 `--browser-channel msedge`（或 `chrome`）。不装 182MB Chromium。

### 1.2 Python 环境（uv，已建好）

```bash
uv sync --frozen --extra all --group test      # 已执行完成，.venv 就绪
```

> `uv.toml` 要求 `uv >= 0.13.4`；本机 anaconda 自带的 0.11.11 会被拒。已升级到 0.13.6。

### 1.3 数据库：只用本地 SQLite（与 Python 默认一致）

实测确认：

| 项 | 值 |
|---|---|
| 默认 DB URI | `sqlite:///C:\Users\28591\.omnigent\chat.db` |
| 引擎 | SQLAlchemy → `sqlite` dialect |
| journal_mode | `wal` |
| 表数 | 20 |

- `omnigent server` 默认 `--database-uri` 就是 `_default_db_uri()` → `~/.omnigent/chat.db`。
- Playwright 套件把 DB 指到 pytest tmp：`--database-uri sqlite:///<tmp>/test.db`，**不碰用户库**。
- **Go 版验收只支持 SQLite**，验收脚本一律显式传 `--database-uri sqlite:///<隔离目录>/test.db`；不引入 MySQL/Postgres。

### 1.4 Mock LLM（确定性，零外部凭据）

- `tests/server/integration/mock_llm_server.py`：OpenAI 兼容 `/v1/`，加 `/stats`、`/mock/configure`、`/mock/reset`、`/mock/requests`。
- 响应构造器：`sse_text_response` / `sse_tool_call_response` / `anthropic_sse_text_response` / `anthropic_sse_tool_call_response` / `sse_text_with_native_items`。
- Playwright 侧用 `configure_mock_llm()` / `set_fallback_mock_llm()` / `reset_mock_llm()` 排响应队列。
- **必须继续用同一套 mock**：Go 后端验收若换 mock，就失去了与基线的可比性。

### 1.5 基线启动命令（本机已验证绿）

```bash
# Windows / Git Bash
export PYTHONUTF8=1
unset HTTP_PROXY HTTPS_PROXY ALL_PROXY http_proxy https_proxy all_proxy
export NO_PROXY=127.0.0.1,localhost

# 1) 冒烟：SPA → server → runner → SSE → DOM
.venv/Scripts/python.exe -m pytest tests/e2e_ui/chat/test_smoke.py \
  -x -q --ui-skip-build --browser-channel msedge
# → 1 passed in 22.51s

# 2) 真 Pi + mock LLM 端到端
.venv/Scripts/python.exe -m pytest tests/e2e/omnigent/test_per_harness_pi.py \
  -x -q -p no:randomly
# → 1 passed in 49.77s
```

两条都实测通过，是整个验收体系的锚点。

---

## 2. 实证记录：跑通原系统时遇到的 4 个真实缺陷（已修，必须进基线）

这些是**原系统在 Windows 上的真实可移植性缺陷**，不是 Go 重构引入的。已修好并验证；Go 版验收脚本不得重复踩。

| # | 位置 | 症状 | 根因 | 修复 |
|---|---|---|---|---|
| 1 | `omnigent/inner/loader.py:108` | 上传 agent bundle → `400 invalid agent bundle: 'gbk' codec can't decode byte 0x94` | `open(path)` 未指定 encoding，zh-CN Windows 默认 GBK | `open(path, encoding="utf-8")` |
| 2 | `omnigent/spec/_omnigent_compat.py:256,296,404` | 同上，spec 探测阶段 | `path.read_text()` 同样无 encoding | `read_text(encoding="utf-8")` |
| 3 | `tests/e2e/_harness_probes.py:164` | 真 Pi 的 e2e 被静默 **skip**，误以为「Pi 未安装」 | Windows 上 `pi` 解析为 npm `.cmd` shim，非 PE 镜像，`CreateProcess` 直接 exec 失败 | `shell=IS_WINDOWS` + 用 `shutil.which` 的绝对路径 + `encoding="utf-8"` |
| 4 | `tests/e2e_ui/conftest.py:605`、`tests/e2e/omnigent/conftest.py:206` | mock LLM fixture 报 `httpx.ConnectTimeout` 而挂掉 | Windows 对刚绑定的 loopback 端口首次连接报**超时**而非 refused，等待循环只捕获 `ConnectError` | 同时捕获 `(ConnectError, ConnectTimeout)` |

另有一个环境级要求：**`PYTHONUTF8=1`**。因为写侧（`write_text` 等）也依赖平台默认编码，只修读侧会导致 UTF-8/GBK 不匹配（实测撞到过 `'utf-8' codec can't decode byte 0xa1`）。验收脚本统一 export。

**回归确认**：`tests/spec` 在改动前后均为 **79 failed / 815 passed**（同一批失败，源于这些测试未隔离 `HOME`，把真实的 `~/.agents/skills` 读了进来 —— 环境问题，与本次改动无关）。改动后整体 815 → 894 passed，无新增失败。

---

## 3. 验收判据：复用仓库自带的官方矩阵（不另立门户）

`feature-map/harness-acceptance.md` 已经给出 **P0/P1 逐条可观察判据**，且明确「Pi 与 Claude、Codex 适用同一套标准」。直接采信它作为验收口径，避免自定义一套更宽松的标准。

### 3.1 P0（Go V1 必须全绿，否则 V1 不算完成）

| ID | 可观察验收（摘自官方矩阵） |
|---|---|
| `launch-reply` | 从受支持入口启动；流式输出与最终答案各出现一次；usage 有上报 |
| `working-status` | 真实进行中的轮次在静默模型/工具期间保持 working；完成/中断/进程退出会收敛指示器；下一轮正常 |
| `tool-history` | 真实工具返回一个值；后续轮次使用该值而不重复调用；工具事件与结果持续可见 |
| `model-effort` | 启动前与会话内的选择到达目标模型/请求；effort 跨工具往返存活；被拒绝的变更不得显示为已生效 |
| `interrupt-continue` | 中断一个进行中的轮次，然后在同一会话完成另一轮，无残留工作、无卡住的 running |

### 3.2 P1（Go V1 达标必须绿；若某项 V1 明确不做，须写「为什么」并单独记录，不得静默当通过）

`reload-resume`、`auth-recovery`、`approvals`、`steering-queue`、`compaction`、`images`、`fork`、`mcp`、`user-resources`、`interactive-input`

### 3.3 每个判据必须在 4 个 surface 上分别取证（官方要求「不可互换」）

| Surface | 入口 | 归属 recipe |
|---|---|---|
| Web 新会话 | 选 harness/workspace/model/effort → 发送 | `feature-map/composer.md`、`custom-agents.md` |
| Web 已有会话 | 追问、改配置、中断、批准、刷新、fork | `feature-map/sessions.md`、`native-harnesses.md` |
| Native 终端 / CLI | `omnigent` CLI 入口、原生控件、Terminal ↔ Chat 切换 | `feature-map/native-harnesses.md`、`terminals.md` |
| SDK CLI / API | `omnigent run` 或 session API，验 server/runner 契约 | `tests/harness_bench/` |

> **V1 范围**：Go 只做 `pi`（SDK/RPC 型），不做 `pi-native`（TUI 包装）。因此 surface 2/3 中「native terminal」相关的行，V1 标记为 **Out of scope (V1)** 并写明理由；surface 1/2 的 Chat 路径 + surface 4 是 V1 必须绿的。

---

## 4. 验收矩阵（逐条打勾表）

复制到 `designs/go-rewrite/ACCEPTANCE-TRACKER.md`，每完成一条填一行。**空着的格子 = 没验收**。

### 阶段 A：契约层（不需要浏览器，但必须全绿才能开工）

| # | 判据 | 命令 | Go | 基线 | 证据 | 勾 |
|---|---|---|---|---|---|---|
| A1 | OpenAPI 结构等价 | `go run ./cmd/openapi-diff --old openapi.json --new <(curl :8080/openapi.json)` | 0 diff | — | `evidence/A1/` | ☐ |
| A2 | 117 path / 154 operation 全部实现 | 同上（缺一个即失败） | — | — | `evidence/A2/` | ☐ |
| A3 | 194 schema 字段/必填/枚举一致 | 同上 | — | — | `evidence/A3/` | ☐ |
| A4 | `ServerStreamEvent` 57 变体 `type` 字面量集合一致 | golden JSON 快照 | — | — | `evidence/A4/` | ☐ |
| A5 | 旧库（Python 写入的 SQLite）Go 可直读 | `go test ./internal/db/... -run Compat` | pass | pass | `evidence/A5/` | ☐ |
| A6 | `Uuid16` / 枚举 SMALLINT / CompressedText 逐字节兼容 | 同上（含压缩帧与旧明文两种行） | — | — | `evidence/A6/` | ☐ |

### 阶段 B：启动链路（第一条必须有真浏览器）

| # | 判据 | 基线用例 | 勾 |
|---|---|---|---|
| B1 | SPA 启动 → `/v1/info` + `/v1/me` + `/v1/harnesses` 成功，页面不降级 | `tests/e2e_ui/chat/test_smoke.py::test_send_message_renders_assistant_response` | ☐ |
| B2 | 会话列表加载（sidebar 渲染出历史会话） | `tests/e2e_ui/sessions/`（见 §5 清单） | ☐ |
| B3 | 历史消息 hydration（刷新后条目位置/内容不漂移） | `tests/e2e_ui/chat/test_*history*` | ☐ |
| B4 | 发消息 → SSE 流式 → assistant 气泡出现且文本非空 | `test_smoke.py`（同上，已实测绿） | ☐ |
| B5 | `WS /v1/sessions/updates` watch-set 增量（新建会话在侧栏即时出现） | `tests/e2e_ui/sessions/` | ☐ |

### 阶段 C：Pi agent（V1 的实质内容）

| # | 判据 | 基线用例 | 勾 |
|---|---|---|---|
| C1 | 真 `pi --mode rpc` 一轮问答（含 `agent_settled` 边界） | `tests/e2e/omnigent/test_per_harness_pi.py`（实测绿） | ☐ |
| C2 | 工具调用往返（loopback TCP 工具服务器 + JS extension） | `tests/e2e/omnigent/test_pi_long_tool_turn.py` | ☐ |
| C3 | 工具历史复用（第二轮回用首轮结果，不重复调用） | 官方 `tool-history` | ☐ |
| C4 | 中断（`abort`）后同会话可继续 | 官方 `interrupt-continue` | ☐ |
| C5 | 流式 thinking / reasoning 渲染 | `tests/e2e/omnigent/test_pi_thinking_compatibility_e2e.py` | ☐ |
| C6 | 重试恢复（provider retry 不截断轮次） | `tests/e2e/omnigent/test_pi_retry_recovery_e2e.py` | ☐ |
| C7 | 压缩（`compaction_end` + `get_messages` 回读） | `tests/e2e/test_pi_compact_restart_resume.py` | ☐ |
| C8 | 策略 ASK 卡片往返（extension_ui_request → Web elicit → 继续） | `tests/e2e/test_pi_extension_ui_round_trip.py` | ☐ |
| C9 | 策略 DENY 不产生副作用且会话可继续 | 官方 `approvals` | ☐ |
| C10 | 模型 / effort 切换生效 | `tests/e2e/test_pi_native_model_scope_e2e.py`（取 Chat 部分） | ☐ |

### 阶段 D：策略与资源

| # | 判据 | 基线用例 | 勾 |
|---|---|---|---|
| D1 | `PHASE_REQUEST` / `PHASE_TOOL_CALL` 拦截生效 | `tests/e2e_ui/approvals/` | ☐ |
| D2 | CEL 表达式策略与 Python 结果一致 | 对拍：同表达式同事件 | ☐ |
| D3 | 终端（PTY）创建 / 读 / 写 / 关闭 | `tests/e2e_ui/shells/` | ☐ |
| D4 | 文件上载 / 下载 / 列表 | `tests/e2e_ui/files/` | ☐ |
| D5 | 工作区文件树与内容搜索 | `tests/e2e_ui/files/` | ☐ |
| D6 | Fork 会话（历史带过去且与父隔离） | `tests/e2e_ui/fork_session/` | ☐ |

### 阶段 E：会话生命周期

| # | 判据 | 基线用例 | 勾 |
|---|---|---|---|
| E1 | 重命名 / 置顶 / 归档 / 删除 | `tests/e2e_ui/sessions/` | ☐ |
| E2 | 搜索（标题 + 消息内容，archived 标记） | `tests/e2e_ui/sessions/` | ☐ |
| E3 | 刷新 / 断线重连（snapshot + live tail 去重） | `tests/e2e_ui/sessions/` | ☐ |
| E4 | presence（共同浏览者头像） | `tests/e2e_ui/collaboration/` | ☐ |
| E5 | 未读标记跨刷新存活 | `tests/e2e_ui/sessions/` | ☐ |

---

## 5. 怎么跑：三个验收档位

### 档位 1 — 快速回归（每次 Go commit）

```bash
export PYTHONUTF8=1 NO_PROXY=127.0.0.1,localhost
unset HTTP_PROXY HTTPS_PROXY ALL_PROXY http_proxy https_proxy all_proxy

# 1) 契约层，秒级
go run ./cmd/openapi-diff --old openapi.json --new http://127.0.0.1:8080/openapi.json

# 2) 冒烟：由 conftest 自己起 server+runner+mock（全部走 mock LLM）
.venv/Scripts/python.exe -m pytest tests/e2e_ui/chat/test_smoke.py   -x -q --ui-skip-build --browser-channel msedge
```

**⚠️ `--ui-base-url` 模式的实测限制（务必先读）**

本机实测：`--ui-base-url http://127.0.0.1:8099` 打一个**手工启动**的 server 时，`test_smoke.py`
会 **失败**（等了 60s 不见 assistant 气泡），原因不是 UI 坏了，而是：

- `mock_llm_server_url` fixture 会**另起一个新 mock**；
- 而 `_spawn_runner_against_external_server()`（conftest.py:895）**不会**给外部 server
  注入 `OPENAI_BASE_URL`（只有它自己 spawn server 的分支才在 :1094 设置）；
- 于是外部 server 的 LLM 调用打不到这个新 mock，回合永远不回来。

所以 `--ui-base-url` **不是**「随便指一个 server 就能验」。要用它，你必须：

1. 让被指向的 server 自己配好 mock，且**与 fixture 新起的那个 mock 是同一个**；
2. 实际操作上唯一干净的做法是走 §档位 3 的 `OMNIGENT_COMPAT_SERVER_PYTHON` 旋钮
   —— 让 conftest 照常自己 spawn/编排 server 与 mock，只把「server 二进制」换成 Go。
   这样 mock 注入、runner 注入、DB 隔离全部保留原逻辑。

**因此：`--ui-base-url` 只用于「手工复现/探索」，不作为验收档位。验收一律走档位 3 的 redirect 旋钮。**

### 档位 2 — 功能对标（每完成一个功能组）

```bash
PYTHONUTF8=1 .venv/Scripts/python.exe -m pytest \
  tests/e2e_ui/sessions tests/e2e_ui/approvals tests/e2e_ui/files \
  --ui-skip-build --browser-channel msedge \
  --ui-base-url http://127.0.0.1:8080 \
  --splits 1 --group 1 -p no:randomly
```

### 档位 3 — 全量（里程碑 / 阶段门）

```bash
# 直接让 conftest 起 Python server 作为「基线」
PYTHONUTF8=1 .venv/Scripts/python.exe -m pytest tests/e2e_ui \
  --ui-skip-build --browser-channel msedge -q

# Go 后端：靠 SERVER redirect 旋钮替换 server 子进程
# 值是「原生可执行文件」路径，不是 venv python（见下方实测约束）
OMNIGENT_COMPAT_SERVER_PYTHON='E:/path/to/go-server-wrapper.exe' \
OMNIGENT_COMPAT_SERVER_VERSION=0.21.0 \
PYTHONUTF8=1 .venv/Scripts/python.exe -m pytest tests/e2e_ui \
  --ui-skip-build --browser-channel msedge -q
```

> **关键杠杆（已验证成立）**：`tests/_helpers/compat.py` 提供
> `OMNIGENT_COMPAT_SERVER_PYTHON` / `OMNIGENT_COMPAT_RUNNER_PYTHON` 两个 redirect 旋钮。
> 本机实测：写一个 Go 二进制充当「server wrapper」，把它设为
> `OMNIGENT_COMPAT_SERVER_PYTHON` 后，`test_smoke.py` **1 passed in 16.36s**，
> wrapper 捕获到的 argv 是：
>
> ```
> raw       = ["-c", "<presence patch 代码>", "server", "--host", ..., "--database-uri", "sqlite:///...",
>              "--artifact-location", ..., "--agent", ...]
> forwarded = ["server", "--host", ..., "--database-uri", "sqlite:///...", "--artifact-location", ..., "--agent", ...]
> ```
>
> 注意两个实测约束（踩过）：
>
> 1. **wrapper 必须是原生可执行文件**（Go 编译的 `.exe`），不能是 bash/`.cmd`。harness 用
>    `subprocess.Popen([exe, "-c", ...])` 直接 CreateProcess，脚本 shim 会 `WinError 2`。
> 2. wrapper 必须**去掉前两个 argv**（`-c` + 那段 patch 的 Python 源码），把余下参数交给真正的
>    Go server。`-m <module>` 形式同样处理。那段 `-c` 代码只是把
>    `presence._LEAVE_GRACE_S` 调成 1s，Go 侧应在自己的配置里提供等价开关（或直接忽略）。
>
> 这样一来 **472 个 Playwright 用例零改动**就能打在 Go server 上，mock 注入、
> runner 注入、SQLite 隔离等编排逻辑全部保留。这是整个验收体系最重要的一条工程杠杆。
>
> 该 wrapper 已固化在 **[`harness/`](./harness/README.md)**（含源码、自检脚本与踩坑清单）：
>
> ```bash
> SERVER_WRAPPER_BIN=/tmp/server-wrapper \
>   bash designs/go-rewrite/harness/verify-compat-knob.sh
> # 实测：1 passed in 15.92s
> ```

---

## 6. 证据规范（缺证据 = 没验收）

每条打勾必须产出：

```
evidence/<ID>/
  manifest.json      # {id, go_commit, python_commit, browser, channel_version,
                     #  command, exit_code, started_at, duration_s, surface}
  test.log           # pytest 输出
  report.html        # --output 生成的 Playwright 报告
  video/*.webm       # 交互类必须有（--video=on）
  screenshots/*.png  # 关键状态
  dom-assert.json    # 关键断言的实际文本/DOM 快照
```

判定规则：

- **基线侧与 Go 侧各一份**，`dom-assert.json` 必须逐键相等（时间戳/随机 id 除外，白名单显式列出）。
- 视频里必须能看到「用户动作 → 结果状态」，不能只有终态截图。
- 若该 ID 的某个 surface 标为 Out of scope，必须在 `manifest.json` 写 `"skipped_reason"`，不得直接不写。

---

## 7. 打勾规则（硬约束）

1. **一个 ID 一行，不合并**。`C2 工具调用往返` 与 `C3 工具历史复用` 是两条，不能因为「都跑通了」合并成一个勾。
2. **只有 Playwright 绿不算完**：交互类必须有视频；纯后端类（A1–A6）必须有 diff 输出。
3. **Go 侧绿、基线侧未跑 = 不通过**。两边都要有证据。
4. **禁用「容忍列表」**：如果一条用例 Go 侧红但「看起来不影响用户」，只能标 `blocked` + 原因，不能标 done。
5. **禁止改断言让 Go 过**。若认为断言本身对 Go 不成立，先在基线侧证明该断言是历史包袱（提 issue），再单独走例外流程。
6. 每阶段结束时把整表导出成 `ACCEPTANCE-REPORT-<phase>.md`，附 commit 与耗时。

---

## 8. 与开发计划的衔接

对应 `02-go-refactor-plan.md` 的阶段，验收门如下：

| 开发阶段 | 必须绿掉的验收行 | 门禁 |
|---|---|---|
| P0 裁判 | A1–A6 | 无 A 全绿不得进入 P1 |
| P1 骨架 | B1–B3, E1–E2 | 前端能只读浏览会话 |
| P2 实时 | B4–B5, E3 | 前端能发消息看到流式 |
| P3 runner + Pi | C1–C4, C6 | **V1 的关键门**：真 Pi 跑通一轮+工具+中断 |
| P4 策略与资源 | C8–C10, D1–D5 | 策略拦截可见、终端可用 |
| P5 硬化 | C5, C7, D6, E4–E5 | 全 P0 全绿 |
| P6 切换 | 全表 + 一次档位 3 全量双跑 | 默认走 Go |

---

## 9. 已知风险（验收层面）

| 风险 | 影响 | 对策 |
|---|---|---|
| `--ui-base-url` 模式下 conftest 仍会 spawn **Python runner** | 若 Go 只换了 server，runner 仍是 Python → 只能验「server 契约」，验不到 Go runner | 阶段 B/C 用 `OMNIGENT_COMPAT_RUNNER_PYTHON` 换成 Go runner wrapper；两级验收分开记录 |
| Windows 首次连接 `ConnectTimeout` | fixture 挂掉，误判为功能失败 | 已在 §2 #4 修复；Go 侧 ready 探针同样必须捕获超时 |
| `PYTHONUTF8` 未设 | UTF-8/GBK 混用导致 bundle 400 | 验收脚本统一 export，并写进 `just` 目标 |
| 79 个既有 `tests/spec` 失败 | 噪音，掩盖真回归 | 验收基线固定为「79 failed / 815 passed」，每次对比差值 |
| mock LLM 与真 Pi 的耦合 | `test_per_harness_pi.py` 文档明确要求串行、且 pi 必须 honors `OPENAI_BASE_URL` | 一律 `-p no:randomly`，不并行；Go 侧同样用这份 mock |
| 视频/截图体积 | 仓库污染 | 证据全部落 `evidence/`（gitignore），PR 用 GitHub attachment |

---

## 10. 立即可执行的三步

```bash
# 1) 固化基线（约 25 分钟，跑通即建立「裁判」）
export PYTHONUTF8=1 NO_PROXY=127.0.0.1,localhost
unset HTTP_PROXY HTTPS_PROXY ALL_PROXY http_proxy https_proxy all_proxy
.venv/Scripts/python.exe -m pytest tests/e2e_ui/chat/test_smoke.py \
  tests/e2e/omnigent/test_per_harness_pi.py \
  -q --ui-skip-build --browser-channel msedge -p no:randomly \
  --video=on --screenshot=on --output=evidence/BASELINE

# 2) 生成空表并开始填
cp designs/go-rewrite/ACCEPTANCE-TRACKER.md evidence/ 2>/dev/null || true

# 3) 写 Go server wrapper（解锁档位 3 全量复用）
#    一个 ~80 行的 python 脚本：把 (server --host --port --database-uri
#    --artifact-location) 转成 exec go-omnigent-server，供
#    OMNIGENT_COMPAT_SERVER_PYTHON 指向
```

---

## 11. 与 `--ui-base-url` 模式的配合（重要实测细节）

`tests/e2e_ui/conftest.py` 的 `_spawn_runner_against_external_server()` 在
`--ui-base-url` 模式下**仍会自己 spawn 一个 Python runner**，通过 WS 隧道连到你指定的
server。含义：

- 只有当 Go server 严格实现 runner 隧道 + 会话契约时，这个 runner 才能连上。
- 因此**阶段 B 验收的是「Go server + Python runner」**，能证明 server 侧契约正确。
- **验不到 Go runner**。要验 Go runner，必须同时用
  `OMNIGENT_COMPAT_RUNNER_PYTHON`（见 `tests/_helpers/compat.py`）把 runner 子进程也换成
  Go wrapper，并在 `manifest.json` 的 `surface` 字段里区分
  `go-server+py-runner` 与 `go-server+go-runner` 两种组合，分开记证。

这两种组合不能互相替代，也不该合并成一行勾。

## 12. 环境变量收口（写进验收脚本头部）

```bash
export PYTHONUTF8=1                              # 必须：否则 UTF-8/GBK 混用导致 bundle 400
export NO_PROXY=127.0.0.1,localhost
unset HTTP_PROXY HTTPS_PROXY ALL_PROXY http_proxy https_proxy all_proxy
# 保持 mock：不要让任何真模型凭据泄漏进测试进程
unset ANTHROPIC_API_KEY OPENAI_API_KEY DATABRICKS_TOKEN LLM_API_KEY
```

`PYTHONUTF8=1` 是硬要求，不是可选优化：本机实测中，只修读侧 encoding 而写侧仍走平台默认，
会导致 `'utf-8' codec can't decode byte 0xa1`。
