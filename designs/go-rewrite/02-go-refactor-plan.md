# Go 后端重构方案（V1：只支持 Pi，但按多 agent 可扩展设计）

**目标**：用 Go 重写 Omnigent 后端，前端（`web/` 原样）零改动可用；V1 只接 Pi agent，但**接口层从第一天就按 N 种 agent 设计**，后续接 Codex / Claude Code 时只加适配器、不动内核。

**非目标（V1）**：托管沙箱、Databricks onboarding、dictation、smart routing、GitHub App、VS Code/移动壳改造、CLI 多命令复刻、除 Pi 外的 11 个 native TUI 家族。

---

## 1. 结论先行

1. **不要整体移植。** 578 K 行 Python 里，Pi + 控制面 + runner 必需的只有约 **10.6 万行**（pi executor 1.1 万 / server 会话面 5.1 万 / stores+db 2.2 万 / runner 核心 2.4 万，见 §2）。剩下 47 万行是 11 个 harness 家族、托管沙箱、onboarding、CLI、REPL。
2. **审计一次、生成两次。** 把 `openapi.json` 3.2.0（154 operation / 194 schema）当唯一真源，用 `oapi-codegen` 生成 Go 类型与 server 接口；SSE 的 57 个事件变体用「生成 + 手写 union」补齐。这样「前后端对齐」从人工比对变成编译期错误。
3. **保留进程拓扑，不要合并成一个单体。** server / runner / harness 三段式是 Omnigent 唯一真正的架构约束（harness 需要在自己的环境里跑 vendor CLI，且要和容器/沙箱/远端 host 解耦）。Go 化后这三个仍然是独立二进制，隧道帧协议照抄。
4. **Pi 的接入面是「JSONL RPC + 一个 JS extension + 一个 loopback TCP 工具服务器」**，与语言无关。这部分是 V1 的核心工作量，也是最该逐字节对齐的部分（§5）。
5. **多 agent 扩展点设计成 4 个 Go interface**（`AgentBackend`、`ToolBridge`、`EventSink`、`CredentialProvider`，§4），Pi 是第一个实现；Codex/Claude 后续只注册新 backend。
6. **现实工期**：单人有经验工程师 ≈ **10–14 周**；2–3 人 ≈ **6–8 周**。风险不在 Go，在「57 个 SSE 事件 × 前端 reducer 的行为等价」和「Pi RPC 的边界语义」。

---

## 2. V1 切片（要搬什么）

| 目标（Go） | 来源（Python） | 行数 | 处理方式 |
|---|---|---|---|
| `api/`（类型 + server 接口） | `openapi.json` + `server/schemas.py` | 生成 | `oapi-codegen` 生成；事件 union 手写 |
| `server/`（HTTP 面） | `server/routes/{sessions,hosts,projects,policies,...}` + `server/*.py` | 51 000 | 重写，按 openapi 接口实现 |
| `store/` + `db/` | `stores/*`、`db/db_models.py`、`db/utils.py` | 21 600 | 手写 SQL + sqlc，或 GORM → 见 §3 |
| `runner/` | `runner/{app,tool_dispatch,routing,transports}` | 23 600 | 重写；`tool_dispatch` 是重点 |
| `agent/pi/` | `inner/{pi_executor,pi_harness}`、`pi_prompt.py`、`pi_extension_ui.py`、`harnesses/pi_native/{credentials,bridge}` | 11 100 | 移植，逐条对齐协议 |
| `policy/` | `policies/*` + `runtime/policies` | 11 400 | CEL 用 `cel-expr/cel-go`（google/cel-go 已转只读兼容仓库） |
| `resources/pi_native/omnigent_pi_native_extension.js` | 同路径 | 2 825 | **原样复用**（JS，无需改） |
| 前端 `web/` | — | 0 | **零改动** |

**剔除**（V1 不做，留 TODO 注释指向 Python 实现）：`onboarding/` 28.9 K、`harnesses/` 中 12 个 native 家族里除 `pi_native` 之外的 11 个（约 9.7 万行）、`inner/` 中除 pi 两个 executor 之外的实现（约 5.9 万行）、`cli.py` 14.8 K、`repl/` 13.4 K、`host/` 中的服务安装与 zygote、托管沙箱、`extensions/`。

---

## 3. 开源库选型

### 3.1 HTTP / 路由

