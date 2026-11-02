# Omnigent 现状架构梳理（前端 / 后端）

> 目标：为「后端用 Go 重写」提供基线。只描述**当前**事实，不含建议。
> 数据采集于本地 checkout（`6d209c0`），所有数字为实测行数 / 文件数。

## 0. 规模基线

| 区域 | 文件数 | 行数 | 备注 |
|---|---|---|---|
| `omnigent/**/*.py`（生产代码） | 1 012 | 578 296 | 后端全部 |
| `tests/**/*.py` | 2 779 | 1 228 854 | 是迁移时唯一的「裁判」候选 |
| `web/src/**/*.{ts,tsx}` | 1 437 | 454 711 | 非测试 199 392 行 |
| `web/electron/src/*.js` | 69 | — | 桌面壳 |
| `web/android`（Kotlin）/ `web/ios`（SwiftUI+WKWebView） | — | — | 薄壳，只承载 WebView |
| `openapi.json` | — | 743 KB | OpenAPI 3.2.0，117 path / 154 operation / 194 schema |

后端 Python 模块规模（行数）：

| 模块 | 行数 | 说明 |
|---|---|---|
| `server/` | 110 217 | 控制面（FastAPI）。其中 `routes/` 68 008 行、219 个 handler |
| `harnesses/` | 101 801 | 12 个 native（TUI 包装）家族；claude_native 30 730、codex_native 22 462、antigravity 11 584、opencode 7 068、pi_native 4 504 |
| `runner/` | 71 044 | 数据面；`app.py` 9 072、`native/orchestration.py` 11 422、`tool_dispatch.py` 9 348、66 个 handler |
| `inner/` | 63 271 | executor 层；codex 5 091、claude_sdk 3 892、**pi 3 481**、terminal 2 965、acp 2 058 |
| `onboarding/` | 28 898 | 安装/登录/凭据/模型发现（Databricks 为主） |
| `runtime/` | 25 955 | 会话循环；`workflow.py` 3 329、`_scaffold.py` 2 084、`_executor_adapter.py` 1 835、`process_manager.py` 1 783 |
| `stores/` | 18 368 | SQLAlchemy store |
| `tools/` | 17 245 | 40 个 builtins 模块，共 35 个 `sys_*` 工具 |
| `host/` | 17 390 | host daemon、zygote、本地 server 生命周期 |
| `cli.py` | 14 839 | click CLI，19 个 `@cli.command` |
| `db/` | 14 738 | 19 张表、118 个 alembic migration |
| `spec/` | 11 446 | Agent YAML spec 解析/校验 |
| `policies/` | 11 414 | CEL + callable 策略 |
| `repl/` | 13 387 | 终端 REPL |
| `llms/` `models/` `entities/` `terminals/` `native/` | ~20 000 | LLM 客户端、模型目录、实体、终端、native 公共层 |

## 1. 进程拓扑（后端最关键的形状）

```
                ┌──────────────────────────────────────────────┐
  浏览器/CLI ──▶│  server（控制面，FastAPI）                     │
                │  routes → stores(DB) / RunnerRouter          │
                └───────┬───────────────────────┬──────────────┘
                        │ WS 隧道 (帧协议)       │ WS 隧道
                        ▼                       ▼
                ┌───────────────┐       ┌──────────────┐
                │ runner 进程    │       │ host daemon  │
                │ 每 session 一个│◀──────│ 启停 runner  │
                └──┬────┬───────┘       └──────────────┘
                   │    │ UDS(POSIX)/loopback TCP(Windows)
                   │    ▼
                   │  ┌──────────────────────────────┐
                   │  │ harness 子进程（每会话一个）   │
                   │  │ FastAPI + HarnessApp scaffold│
                   │  │ POST /v1/sessions/{id}/events│
                   │  └──────────┬───────────────────┘
                   │             ▼
                   │      Executor（in-process）
                   ▼             ▼
              terminals    pi --mode rpc / vendor SDK / native TUI
```

要点（源码依据）：

