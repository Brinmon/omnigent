# Go 后端验收追踪表

> 判据与规则见 [03-acceptance-plan.md](./03-acceptance-plan.md)。
> **一个 ID 一行，空着的格子 = 没验收。** 只有 Playwright 实战通过（附证据）才能填 `✅`。

**基线状态（已全量跑完）**：**16 PASS / 4 FAIL / 1 SKIP**。详见 [BASELINE-REPORT.md](./BASELINE-REPORT.md)。
**基线 commit**：`6d209c0` + 7 处可移植性修复（见报告 §3）。
**Go commit**：—
**浏览器**：系统 Edge 156（`--browser-channel msedge`），未下载 Chromium。
**重跑**：`bash designs/go-rewrite/run-baseline.sh [ID...]`

图例：`⬜` 未开始 · `🟡` 基线绿·Go 未做 · `🟦` Go 绿·基线未跑 · `✅` 双侧绿且证据齐 · `🚫` V1 明确不做（须填理由） · `⛔` 阻塞（须填原因）

---

## 阶段 A · 契约层（无浏览器，但阶段门禁）

| ID | 判据 | 命令 / 用例 | 基线 | Go | 证据 | 勾 |
|---|---|---|---|---|---|---|
| A1 | OpenAPI path 集合等价 | `go run ./cmd/openapi-diff --old openapi.json --new http://127.0.0.1:8080/openapi.json` | — | — | `evidence/A1/` | ⬜ |
| A2 | 154 operation 全部实现 | 同上 | — | — | `evidence/A2/` | ⬜ |
| A3 | 194 schema 字段/必填/枚举一致 | 同上 | — | — | `evidence/A3/` | ⬜ |
| A4 | `ServerStreamEvent` 57 变体 `type` 集合一致 | `go test ./internal/api -run StreamEventGolden` | — | — | `evidence/A4/` | ⬜ |
| A5 | 旧库（Python 写）Go 可直读 | `go test ./internal/db -run Compat` | — | — | `evidence/A5/` | ⬜ |
| A6 | Uuid16 / SMALLINT 枚举 / CompressedText 兼容 | 同上（含 zstd 帧 + 旧明文两种行） | — | — | `evidence/A6/` | ⬜ |

---

## 阶段 B · 启动链路

| ID | 判据 | 用例 | 基线 | Go | 证据 | 勾 |
|---|---|---|---|---|---|---|
| B1 | SPA 启动 + `/v1/info`/`/v1/me`/`/v1/harnesses` 不降级 | `tests/e2e_ui/chat/test_smoke.py` | ✅ 43s | — | `evidence/B1/` | 🟡 |
| B2 | 会话列表渲染（sidebar 出历史） | `tests/e2e_ui/sessions/test_sidebar_search.py`、`tests/e2e_ui/sessions/test_sidebar_rename.py` | ✅ 47s | — | `evidence/B2/` | 🟡 |
| B3 | 历史 hydration（刷新后位置不漂移） | `tests/e2e_ui/chat/test_initial_history_loading.py`、`tests/e2e_ui/chat/test_reload_continue.py` | ✅ 50s | — | `evidence/B3/` | 🟡 |
| B4 | 发消息 → SSE 流式 → assistant 气泡非空 | `tests/e2e_ui/chat/test_smoke.py::test_send_message_renders_assistant_response` | ✅ 30s | — | `evidence/B4/` | 🟡 |
| B5 | `WS /v1/sessions/updates` 增量（新会话即时出现） | `tests/e2e_ui/sessions/test_session_updates_stream.py` | ✅ 58s | — | `evidence/B5/` | 🟡 |

---

## 阶段 C · Pi agent（V1 实质内容）

| ID | 判据 | 用例 | 基线 | Go | 证据 | 勾 |
|---|---|---|---|---|---|---|
| C1 | 真 `pi --mode rpc` 一轮问答（`agent_settled` 边界） | `tests/e2e/omnigent/test_per_harness_pi.py` | ✅ 36s | — | `evidence/C1/` | 🟡 |
| C2 | 工具调用往返（TCP 工具服务器 + JS extension） | `tests/e2e/omnigent/test_pi_long_tool_turn.py` | ❌ 59s | — | `evidence/C2/` | ⛔ |
| C3 | 工具历史复用（次轮回用不重调） | 官方 `tool-history`；用 `mock` 排两轮 | ⬜ | — | `evidence/C3/` | ⬜ |
| C4 | 中断后同会话可继续 | `tests/e2e/test_pi_interrupt_reap_budget_race_e2e.py` | ✅ 8s | — | `evidence/C4/` | 🟡 |
| C5 | thinking / reasoning 流式渲染 | `tests/e2e/omnigent/test_pi_thinking_compatibility_e2e.py` | ✅ 186s | — | `evidence/C5/` | 🟡 |
| C6 | provider 重试恢复不截断轮次 | `tests/e2e/omnigent/test_pi_retry_recovery_e2e.py` | ✅ 34s | — | `evidence/C6/` | 🟡 |
| C7 | 压缩（`compaction_end` + `get_messages`） | `tests/e2e/test_pi_compact_restart_resume.py` | ❌ 54s | — | `evidence/C7/` | ⛔ |
| C8 | 策略 ASK 卡片往返后继续 | `tests/e2e/test_pi_extension_ui_round_trip.py` | ⚠️ 6s | — | `evidence/C8/` | 🚫 |
| C9 | 策略 DENY 无副作用且会话可继续 | 官方 `approvals`；`tests/e2e_ui/approvals/test_approval_card.py` | ⬜ | — | `evidence/C9/` | ⬜ |
| C10 | 模型 / effort 切换生效 | `tests/e2e/test_pi_native_model_scope_e2e.py`（Chat 部分） | ⬜ | — | `evidence/C10/` | ⬜ |