| 选择 | 库 | 理由 |
|---|---|---|
| **推荐** | `net/http` + `chi` `v5` + `oapi-codegen` `v2` | chi 是 `http.Handler` 中间件生态的中立底座；oapi-codegen 能直接吃现有 `openapi.json`，生成 `ServerInterface`，实现不了就编译失败——这是「前端零改动」的强制保障 |
| 备选 | `huma` `v2` | 自带 OpenAPI 生成 + 校验，但它是**以代码为真源**，与「保住现有 154 operation 契约」方向相反；只在后续新增 API 时考虑 |
| 不选 | Gin / Echo / Fiber | 与 oapi-codegen 也能配，但中间件/context 模型会诱导偏离 `net/http`，且本仓库需要大量原生 SSE / WebSocket / hijack，标准库更省事 |

配套：`go-playground/validator/v10`（请求校验）、`google/uuid`、`spf13/cobra` + `spf13/viper`（CLI 与配置）。

### 3.2 实时传输

| 用途 | 库 | 说明 |
|---|---|---|
| SSE（57 事件 / 心跳 / `[DONE]`） | `net/http` + `http.Flusher`（或 `sse` 小库） | 事件是 JSON，别用重型 SSE 框架；**必须 `Flush()` after each event**，否则前端收不到增量 |
| WebSocket（tunnel / updates / terminal attach） | `coder/websocket`（原 `nhooyr.io/websocket`） | `context.Context` 一等公民、并发写安全、零依赖；`gorilla/websocket` 已归档 |
| 隧道帧编解码 | 手写（`encoding/json`） | 帧协议只有 13 种 kind，手写比引入 RPC 框架更可控 |

### 3.3 数据库

| 选择 | 库 | 说明 |
|---|---|---|
| **推荐** | `pgx/v5` + `sqlc` + `goose` | sqlc 从 SQL 生成类型安全代码，读路径可控；`conversation_items` 的 `(workspace_id, conversation_id, position)` 前缀扫描必须能手写 |
| 可选 | `sqlc` + `modernc.org/sqlite`（纯 Go，无 CGO） | V1 先支持 SQLite + PostgreSQL 双后端，MySQL/CockroachDB 后置 |
| 迁移 | `goose`（`pressly/goose/v3`） | 用 Python `db/migrations/versions/*.py`（118 个）的人工审计结果，写等价的 `.sql` 基线，不自动翻译 alembic |
| 不选 | GORM | 会隐藏本仓库最关心的锁/顺序语义（`next_position` 分配器、`SKIP LOCKED` 类并发）；且跨 4 种方言行为不一致 |
| 不选 | `lib/pq` | 维护模式 |

**关键**：`Uuid16`（32 hex ↔ 16 字节）、枚举 `SMALLINT` 稳定码、`CompressedText`（`0x00` + codec 字节 + zstd 帧）三件事必须**逐字节兼容**，否则旧库里已有的数据读不出来。zstd 用 `klauspost/compress/zstd`。

### 3.4 其它

| 领域 | 库 |
|---|---|
| 日志 | 标准库 `log/slog`（+ `slog-multi`/`slog-formatter` 需要时） |
| 错误 | 标准库 `errors` + `fmt.Errorf("%w")`；需要属性/上下文时 `samber/oops` |
| 集合工具 | `samber/lo`（只在 2 段以上链式变换时用，单次循环用 `for`） |
| 配置 | `spf13/viper` + 环境变量（保留 `OMNIGENT_*` 全部变量名） |
| MCP | `modelcontextprotocol/go-sdk`（官方，Apache-2.0，v1.8+，支持 2026-07-28 协议且向后兼容） |
| CEL 策略 | `cel-expr/cel-go`（原 `google/cel-go` 已迁址，只读兼容仓库） |
| 全文检索 | V1 用 `ILIKE`/`LIKE lower(...)` + 索引；PostgreSQL 侧沿用 `pg_trgm` |
| 终端（PTY） | `creack/pty`（POSIX）；tmux 用 one-shot `tmux` 子进程或 `tmux -CC` 控制模式 |
| 指标/追踪 | OpenTelemetry Go（`otelhttp`、`otelsql`） |
| 测试 | 标准库 `testing` + `stretchr/testify`；E2E 用 `testscript` 或 Go 版 httptest 打真 server |
| 构建 | `goreleaser` + `ko`；SPA 用 `go:embed`（见 §6） |
| 静态分析 | `golangci-lint` |