- **server** 只做持久化、鉴权、编排、SSE 多路复用，**不直接跑 agent**。`server/app.py:create_app` 有 38 处 `app.include_router(...)`（除 `/health`、`/api/version`、`.well-known` 外全部 `prefix="/v1"`）。
- **runner** 每 session 一个进程（`host/connect.py` 注释 "One live runner per session"），持有 harness 子进程、终端、MCP 连接池、工具派发器。入口 `runner/_entry.py`，也可通过 UDS/TCP 直连（`runner/transports/{uds,tcp}.py`）。
- **harness** 是最小单元：一个 FastAPI app，只暴露 `POST /v1/sessions/{conversation_id}/events` + SSE，外加 `/health`；用 per-spawn bearer token 鉴权（`OMNIGENT_HARNESS_AUTH_TOKEN`）。公共脚手架在 `runtime/harnesses/_scaffold.py`（`HarnessApp`），执行器适配在 `_executor_adapter.py`。
- **POSIX 上还有 zygote**：`runner/_zygote.py` 起一个常驻进程预导入依赖，然后 `fork()` 出每个 runner，省掉每会话 ~120MB 的冷导入。
- **host daemon**（`host/`，6 217 行的 `connect.py`）负责本机 runner 生命周期、服务安装（launchd/systemd）、本地 server 的 pidfile 复用。

### 1.1 三条跨进程协议

| 协议 | 位置 | 形状 |
|---|---|---|
| **Runner WS 隧道** | `runner/transports/ws_tunnel/frames.py` | JSON 帧：`hello` / `request` / `response.head` / `response.body`(encoding=utf-8\|base64) / `response.end` / `request.cancel` / `ping`·`pong` / `ws.open`·`ws.frame`·`ws.close` / `event.ready`·`event.batch`·`event.ack`。首次 `hello` 带 `frame_protocol_version`（严格 major） |
| **Host WS 隧道** | `host/frames.py` | 只有控制帧（launch/stop runner + 结果），不承载 HTTP 流量；错误码 `harness_not_configured` / `workspace_missing` / `launch_handler_failed` |
| **Harness HTTP** | `runtime/harnesses/_scaffold.py` | 下行事件 `POST /v1/sessions/{id}/events`，`type` = `message`/`interrupt`/`tool_result`/`approval`；上行 SSE |

## 2. 后端分层与职责

### 2.1 server（控制面）

`omnigent/server/`：

- `app.py`（5 465 行）`create_app()`：中间件（gzip、审计、请求 id）、静态 SPA 挂载（`static/web-ui`，`OMNIGENT_WEB_UI_DIST` 可覆盖）、默认 agent 播种、router 装配、lifespan（harness process manager / scheduler / reaper）。
- `routes/`：54 个模块。体量集中在 `routes/_sessions/{orchestration.py 13 951, helpers.py 12 537}`、`sessions/routes_{core 4 508, resources 4 214, events 3 983}`。
- `schemas.py`（5 489 行，194 个 schema）是**唯一的线上契约**：harness 子进程也复用它，不存在第二套协议模块。
- `auth.py`：三种模式 `header`（默认，`X-Forwarded-Email`）/ `oidc` / `accounts`（OSS 默认，首个用户即 admin）；cookie 校验路径统一。
- `presence.py`、`session_live_state.py`：SSE 订阅槽、快照+增量去重、心跳（15s）。
- `feature_flags.py`：15 个 `OMNIGENT_FEATURES` 开关。
- `managed_hosts.py` / `provider_sandbox_profiles.py` / `managed_sandbox_reaper.py`：托管沙箱（Modal/Daytona/E2B/K8s…）。
- `scheduled/`、`background_session_titles.py`、`smart_routing.py`、`mcp_*`、`dictation*`、`github_app*`、`oidc*`、`accounts_*`。

### 2.2 会话读模型与事件（前端直接依赖）

- 持久化条目 11 种（`db/enum_codecs.py:ITEM_TYPE`）：`message`、`function_call`、`function_call_output`、`reasoning`、`error`、`compaction`、`native_tool`、`resource_event`、`routing_decision`、`slash_command`、`terminal_command`。
- 线上事件 union `ServerStreamEvent` **56 个变体**（`openapi.json` 实测：oneOf 56 项，与 discriminator.mapping 56 键一致；定义在 `server/schemas.py:5118`，discriminated by `type`）。每个事件带 `sequence_number`（由 SSE 序列化器赋值）。注：`HarnessStreamEvent` 是 runner 与 harness 之间的内部事件，不出现在 `openapi.json` 中。
- 断线语义（`routes/sessions/__init__.py` 文档）：**snapshot + live tail，不是 replay**。客户端开流 → GET 快照 → 按 item id 去重。
- 每个会话两条长连接：`GET /v1/sessions/{id}/stream`（SSE，事件+presence）和 `WS /v1/sessions/updates`（侧边栏 watch-set 增量：`watch` → `snapshot`/`changed`/`removed`/`heartbeat`）。

