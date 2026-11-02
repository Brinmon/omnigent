# 验收基线报告（Windows / 系统 Edge / SQLite / mock LLM）

**基线 commit**：`6d209c0` + 本报告 §3 的 7 处可移植性修复
**执行脚本**：`designs/go-rewrite/run-baseline.sh`（可重复、分批、超时隔离、证据落盘）
**环境**：Windows 11 / Python 3.12.13 (.venv) / `uv 0.13.6` / 系统 Edge 156（`--browser-channel msedge`，未下载 Chromium）/ SQLite WAL / mock LLM
**执行时间**：约 25 分钟（含 5 次失败重试与 7 处缺陷修复）

---

## 1. 总表

| ID | 判据 | 结果 | 耗时 | 证据 |
|---|---|---|---|---|
| B1 | SPA 启动 + `/v1/info` `/v1/me` `/v1/harnesses` | ✅ PASS | 43s | `evidence/baseline/B1/` |
| B2 | 会话列表渲染（sidebar） | ✅ PASS | 47s | `evidence/baseline/B2/` |
| B3 | 历史 hydration（刷新不漂移） | ✅ PASS | 50s | `evidence/baseline/B3/` |
| B4 | 发消息 → SSE → assistant 气泡 | ✅ PASS | 30s | `evidence/baseline/B4/` |
| B5 | `WS /v1/sessions/updates` 增量 | ✅ PASS | 58s | `evidence/baseline/B5/` |
| E1 | 重命名 / 置顶 / 归档 / 删除 | ✅ PASS | 78s | `evidence/baseline/E1/` |
| E2 | 搜索（标题 + 内容 + archived） | ✅ PASS | 51s | `evidence/baseline/E2/` |
| E3 | 刷新 / 断线重连去重 | ✅ PASS | 26s | `evidence/baseline/E3/` |
| E4 | presence 共同浏览者 | ✅ PASS | 71s | `evidence/baseline/E4/` |
| E5 | 未读标记跨刷新 | ✅ PASS | 43s | `evidence/baseline/E5/` |
| C1 | 真 `pi --mode rpc` 一轮问答 | ✅ PASS | 36s | `evidence/baseline/C1/` |
| **C2** | **工具调用往返** | ❌ **FAIL** | 59s | `evidence/baseline/C2/` |
| C4 | 中断后继续 | ✅ PASS | 8s | `evidence/baseline/C4/` |
| C5 | thinking / reasoning 流式 | ✅ PASS | 186s | `evidence/baseline/C5/` |
| C6 | provider 重试恢复 | ✅ PASS | 34s | `evidence/baseline/C6/` |
| **C7** | **压缩 + get_messages** | ❌ **FAIL** | 54s | `evidence/baseline/C7/` |
| C8 | 策略 ASK 往返 | ⚠️ SKIP | 6s | `evidence/baseline/C8/` |
| D1 | `PHASE_REQUEST`/`PHASE_TOOL_CALL` 拦截 | ✅ PASS | 36s | `evidence/baseline/D1/` |
| **D3** | **终端创建/读/写/关闭** | ❌ **FAIL** | 414s | `evidence/baseline/D3/` |
| **D5** | **工作区文件搜索** | ❌ **FAIL** | 89s | `evidence/baseline/D5/` |
| D6 | Fork（带历史、与父隔离） | ✅ PASS | 37s | `evidence/baseline/D6/` |

**统计**：16 PASS · 4 FAIL · 1 SKIP

---

## 2. 每个失败的定性（这是 Go 重构的关键输入）

### C2 工具调用往返 — ❌ 测试脚手架缺陷，**不是产品缺陷**

**决定性证据**（已实测抓取）：

```
[DIAG] mock requests: 2
[DIAG] stdout: 'Long task finished. LONG_TOOL_TURN_COMPLETED\n'
[DIAG] stderr: ''
[DIAG] files: ['__pycache__', 'long_maintenance_tool.py', 'long_tool_agent.yaml', 'sitecustomize.py']
```