### 3.5 明确不引入

Agent 编排框架（LangChain-Go / eino / adk-go 等）——本项目的价值在「会话状态机 + 事件流 + 策略」，引入编排框架会导致与前端 57 个事件的对齐失控。Pi 本身就是 agent 框架，Go 侧只做**宿主**。

---

## 4. 多 agent 可扩展架构（V1 只有 Pi 实现）

用 4 个 interface 把「agent 家族差异」关在一个包里。内核只认 interface，永远不 `switch harness_name`。

```go
package agent

// 1) 后端：一次会话的 agent 生命周期（Pi 用 JSONL RPC 实现；将来 Codex 用 app-server JSON-RPC）
type Backend interface {
    // 由 spec 决定的静态身份
    ID() string                  // "pi" | "codex" | "claude-sdk" ...
    Capabilities() Capabilities  // 对齐 Python HarnessCapabilities 的 16 个轴

    // 生命周期
    Start(ctx context.Context, cfg SessionConfig) error
    Close(ctx context.Context) error

    // 驱动一轮
    RunTurn(ctx context.Context, in TurnInput, sink EventSink) error

    // 交互（不支持时返回 ErrUnsupported，而不是 panic）
    Interrupt(ctx context.Context) error
    Steer(ctx context.Context, msg UserMessage) error
    SetModel(ctx context.Context, model string) error
    SetEffort(ctx context.Context, effort string) error
    Compact(ctx context.Context) error
}

// 2) 工具桥：把宿主工具暴露给 agent（Pi = 生成 JS extension + loopback TCP）
type ToolBridge interface {
    // 返回需要注入 agent 的产物（文件路径 / 参数 / 环境变量）
    Prepare(ctx context.Context, tools []ToolSpec) (BridgeArtifacts, error)
    // agent 反向调用宿主工具的回调注册
    Serve(ctx context.Context) error
}

// 3) 事件出口：executor → 适配层（对应 Python 的 ExecutorEvent）
type EventSink interface {
    Text(delta string)
    Reasoning(delta string)
    ToolStart(callID, name string, args json.RawMessage)
    ToolEnd(callID, name string, result json.RawMessage, isErr bool)
    Usage(u Usage)
    Compaction(summary string, messages []Message)
    Error(err error, retryable bool)
}

// 4) 凭据：Pi 需要写 models.json + 定时刷新 token；Claude 用 API key；Codex 用自家登录
type CredentialProvider interface {
    Prepare(ctx context.Context, spec SessionConfig) (Env map[string]string, Files map[string][]byte, err error)
    Refresh(ctx context.Context) (token string, err error)  // Pi 的 gateway_auth 帧调用它
}
```

**注册表**（`agent/registry.go`）：

```go
var registry = map[string]Factory{}          // "pi" -> newPiBackend
func Register(id string, f Factory)          // 各 backend 包 init() 里注册
func Get(id string) (Backend, error)         // runner 按 spec.harness 取
```

**能力矩阵**用 Go 值类型复刻 Python 的 `HarnessCapabilities`（16 个字段），并**直接由 `GET /v1/harnesses` 吐给前端**——前端已经是数据驱动的（`lib/capabilities.ts` + `GET /v1/harnesses`），加新 agent 不需要改前端。

**为什么这样能「V1 只支持 Pi，但未来只加适配器」**：

- runner 里 ToolBridge 的选择、凭证的注入、Harnessapp 的能力判断全部来自 registry，没有 `if harness == "pi"`。
- 唯一 Pi 特有且**不需要**抽象的是 JS extension 内容——它是 ToolBridge 的产物（`BridgeArtifacts.Files`），Codex 版会产出自己的配置（如 `config.toml` / app-server 参数），内核不知道。
- `spec.harness: pi` 是唯一的接线点，与 Python 的 `HARNESS_MODULES["pi"] = "omnigent.inner.pi_harness"` 语义一一对应。

---

## 5. Pi 适配器（V1 核心，必须逐条对齐）

源码基准：`inner/pi_executor.py`（3 481 行）。Pi 官方协议文档在本机 `~/.pi/agent/install/releases/<ver>/node_modules/@earendil-works/pi-coding-agent/docs/{rpc,rpc-commands,json,extensions}.md`。

### 5.1 进程与 CLI