### 2.3 runner（数据面）

- `app.py:create_runner_app()`：`POST /v1/sessions`（初始化并加载 spec）、`POST /v1/sessions/{id}/events`、资源路由（终端/工作区/文件/git/PR）、`model_option_routes.py`（按 harness 分派的模型候选）、`mcp_routes.py`（`/v1/sessions/{id}/mcp/execute`、`/v1/elicitations/{id}`）。
- `tool_dispatch.py`（9 348 行）：本地派发 `sys_os_*`（OSEnvironment）、`sys_call_async` 类（REST）、文件类、终端类、MCP 类；**executor 自己不执行工具**，只发 `action_required`，runner 统一派发。
- `native/orchestration.py`（11 422 行）：12 个 `<x>_native` 的 terminal 自动创建、transcript forwarder 监管、权限/usage/compaction 镜像。当前是 `if harness_name == "..."` 的 11 路硬编码分派（`designs/harness-modular-registry-proposal.md` 已把它列为待抽象的技术债）。
- `transports/ws_tunnel/`（4 845 行）：把 runner 的 ASGI 应用反向暴露给 server。

### 2.4 executor / harness 抽象

`inner/executor.py`（820 行）定义唯一的 agent 后端接口：

```python
class Executor:
    async def run_turn(self, messages, tools, system_prompt, config) -> AsyncIterator[ExecutorEvent]
    # 能力声明
    def supports_streaming/handles_tools_internally/replays_tool_history/supports_live_message_queue/...
    async def interrupt_session(session_key) / enqueue_session_message(session_key, content) / close_session(...)
```

事件类型：`TextChunk`、`ReasoningChunk`、`ToolCallRequest`、`ToolCallComplete`、`CompactionStarted/Complete`、`SubAgentStarted/Completed/ToolCall`、`TurnComplete`、`TurnCancelled`、`ExecutorError`。

`runtime/harnesses/_executor_adapter.py`（1 835 行）把它翻译成 Omnigent SSE，并安装稳定的 tool / elicitation / policy 桥。

### 2.5 harness 注册表与能力矩阵

- `harness_plugins.py`（1 340 行）：`HarnessContribution` / `NativeCodingAgent` / `HarnessCapabilities`（16 个能力轴：`integration_mode`、`elicitation`、`resume`、`effort`、`model_family`、`auth`、`subagents`、`interrupt`、`streaming`、`steering`、`live_queue`、`images`、`compaction`、`fork_history`、`shell_tool_*`、`instruction_delivery`）。
- 24 个 harness id（`pi`、`pi-native`、`claude-sdk`、`codex`、`codex-native`、`opencode-native`、`cursor`、`copilot`、`antigravity`、`qwen`、`kimi`、`goose`、`hermes`、`kiro-native`、`devin-native`、`acp`、`acp:<slug>`…）+ 21 条别名。另有两个预置 ACP 行（`grok`、`jcode`）通过 `ACP_CLI_HARNESSES` 声明，共享同一个 `acp_harness` 实现——这是「加 agent 只加一行数据」的现有范例。
- 可选 harness 通过 Python entry point `omnigent.community.harness` 注入；`GET /v1/harnesses` 回吐 catalog（含 capabilities），前端据此渲染 picker。

### 2.6 数据层

- SQLAlchemy 2.0 **同步** + `asyncio.to_thread` 包裹（`helpers.py` 里 69 处）；19 张表：`agents`、`files`、`users`、`preferences`、`account_tokens`、`connections`、`device_grants`、`session_permissions`、`omnigent_conversation_metadata`、`projects`、`conversations`、`conversation_items`、`conversation_labels`、`comments`、`policies`、`hosts`、`user_daily_cost`、`scheduled_tasks`、`scheduled_task_runs`。
- 支持 SQLite / PostgreSQL / MySQL / CockroachDB；`Uuid16` 自定义类型把 32 hex 存成 16 字节；枚举存 `SMALLINT` 稳定码；`CompressedText` 用 zstd（NUL 哨兵 + codec 字节的帧格式，兼容旧明文）。
- 迁移 118 个 alembic 版本。