- mock LLM **被调用了 2 次**，说明 `prompt → 模型 → 第二轮` 整条链路是通的。
- 但 `long_task_started` 标记文件**从未生成** —— mock 返回的 `tool_calls` 没有被 Pi 执行。
- 原因：`test_pi_long_tool_turn.py` 的 mock 用 Responses API 的 `function_call` 形状（`sse_tool_call_response`），
  而 Pi 走 `OPENAI_BASE_URL` 的 chat/completions 路径，对注入的 tool_call 不产生执行。
- **已在干净 worktree（`git worktree add` 到 HEAD）复现同样失败**，与本轮任何改动无关。

**对 Go 的影响**：Go 侧**不需要**为这条写特殊逻辑；但「工具调用往返」这个 P0 判据必须有可信的验证手段，
所以需要在 Go 阶段自建一个真工具调用场景（不经 mock 的 tool_call 注入），见 §4 待补项。

### C7 压缩 — ❌ 前置条件不满足

`assert 40 >= (30 + 15)`：跑满 40 轮仍未触发 Pi 自动压缩。测试依赖 scripted 32k 上下文窗口让 Pi 溢出，
但 mock 下的 token 计数与 Pi 内部判定不一致，40 轮 × ~4KB 未越过阈值。

**定性**：不是代码缺陷，是「压力注入量」与「Pi 压缩阈值」在 mock 环境下对不上。
**对 Go 的影响**：压缩逻辑要复刻，但**验收方式需要改**——Go 侧应该用 `--context-window` 之类显式参数把窗口调小，而不是靠堆轮次。

### D3 终端 — ❌ Windows 环境限制

界面上直接显示 **`Failed to load: 502 Bad Gateway`**，等待 `terminal-view` 超时。
runner 侧终端接口不可达。本机 `tmux` 是 **psmux 3.3.8**（Winget 包的 `marlocarlo.psmux`），
并非 Omnigent 期望的标准 tmux；`omnigent/inner/terminal.py` 有 4 处 `IS_WINDOWS` 分支。

**定性**：Windows 上终端栈不完整。
**对 Go 的影响**：V1 若只做 Linux/macOS 则不受影响；若要做 Windows，终端层需要走 ConPTY 而非 tmux，
这是**新增工作量**，不是复刻。验收上，D3 在 Windows 上应标 `blocked(env)`，改到 Linux 环境补证。

### D5 文件搜索 — ❌ runner 资源接口在 Windows 上返回非法响应

**决定性证据**（server 侧抓到的 runner 真实回复）：

```
method=PUT status=500 kind=invalid_json ctype='text/plain' bytes=21 body=b'Internal Server Error'
```

- 这是 **runner 的 uvicorn 未捕获异常**（`text/plain`，非 JSON）。
- 而 runner 在自己的 diag 里**完全没有记录** —— 说明异常发生在路由 handler 之前（中间件/依赖解析层）。
- `CallerProcessFilesystem` 的构造依赖 `reachable_roots(cwd, policy)`，需要一个已解析的 `SandboxPolicy`；
  在 Windows 下这条解析路径有问题。

**定性**：runner 的 filesystem 依赖链在 Windows 上有未处理异常，属**真实缺陷**（非我引入）。
**对 Go 的影响**：Go 侧必须把「fs 依赖解析失败」映射成带 detail 的 4xx/5xx JSON，
**绝不能**返回裸 `text/plain: Internal Server Error`（当前 server 侧就是因为解析不了才报
"runner resource endpoint returned an invalid response"）。这是一个明确的**可观测性改进点**。

### C8 策略 ASK 往返 — ⚠️ SKIP（2 skipped）

用例自身条件不满足而 skip，不是失败。V1 需要另找证据（见 §4）。

---

## 3. 本轮修复的 7 处可移植性缺陷（全部已在基线上验证）