```
pi --mode rpc --no-session
   [--model <id>]                 # databricks 前缀特例：PI_CODING_AGENT_DIR 含 "databricks" 时加 "databricks/"
   [--thinking <level>]
   [--append-system-prompt <T>]   # append 模式
   [--system-prompt <T> --append-system-prompt ""]   # replace 模式（空 append 抑制 APPEND_SYSTEM.md 发现）
   [--no-skills] [--skill <path>]...
   [--extension <file>]...
```

环境：`_clean_pi_env` 白名单（前缀匹配 + 精确匹配两组，`PI_CODING_AGENT_DIR`、`HARNESS_PI_*` 等），**绝不 merge `os.environ`**（避免 host secret 泄漏进沙箱）。

### 5.2 输入命令（stdin JSONL）

| 命令 | 关键字段 | 用途 |
|---|---|---|
| `prompt` | `message`, `images[]`, `streamingBehavior:"followUp"`, `id` | 启动一轮 |
| `steer` | `message`, `id` | 轮内注入 |
| `abort` | `id` | 中断 |
| `set_thinking_level` | `level`, `id` | 努力度 |
| `get_available_thinking_levels` | `id` | 候选 |
| `get_messages` | `id` | 压缩后上下文回读 |
| `compaction_abort` | `id` | 打断压缩 |

### 5.3 输出事件（stdout JSONL）→ `EventSink`

| Pi 事件 | 映射 |
|---|---|
| `response` | 命令 ack；`success:false` → `ExecutorError` |
| `message_start` | 重置 message 级流式标记 |
| `message_update` + `assistantMessageEvent.type` | `thinking_start/delta/end` → `Reasoning`；`text_delta` → `Text`；`toolcall_*` → 记录 |
| `tool_execution_start` | `ToolStart`（带 `toolCallId`） |
| `tool_execution_end` | `ToolEnd`（`isError`；`result.content[].type=="text"` 抽文本） |
| `message_end` | 捕获 per-call `usage`；`stopReason=="error"` → `pending_error` |
| `agent_end` | 一次底层 run 结束；`willRetry` 忽略；老 pi 版本在此收尾 |
| `agent_settled` | **真正的轮次边界**（pi ≥ 0.80.4）；`aborted` 决定是否 `TurnCancelled` |
| `compaction_end` | `Compaction`；随后 `get_messages` 拉压缩后上下文 |
| `extension_ui_request` | 转 elicitation（`select`/`confirm`/`input`/`editor` 等 dialog 方法）；fire-and-forget 方法直接回 |

**边界语义（最容易做错的地方）**：

- `agent_end` ≠ 轮次结束。retry / overflow recovery / compaction / 排队消息都会继续，必须等 `agent_settled`。
- 空闲超时不是进程死亡：`stdout_at_eof()`（reader 结束 + 队列空）才判定 EOF，否则继续等。
- 出错后要 `evict` 这个 RPC 会话，不要复用。
- 文本已流出但消息失败时，pi 会重试并重新生成——不能把新文本接在旧片段后面。

### 5.4 工具桥（与语言无关的三个件）

1. **生成 JS extension**（Go 里用 `text/template` 渲染，产物落临时目录，`--extension` 传入）：`pi.registerTool({name,label,description,promptSnippet,parameters,execute})`，`execute` 通过 loopback TCP 回连。
2. **loopback TCP 工具服务器**（`net.Listen("tcp","127.0.0.1:0")`）：JSONL 请求 `{"id","token","tool","args"}`；token `crypto/rand` 32 字节，`hmac.Equal` 常数时间比较；帧大小上限（Python 侧 `_TOOL_REQUEST_LIMIT`）；`kind:"gateway_auth"` 返回刷新后的凭据；`kind:"policy_eval"` 只评估不执行。
3. **策略钩子**：extension 的 `pi.on("tool_call")` 对**非桥接**工具发 `policy_eval`；桥接工具在 MCP 侧已评估，**必须跳过**否则双弹窗。

### 5.5 凭据

`models.json` + `PI_CODING_AGENT_DIR`，三套 provider（OpenAI Responses / Anthropic Messages / OpenAI Completions），按 model family 路由；gateway token 刷新在 `Refresh()` 里，由工具服务器的 `gateway_auth` 帧按需触发。V1 先实现「单 provider + 环境变量 token」的最小闭环，Databricks profile 分支留接口。

### 5.6 系统提示词

`runtime/prompt.py` 的组合逻辑：spec instructions（用户）在前，框架指令在后，且**必须保持顺序**；`pi_prompt_args` 的 append/replace 语义见 5.1。