### 2.7 策略 / 工具

- `policies/`：`PHASE_REQUEST`、`PHASE_TOOL_CALL`（`FAIL_CLOSED_PHASES`），判定 ALLOW / ASK / DENY；ASKa 走 elicitation 卡片；CEL 表达式（`cel-python`）+ Python callable + 11 个内置策略（shell、cost、working_dir、safety、routing、orchestration…）。
- `tools/`：35 个 `sys_*`（`sys_os_shell`、`sys_session_*`、`sys_terminal_*`、`sys_timer_*`、`sys_scheduled_task_*`…）+ MCP 工具 + 子 agent 工具，由 `tool_manager` 按 spec 注册。

### 2.8 Pi 接入（v1 必须完整复刻的部分）

Pi 有**两条**独立路径：

**(a) `harness: pi` — RPC 子进程（SDK 型，主路径）**

- `inner/pi_executor.py`（3 481 行）起 `pi --mode rpc --no-session`，JSONL 走 stdin/stdout。
- CLI 参数：`--model`、`--thinking`、`--append-system-prompt` / `--system-prompt`（`pi_prompt.py`，replace 模式追加空 append 以抑制 `APPEND_SYSTEM.md` 发现）、`--no-skills` / `--skill <path>`、`--extension <path>`。
- 工具桥：把 Omnigent 工具 schema 生成**一个 JS extension**（`_generate_extension_js`，约 300 行模板），extension 调 `pi.registerTool()`，`execute()` 通过 **loopback TCP JSONL 工具服务器**回连（每 server 一个 `secrets.token_urlsafe(32)` token，`hmac.compare_digest` 校验）；帧 `{"id","token","tool","args"}`。
- 策略桥：extension 的 `pi.on("tool_call")` 对**非桥接的 Pi 原生工具**发 `kind:"policy_eval"` 帧，服务端跑 TOOL_CALL 策略；桥接工具已在 `/mcp` 侧评估，避免双评估。
- 轮次边界：`agent_settled`（pi ≥ 0.80.4）为准，老版本退化到 `agent_end`；`message_end` 捕获 per-call usage，`agent_end` 兜底；`compaction_end` 上报压缩结果（并用 `get_messages` 拉压缩后上下文）。
- 交互命令：`prompt`（带 `images` 原生字段、`streamingBehavior:"followUp"`）、`steer`、`abort`、`set_thinking_level`、`get_available_thinking_levels`、`get_messages`、`compaction_abort`。
- `extension_ui_request` → 转成 Web 端 elicitation 卡片（`pi_extension_ui.py`）。
- 凭据：`harnesses/pi_native/credentials.py`（2 063 行）生成 `models.json` + `PI_CODING_AGENT_DIR`，支持 Databricks / 通用 OpenAI-兼容 gateway / Anthropic Messages 三套 provider，token 通过工具服务器的 `gateway_auth` 帧按需刷新。

**(b) `harness: pi-native` — 包真 TUI**

- `harnesses/pi_native/`：tmux 里跑真 `pi`，web 消息经**文件 inbox bridge**（`bridge.py`）+ 2 825 行的 `resources/pi_native/omnigent_pi_native_extension.js` 注入（`pi.sendUserMessage`）。
- 策略走 HTTP：`POST /v1/sessions/{id}/policies/evaluate`（不走 turn 级 RPC 往返，因为 `_current_ctx` 已清空）。

## 3. 前端架构

### 3.1 技术栈

React 18 + Vite + TypeScript；TanStack Query（REST 缓存）+ Zustand（`chatStore.ts` 9 334 行）；Radix UI + Tailwind（39 个 ui 原子）；tiptap（composer 富文本）；`@xterm/*`（终端）；Monaco（代码查看）；streamdown + shiki + mermaid + katex（渲染）；OpenTelemetry web（`initBrowserTelemetry`）。

### 3.2 分层