---

## 阶段 D · 策略与资源

| ID | 判据 | 用例 | 基线 | Go | 证据 | 勾 |
|---|---|---|---|---|---|---|
| D1 | `PHASE_REQUEST`/`PHASE_TOOL_CALL` 拦截生效 | `tests/e2e_ui/approvals/test_approval_card.py`、`test_ask_user_question.py` | ✅ 36s | — | `evidence/D1/` | 🟡 |
| D2 | CEL 策略结果与 Python 逐例一致 | 对拍脚本（同表达式 × 同事件样本） | ⬜ | — | `evidence/D2/` | ⬜ |
| D3 | ~~终端创建/读/写/关闭~~ | ~~`tests/e2e_ui/shells/test_new_shell.py`~~ | 🚫 不适用 | — | — | 🚫 |
| D4 | 文件上传/下载/列表 | `tests/e2e_ui/files/`（挑上传与列表各一） | ⬜ | — | `evidence/D4/` | ⬜ |
| D5 | 工作区文件树 + 内容搜索 | `tests/e2e_ui/files/test_file_search.py` | 🟡 500 已修 | — | `evidence/D5/` | 🟡 |
| D6 | Fork 会话（带历史、与父隔离） | `tests/e2e_ui/fork_session/test_fork_from_middle.py`、`test_fork_response_not_full_transcript.py` | ✅ 37s | — | `evidence/D6/` | 🟡 |

---

## 阶段 E · 会话生命周期

| ID | 判据 | 用例 | 基线 | Go | 证据 | 勾 |
|---|---|---|---|---|---|---|
| E1 | 重命名 / 置顶 / 归档 / 删除 | `tests/e2e_ui/sessions/test_sidebar_rename.py`、`test_sidebar_pin_unpin.py`、`test_sidebar_delete.py`、`test_archived_session_header_menu.py` | ✅ 78s | — | `evidence/E1/` | 🟡 |
| E2 | 搜索（标题 + 内容 + archived 标记） | `tests/e2e_ui/sessions/test_session_search.py`、`test_sidebar_search.py` | ✅ 51s | — | `evidence/E2/` | 🟡 |
| E3 | 刷新/断线重连（snapshot + live tail 去重） | `tests/e2e_ui/chat/test_reload_continue.py`、`tests/e2e_ui/sessions/test_session_updates_stream.py` | ✅ 26s | — | `evidence/E3/` | 🟡 |
| E4 | presence（共同浏览者） | `tests/e2e_ui/collaboration/test_collab_realtime.py` | ✅ 71s | — | `evidence/E4/` | 🟡 |
| E5 | 未读标记跨刷新存活 | `tests/e2e_ui/sessions/test_sidebar_mark_unread.py`、`tests/e2e_ui/approvals/test_inbox_unread.py` | ✅ 43s | — | `evidence/E5/` | 🟡 |

---

## 阶段门禁

| 门 | 要求 | 状态 |
|---|---|---|
| G0 → P1 | A1–A6 全 ✅ | ⬜ |
| P1 → P2 | B1–B3、E1–E2 全 ✅ | ⬜ |
| P2 → P3 | B4–B5、E3 全 ✅ | ⬜ |
| **P3 → P4（V1 关键门）** | C1–C4、C6 全 ✅ | ⬜ |
| P4 → P5 | C8–C10、D1–D2、D4–D5 全 ✅ | ⬜ |
| P5 → P6 | C5、C7、D6、E4–E5 全 ✅ | ⬜ |
| P6 切换 | 全表 ✅ + 档位 3 全量双跑 | ⬜ |

---

## 待办：V1 明确不做（需写理由，不得静默跳过）

| 项 | surface | 理由 |
|---|---|---|
| **交互终端（Web UI 内 PTY）** | `terminals/`、`test_new_shell*.py` | **决定：整个 V1 不做，Go 侧首次实现。** Python 版终端栈绑死 `tmux -S <unix-socket>`（全仓库 85 处），Windows 无 Unix domain socket，移植需把寻址改成 `-L <name>` 并改 11 个 native bridge —— 成本远超收益。Pi harness 自带 bash 工具，agent 执行命令不依赖它；人要看文件直接用系统终端开 `E:/tmp/omnigent-sessions/<sid>/workspace` 等价。**时机：Go 重构到基本可用后，在 Go 里用 `creack/pty`（POSIX）/ ConPTY（Windows）首次实现**，不背 Python 的 tmux 包袱 |
| `pi-native`（真 TUI 包装） | Native 终端 | V1 只做 `harness: pi`（RPC 子进程）。native TUI 需 tmux + 文件 inbox bridge + 2 825 行 JS extension，属 V2 |
| 托管沙箱（Modal/Daytona/E2B/K8s） | 全部 | V1 只支持本机 runner；`/v1/sandbox-providers` 返回空 catalog，前端已有降级路径 |
| Databricks onboarding / 凭据刷新 | Web 新会话 | V1 用环境变量单 provider；`CredentialProvider` 接口已预留 |
| dictation / smart routing / GitHub App | 对应页面 | 不在 V1 切片内 |
| 移动端 / Electron 壳改造 | Alternative UI | 复用 `web/` 原样，壳只需能连 Go server |