---

## 6. 仓库布局与前端复用

```
go-backend/                      # 与现有 Python 并存，一个 monorepo
  cmd/omnigent-server/main.go
  cmd/omnigent-runner/main.go
  cmd/omnigent/main.go           # 最小 CLI：server/run/resume/version
  internal/
    api/            # oapi-codegen 产物 + SSE union
    controlplane/   # server 实现（routes、auth、presence、sse hub）
    runner/         # runner 实现（tool_dispatch、resources、tunnel client）
    agent/          # Backend/ToolBridge/EventSink/Credential 接口 + registry
    agent/pi/       # 唯一 V1 实现
    store/          # sqlc 产物 + store 实现
    db/             # 连接、迁移嵌入、Uuid16、CompressedText、enum codec
    policy/         # CEL 引擎 + 内置策略
    tunnel/         # 13 种帧的编解码 + 双端
    webui/          # go:embed web/dist
  migrations/       # goose .sql（人工审计 alembic 后的基线）
  openapi/          # openapi.json 快照 + 生成脚本
```

SPA 复用：沿用 `web/` 原样构建，`go:embed all:webdist` 打包；开发模式 Vite 代理 `/v1`、`/api`、`/health` 到 Go（现有 `web/vite.config.ts` 的 `OMNIGENT_URL` 逻辑已经可用，只需指向 Go 端口）。`/v1/info`、`/v1/harnesses` 两个引导接口必须先实现，否则前端启动即降级。

**索引兜底**：SPA fallback 必须区分「文件不存在」与「路由不存在」——`/assets/*` 缺失返回 404，其余返回 `index.html`，否则前端会拿到 HTML 当 JS 解析。

---

## 7. 等价性验证（迁移的「裁判」）

现有 2 779 个 Python 测试是唯一可信裁判，但多数 import 内部实现，**无法跨语言复用**。因此需要先造裁判：

| 层级 | 做法 | 复用来源 |
|---|---|---|
| L1 契约 | `oapi-codegen` 生成失败即失败；Go server 的 `GET /openapi.json` 与旧 `openapi.json` 做结构 diff（path / operation / schema 名 / 必填 / 枚举） | `scripts/dump_openapi.py` 的思路照搬为 Go 工具 |
| L2 行为 | 进程级 E2E：Go server + Go runner + **真 pi** + mock LLM，跑一组会话录制（创建→发消息→流式→工具调用→策略 ASK→中断→压缩→fork） | `tests/server/integration/mock_llm_server.py`（`sse_text_response` / `sse_tool_call_response` / `anthropic_sse_*`）+ `tests/e2e_ui/conftest.py:configure_mock_llm` |
| L3 前端 | 用现有 617 个前端测试 + Playwright E2E（`tests/e2e_ui/`，33 个 suite）打 Go 后端 | 前端测试已 100% 走 HTTP/SSE，换后端无需改测试 |
| L4 数据 | 拿一份旧 Python 版写出的 SQLite 库，用 Go 版直接读：会话列表、条目渲染、压缩字段、枚举码全等 | 需要 `Uuid16`/`CompressedText`/enum 三件兼容（§3.3） |

**裁判必须在第 1 步就造好**（没有裁判就没有退出条件），推荐顺序：L4（最快见效）→ L1（防漂移）→ L3（最大覆盖面）→ L2（最深）。

---

## 8. 分阶段计划与工作量

| 阶段 | 内容 | 交付判据 | 人周（单人） |
|---|---|---|---|
| **P0 裁判** | 旧库 fixture + Go 读取器；openapi diff 工具；mock LLM 复用 | L4 + L1 绿 | 1.0 |
| **P1 骨架** | oapi-codegen 接入；chi 路由；SQLite/pgx + sqlc；`/v1/info`、`/v1/me`、`/v1/harnesses`、`/v1/sessions` CRUD、`/v1/sessions/{id}/items` | 前端能打开会话列表与历史（只读） | 1.5 |
| **P2 实时** | SSE hub（57 事件 + 序列号 + 心跳 + `[DONE]`）、`WS /v1/sessions/updates`、`POST /events` | 前端能发消息并看到流式（假 agent 回显） | 1.5 |
| **P3 runner + Pi** | runner 二进制、隧道双端、会话初始化、`agent/pi` RPC 实现、EventSink→SSE、工具服务器 + JS extension 模板 | 真 pi 跑通一轮 + 工具调用 + 中断 | 3.0 |
| **P4 策略与资源** | CEL 引擎、`PHASE_REQUEST`/`PHASE_TOOL_CALL`、ASK 卡片 round-trip；终端（PTY/tmux）、文件、工作区 | 策略拦截与批准可见；终端可用 | 2.0 |
| **P5 硬化** | 压缩、fork/resume、usage/成本、presence、错误码一致、并发/取消、OpenTelemetry | 全量 L1–L4 绿 | 2.0 |
| **P6 切换** | 双跑、开关、文档、卸载 Python 侧代码路径 | 默认走 Go，Python 保留一版回退 | 1.0 |
| 合计 | | | **12 周**（含 20% 缓冲） |