```
web/src/
  main.tsx            启动序列：主题 → 字体 → trace → capabilities(/v1/info) → identity(/v1/me) → Router
  App.tsx             路由表（chat / grid / inbox / canvas / tasks / usage / settings / members / policies / extensions / approve）
  shell/  (154 k)     AppShell、Sidebar、NewChatDialog、PullRequestPanel、FileViewer、WorkspacePanel、CodeViewer…
  pages/  (52 k)      ChatPage(5 416)、SettingsPage(3 522)、TasksPage、UsagePage、Setup/Login/Register/Members/Policies…
  components/ (80 k)  chat/、composer/、blocks/、ui/、BrowserPane、ImageLightbox…
  hooks/  (54 k)      227 个 hook：SessionUpdatesProvider、RunnerHealthProvider、useSession、useConversations…
  lib/    (68 k)      sessionsApi(1 600)、events(1 055)、blockStream(1 066)、renderItems(1 988)、identity(716)、
                      capabilities、sessionHost、inbox、attachments、preferences…
  store/  (33 k)      chatStore、conversationRegistry、conversationState、streamSlots、terminalActivity、sessionImport
  extensions/         第三方扩展宿主（iframe srcdoc + 单 MessagePort 协议）
  canvas/ grid/       多会话画布 / 网格视图
```

### 3.3 数据流

1. **REST**：`authenticatedFetch`（`lib/identity.ts`）统一注入身份头 + replica 路由头（`X-Databricks-Omnigent-Slice-Key`），401 按 `login_url` 走登录。
2. **会话实时**：`openSessionStream()` → SSE → `parseSseStream` → `blockStream.ts`（reducer）→ `chatStore` → 渲染 `blocks`。事件类型定义在 `lib/events.ts`（1055 行，57 个 interface，**手工与 Python `ServerStreamEvent` 对齐**，文件头注明 parity 约定）。
3. **侧边栏**：`SessionUpdatesProvider` 单条 `WS /v1/sessions/updates`，watch-set 全量替换语义 + 服务端 diff 推送。
4. **条目渲染**：`renderItems.ts` 把持久化 item（11 种）与流式事件合成 block 列表，`itemsToBlocks`/`blockStream` 双路径保证「刷新后一致」。
5. **模型/harness 能力**：启动拉 `GET /v1/info` + `GET /v1/harnesses`，驱动 picker 与功能开关（避免前端硬编码）。

### 3.4 多端外壳

- **Electron**（`web/electron/src`，69 个 js）：主进程 + 若干 preload（browser action、ssh、arca 连接、find、update overlay、workspace picker），内置 Chromium 视图（`browserViewRegistry.js`、`chromeCookieStore.js`）。
- **Android**（Kotlin + `WebView`，`WebViewCompat.addWebMessageListener` 通道）、**iOS**（SwiftUI + `WKWebView`）：都只是加载远端 server 的薄壳。
- **VS Code 扩展**（`editors/vscode`）与 **Python UI SDK**（`sdks/ui`）、**web-extension SDK**（`sdks/web-extension`）。

## 4. 迁移视角下的「必守契约」

Go 版后端只要能满足下面 6 条，前端一行不用改：

1. `openapi.json`（3.2.0，154 operation）——建议直接当唯一真源做 spec-first 代码生成。
2. `ServerStreamEvent` 56 个变体的 `type` 字面量 + 字段名（snake_case）+ `sequence_number`。
3. SSE 语义：不 replay，`[DONE]` 收尾，15s 心跳，presence 随流注册。
4. `WS /v1/sessions/updates` 的 `watch`/`snapshot`/`changed`/`removed`/`heartbeat` 五种帧。
5. `POST /v1/sessions/{id}/events` 的 `type` 判别（含 `pending_id` / `denied` / `queued` / `recovered` 返回字段）。
6. Pi 的 RPC 线协议（见 2.8a）——这是 v1 唯一必须逐条对齐的外部协议。

## 5. v1 不需要搬的东西（明确剔除项）

`onboarding/`（Databricks 安装流）、`managed_hosts` / 沙箱 provider（Modal/Daytona/E2B/K8s…）、`dictation*`、`smart_routing` / `routing_backend`、`github_app*`（PR 资源可后置）、`repl/`、`cli` 的全部 19 个子命令、`harnesses/` 里 12 个 native 家族中除 `pi_native` 外的 11 个、`inner/` 里除 `pi_executor`/`pi_native_executor` 外的 executor、`extensions/` 插件体系、Android/iOS 外壳。