这些是**原系统在 Windows 上的真实缺陷**，修完基线才跑得动。Go 版验收脚本不得重复踩。

| # | 位置 | 症状 | 修复 |
|---|---|---|---|
| 1 | `omnigent/inner/loader.py:108` | 上传 bundle → `400 'gbk' codec can't decode 0x94` | `open(path, encoding="utf-8")` |
| 2 | `omnigent/spec/_omnigent_compat.py:256,296,404` | spec 探测阶段同样 GBK 崩 | `read_text(encoding="utf-8")` |
| 3 | `tests/e2e/_harness_probes.py:164` | **真 Pi 被静默 skip**，误判「Pi 未安装」 | `shell=IS_WINDOWS` + 绝对路径 + utf-8 |
| 4 | `tests/e2e_ui/conftest.py:605`、`tests/e2e/omnigent/conftest.py:206`、`tests/e2e/conftest.py:216,315` | mock/health 就绪循环挂 `ConnectTimeout` | 同时捕获 `(ConnectError, ConnectTimeout)` |
| 5 | `tests/e2e/omnigent/conftest.py:40` | **干净 worktree 上所有 `tests/e2e` 无法启动** | 同时找 `.venv/bin/python` 与 `.venv/Scripts/python.exe` |
| 6 | `tests/_helpers/live_server.py:146` | `Path.home()` → `Could not determine home directory` | Windows 下设 `USERPROFILE` |
| 7 | `tests/e2e/omnigent/test_pi_thinking_compatibility_e2e.py:70,115` | `capture_output+text=True` 返回 None；env 过窄触发 `WinError 10106` | 加 `encoding="utf-8"`；补 Windows 必需环境变量 |

**外加一条运行期必需的隔离**（写进 `run-baseline.sh`）：

```bash
export PI_CODING_AGENT_DIR=$EVIDENCE/pi-agent-dir
```

原因：开发者全局 Pi 装了 `pi-agent-goal` / `pi-intercom` / `pi-subagents` / `pi-cworker` 等包，
其中 `E:\GitCode\pi-cworker\extensions\index.ts` 当前是坏的（缺 `./stage-cursor.ts`），
Pi 启动即 `Failed to load extension` 退出。**这就是 C1/C5 时绿时红的真凶**。
隔离后 C1 从 55s/失败 变为稳定的 36s/通过。

---

## 4. 基线暴露的 3 个「验收方法本身要改」的地方（Go 阶段的直接输入）

1. **C2（工具往返）不能靠 mock 注入 tool_call。**
   Go 验收要自建场景：注册一个真 Python/Go 工具 → 让 mock 返回合法 chat/completions tool_call →
   断言工具副作用文件出现。或者直接用真实小模型跑一次。

2. **C7（压缩）不能靠堆 40 轮。**
   Go 侧应提供显式上下文窗口参数（如 `--context-window 2000`），用少量轮次稳定触发；
   否则每次验收要跑 40 轮，不稳定且慢（当前 54s 才失败）。

3. **D3（终端）/ D5（文件）在 Windows 上是环境阻塞，不是功能缺失。**
   建议：D3 标 `blocked(env)`，在 Linux 容器里补证；
   D5 的 runner 500 是**真缺陷**，Go 侧要保证返回结构化错误，并把这个案例写进契约测试。

---

## 5. 对 Go 重构范围的修正

基线的实际结果证明 V1 切片基本正确，但有两点要调整：

| 原计划 | 基线证据 | 调整 |
|---|---|---|
| V1 必须绿 P0 五项 | `launch-reply`(C1✅) `working-status`(隐含✅) `tool-history`(C2❌脚手架) `model-effort`(C5✅) `interrupt-continue`(C4✅) | **P0 已 4/5 可验**；`tool-history` 需自建场景后才算可验 |
| D3/D5 作为 V1 必做 | Windows 上 D3 阻塞、D5 暴露真缺陷 | D3 改 Linux 补证；D5 的「结构化错误」纳入 M1 契约 |
| 只支持 SQLite | 基线全程 SQLite WAL，20 表，无问题 | 维持原判，无需改 |

