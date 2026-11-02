# Go 重构 — 索引

**从这里开始。上下文压缩后，先读这份，再读被指向的那份。**

## 当前状态（一行）

Go 版能打开已有会话聊天；**不能新建会话**。下一步是 R1（会话创建），
见 [R-PLAN-SESSIONS.md](./R-PLAN-SESSIONS.md)。

## 立刻要做的

```
A8/A9 → R1.1 → R1.2 → R1.3 → R1.4 → R1.5 → R1.6 → B6/B7/B8
```

计划和验收全在 **[R-PLAN-SESSIONS.md](./R-PLAN-SESSIONS.md)**，含：
- 实测到的 `POST /v1/sessions` 契约（含 4 张表的写入字段）
- bundle 两个隐藏约束（文件名不能是 `config.yaml`）
- 错误行为表（422 / 404 的精确判据）
- R1–R4 任务拆解与工期
- A8–A11 + B6–B8 + E6 验收判据

## 文档地图

### 计划与架构（先读）
| 文件 | 内容 |
|---|---|
| **[R-PLAN-SESSIONS.md](./R-PLAN-SESSIONS.md)** | **当前计划 + 验收，从这里继续** |
| [01-current-architecture.md](./01-current-architecture.md) | Python 版架构梳理（211 行） |
| [02-go-refactor-plan.md](./02-go-refactor-plan.md) | Go 重构总体方案 + 库选型 |
| [03-acceptance-plan.md](./03-acceptance-plan.md) | 验收方法论（三层 × 32 条 ID） |
| [ACCEPTANCE-TRACKER.md](./ACCEPTANCE-TRACKER.md) | 验收追踪表（已勾项见下） |

### 已完成阶段的记录（读对应那份就知道做了什么、踩了什么坑）
| 文件 | 阶段 | 关键结论 |
|---|---|---|
| [BASELINE-REPORT.md](./BASELINE-REPORT.md) | 基线 | 16 PASS / 4 FAIL / 1 SKIP；修了 7 个 Windows 可移植性缺陷 |
| [P0-CONTRACT.md](./P0-CONTRACT.md) | 契约层 | A1–A7 全绿；`openapi-diff` 带负向测试 |
| [P2-EVENTS.md](./P2-EVENTS.md) | 实时层 | SSE + WS；`sequence_number` 为 null 是对的 |
| [P3-PI-HARNESS.md](./P3-PI-HARNESS.md) | Pi 接入 | `agent_end` **不是**轮次边界，`agent_settled` 才是 |
| [A-PERSISTENCE.md](./A-PERSISTENCE.md) | 持久化 | sqlc 重复参数坑；中断要 abort 不能 kill |
| [B-TOOL-BRIDGE.md](./B-TOOL-BRIDGE.md) | 工具桥 | 桥接工具跳过 hook（防双弹窗） |
| [P4-POLICY.md](./P4-POLICY.md) | 策略引擎 | DENY > ASK > ALLOW；求值失败 fail closed |
| [C-BROWSER-ACCEPTANCE.md](./C-BROWSER-ACCEPTANCE.md) | 浏览器验收 | 5 步全绿；3 个 API 测试抓不到的缺陷 |

### 抓出来的真实参考数据（`*.json`，实现时的对照）
| 文件 | 内容 |
|---|---|
| `create_session_reference.json` | **创建会话的响应 + 4 表快照**（R1 要照这个写） |
| `pi_rpc_reference.json` | 一轮 Pi RPC 的完整事件流（15 个事件） |
| `pi_tool_events.json` | 工具调用的真实事件形状 |
| `sse_reference.json` | SSE 帧 + header |
| `payload_wire.json` | 每种 item 类型的线上字段集 |
| `go/testdata/http_golden.json` | `/v1/info` `/v1/me` `/v1/harnesses` `/v1/sessions` 的真实响应 |

### 工具（直接复用，别重写）
| 脚本 | 用途 |
|---|---|
| `run-contract.sh` | A1–A7，秒级，CI 用 |
| `compare-servers.py` | 双后端对拍 |
| `browser-acceptance.py` | 浏览器 5 步验收 |
| `capability-probe.py` | 探测哪些端点还是 501 |
| `bisect-items.py` | 二分定位哪个 item 让前端崩 |
| `capture-create-session.py` | 重新抓创建会话契约 |
| `make-reference-db.py` | 重建 fixture |
| `probe-pi-rpc.py` / `capture-sse.py` | 抓 Pi / SSE 协议 |

## 代码在哪

```
go/                        独立 module（github.com/Brinmon/omnigent/go）
├── cmd/omnigent/          统一入口：serve 提供 API + web/dist
├── cmd/openapi-diff/      A1–A3 契约比对（带 9 个负向测试）
├── internal/api/          oapi-codegen 生成（194 schema / 154 operation）
├── internal/db/           Uuid16 / 枚举码 / CompressedText（与 Python 逐字节兼容）
├── internal/event/        56 个 SSE 事件类型
├── internal/pi/           `pi --mode rpc` 客户端
├── internal/policy/       CEL 引擎
├── internal/server/       HTTP 层（含 SPA 静态服务）
├── internal/store/        sqlc 生成的查询
├── internal/stream/       会话事件 fan-out
├── internal/toolbridge/   loopback 工具端点 + 生成的 Pi extension
└── testdata/              reference.db（Python 写的库，A5/A6 的 oracle）
```

## 环境前提（每次都要）

```bash
export PYTHONUTF8=1
export NO_PROXY=127.0.0.1,localhost
unset HTTP_PROXY HTTPS_PROXY ALL_PROXY http_proxy https_proxy all_proxy
export OMNIGENT_PI_MODEL=openai/deepseek-v4.1-flash
```

前端要构建一次：`cd web && pnpm exec vite build`

**端口陷阱**：Windows 动态端口保留区间会占 6xxxx 段。手工调试用 `18080`–`18199`。

## 验收现状

| 组 | 状态 |
|---|---|
| A1–A7 契约 | ✅ 全绿 |
| B1/B2/B3/B4 启动与聊天 | ✅ |
| C1/C2/C4 Pi 与工具桥 | ✅ |
| D1/D2 策略 | 🟡 DENY 生效，ASK 卡片待做 |
| E3 刷新去重 | ✅ |
| **B6/B7/B8 新建会话** | ⬜ **下一步** |
| E1/E2/E6 管理/fork | ⬜ R2/R3 |

## 已知缺口（诚实清单）

- **无 runner 进程** — server 直接 spawn Pi。session 不能跨 server 存活。
- **`ASK` 只是「带理由的阻断」** — Pi 的 `tool_call` 钩子无法暂停等人工。
- **无 sub-agent** — 三层都缺：能力声明 false、工具桥无 spawn 工具、无子会话创建。
- **`POST/PATCH/DELETE /v1/sessions`、fork、hosts、projects 全是 501** — R1–R3 的内容。
- **浏览器验收只有 5 步** — B1–E5 全矩阵还需逐条做。