**可并行度**：P2 与 P3 可并行（两人）；P4 独立。3 人 ≈ 6–8 周。

---

## 9. 主要风险与对策

| 风险 | 表现 | 对策 |
|---|---|---|
| **SSE 事件漂移** | 前端 reducer 静默丢弃未知事件，表现为「少了一段 UI」而不报错 | openapi diff + 57 个变体的 golden JSON 快照测试；Go 侧用 `json.RawMessage` 保留未知字段 |
| **Pi 轮次边界做错** | 提前结束（用 `agent_end`）→ retry/压缩被截断，前端一直转圈 | 严格实现 `agent_settled` + `stdout_at_eof`；用 mock LLM 造 retry/compaction 场景做回归 |
| **工具桥双重策略评估** | 桥接工具被评估两次 → 弹两次批准 | extension 侧 `BRIDGED` 集合跳过（照抄 2 825 行 JS 的既有逻辑） |
| **旧库读不出来** | 已有数据的用户升级即崩 | P0 先做 L4；`Uuid16`/`CompressedText`/enum 三者逐字节对齐 |
| **多 agent 抽象过度** | V1 只有 Pi，过早抽象导致空转 | 只抽 §4 的 4 个 interface，不抽「工具执行器」「终端」「沙箱」；Pi 未用到的能力返回 `ErrUnsupported` |
| **无 Sandbox/托管能力** | 现有 Python 用户依赖 Modal 等 | V1 明确不支持；`/v1/sandbox-providers` 返回空 catalog，前端已有降级路径 |
| **CGO / 跨平台** | Windows 无 PTY、SQLite 需 CGO | `modernc.org/sqlite`（纯 Go）；PTY 在 Windows 走 ConPTY（`UserExistsError/conpty`），或 V1 声明 Windows 终端功能降级 |
| **工期被低估** | 578 K 行的心理压力导致「全搬」冲动 | 严守 §2 切片；用 `code-migration` skill 的 rulebook + gap inventory 把「哪些不搬」写成规则 |

---

## 10. 下一步（可直接执行）

1. `cp .agents/skills/code-migration/prompts/00-feasibility.md` 的占位符填好，在 `go-backend/` 建空前先跑一遍可行性报告（它会给「是否该迁」的独立判断）。
2. 跑 `prompts/01-dependency-map.md`：用 `scripts/depmap_python.py` 生成 `migration/manifest.tsv`，把 §2 的切片定成有顺序的文件清单。
3. 抄 `templates/RULEBOOK.md` → `migration/RULEBOOK.md`，先把下面几条规定死（这些是「两个 agent 会答不同」的地方）：
   - `dict[str,Any]` 边界 JSON：一律 `json.RawMessage` 直通，不进 struct；
   - 时间：统一 `int64` epoch 秒（不引入 `time.Time` 到线上）；
   - 可空：指针 + `omitempty`（不用 `mo.Option`，前端契约是 `null`）；
   - 错误：`OmnigentError` 的 `code/category/impact/phase` 四元组原样保留为 Go struct；
   - 枚举：Go `int` 常量 + `String()`，线上仍是字符串名（照 Python 的 `enum_codecs` 分层）。
4. 用 `py2go` 的 `references/translation-rules.md` 作为构造级映射表（`async def` → `func`、`asyncio.gather` → `errgroup`、`asyncio.Queue` → `chan T` 等已在表里）。
5. P0 期间就把 `tests/server/integration/mock_llm_server.py` 的响应模板翻译成 Go 测试辅助（或直接以子进程方式复用它——它是 Python，可在 E2E 里共存）。