---

## 6. 复现命令

```bash
cd /e/GitCode/omnigent

# 全部（约 25 分钟，分批执行，每批独立超时）
bash designs/go-rewrite/run-baseline.sh

# 只跑某几组
bash designs/go-rewrite/run-baseline.sh B1 C1 D1

# 单条失败项复现（绕过脚本，看完整日志）
export PYTHONUTF8=1 PI_CODING_AGENT_DIR=/tmp/pi-clean
export NO_PROXY=127.0.0.1,localhost
unset HTTP_PROXY HTTPS_PROXY ALL_PROXY http_proxy https_proxy all_proxy
.venv/Scripts/python.exe -m pytest <用例> -q -p no:randomly -s \
  --ui-skip-build --browser-channel msedge
```

脚本行为：
- 每条 ID 独立 `timeout`，一条挂住不影响其他；
- 自动写 `evidence/baseline/<ID>/manifest.txt`（commit / python / 命令 / 耗时 / 尾部输出）；
- 自动区分 `PASS / FAIL / TIMEOUT / HARNESS_ERROR`（pytest 参数错误不算测试失败）；
- 自动 export `PI_CODING_AGENT_DIR` 隔离全局 Pi 包。

---

## 7. 结论

**基线可用。** 16 条绿的是 Go 重构的「裁判」——它们证明了：
浏览器 → SPA → HTTP/SSE/WS → server → 隧道 → runner → Pi 整条链路在基线上工作，
且**每条都有可复跑的用例 + 落盘证据**。

4 条红里：
- **2 条是测试脚手架缺陷**（C2/C7），Go 阶段要换验收方法，不是复刻对象；
- **1 条是真缺陷**（D5），Go 阶段要改进错误可观测性；
- **1 条是 Windows 环境阻塞**（D3），换 Linux 补证。

下一步按计划进入 P0：写 `openapi-diff` 工具 + 旧库直读器 —— 这两条（A1–A6）不需要浏览器，
但必须先绿，否则后面 16 条 Playwright 用例没法安全地打到 Go 上。

---

## 8. 环境隔离清单（下次跑基线前必看）

基线之所以前几次不稳定，全部因为**继承了开发机上的全局状态**。已固化到脚本，但记录原因：

| 继承来的东西 | 后果 | 隔离方式 |
|---|---|---|
| 全局 Pi 包（`pi-cworker`/`pi-subagents`/`pi-agent-goal`/`pi-intercom`） | `pi` 启动即 `Failed to load extension` 退出 → C1/C5 时绿时红 | `export PI_CODING_AGENT_DIR=<evidence>/pi-agent-dir` |
| `HTTP_PROXY`/`HTTPS_PROXY` | mock LLM 的 127.0.0.1 请求被代理吞掉 → `ConnectTimeout` | `unset` 全部 `*_PROXY`，`NO_PROXY=127.0.0.1,localhost` |
| 真模型凭据（`ANTHROPIC_API_KEY` 等） | 测试意外打到真模型，结果不确定且花钱 | `unset` 全部模型 key |
| 系统 locale（zh-CN/GBK） | YAML/bundle 读写 GBK 崩 | `PYTHONUTF8=1` |
| 父进程的 `OMNIGENT_RUNNER_*` / `RUNNER_SERVER_URL` | 子测试连到控制者 session 的 runner | `run-baseline.sh` 已清；手工跑需自己清 |
| 全局 `~/.omnigent/chat.db` | 污染用户数据 | pytest 已用 tmp 库；手工跑要显式 `--database-uri` |

检查一条是否被污染的最小方法：

```bash
# 若这条能过，说明隔离生效
bash designs/go-rewrite/run-baseline.sh C1
# 期望：PASS，且耗时 ~36s（不是 55s+）
```
