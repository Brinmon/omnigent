# R1–R4: 会话创建与管理 — 计划与验收

**这份文档替代对话上下文。压缩后从这里继续。**

状态：计划已定，**尚未开工**。
上一阶段（A 持久化 / B 工具桥 / P4 策略 / C 浏览器验收）已完成，见同目录各 `.md`。

---

## 0. 为什么从这里开始

实测 `capability-probe.py` 的结果：

| 能力 | 状态 |
|---|---|
| 打开已有会话、聊天、流式回复 | ✅ 可用 |
| 历史持久化 | ✅ 可用 |
| 策略拦截工具调用 | ✅ 可用 |
| **新建会话** | ❌ `POST /v1/sessions` → 501 |
| **派 sub-agent** | ❌ 三层都缺 |
| 改标题 / 删除 / fork | ❌ 501 |

**`POST /v1/sessions` 是「能用」的门槛。** 没有它只能聊已有会话，不能开新的。

---

## 1. `POST /v1/sessions` 的真实契约（实测，勿凭猜测）

采集脚本：`capture-create-session.py`
采集结果：`create_session_reference.json`

### 1.1 契约没有护栏

```json
"requestBody": { "content": {} }
"201": { "schema": {} }
```

`openapi.json` 对这条路由的描述是**空的**。`oapi-codegen` 生成的是 `interface{}`，
写错了编译器不拦。**A1–A3 的保护在这里不存在**，只能靠实测对齐（即 A8/A9 存在的原因）。

### 1.2 两条创建路径

| 路径 | Content-Type | 语义 | 实测 |
|---|---|---|---|
| bundle 上传 | `multipart/form-data` | 存 bundle + 建 agent 行 + 建会话行，**一个事务** | 201 → `{session_id, agent_id, agent_name}` |
| 绑定已有 agent | `application/json` | 只建会话行 | 201 → 完整 53 字段快照 |

前端「新建会话」走 **multipart**。

### 1.3 bundle 的两个隐藏约束（实测踩出来的）

| 约束 | 踩坑表现 | 原因 |
|---|---|---|
| 文件名**不能是** `config.yaml` | `config.yaml missing required field: spec_version` | 该名字走严格 `spec_version:1` 解析器；其他名字走 compat 适配器 |
| 必须含 `executor.harness` | — | compat 适配器据此设置 `executor.type: omnigent` |

**正确的最小 bundle**（文件名如 `probe_agent.yaml`）：

```yaml
name: probe_agent
description: Minimal agent.
executor:
  model: deepseek-v4.1-flash
  harness: pi
prompt: |
  You are a minimal agent. Answer briefly.
```

### 1.4 创建时写入 4 张表

```
conversations
  id                      16 字节 uuid
  created_at / updated_at  epoch 秒（两者相同）
  title                    来自 metadata.title，缺省可能为空串
  root_conversation_id     = 自身 id
  next_position            0
  workspace_id             0
  archived                 0
  agent_id                 新建的 agent id
  parent_conversation_id   NULL（顶层会话）

agents
  id                       16 字节 uuid
  created_at               epoch 秒
  name                     来自 yaml 的 name
  bundle_location          "<agent_id>/<sha256 前 64 hex>"
  version                  1
  kind                     2  (AGENT_KIND["user"])
  created_by               "local"
  description              CompressedText（可为 NULL）

session_permissions
  user_id                  "local"
  conversation_id          新会话 id
  level                    4  (LEVEL_OWNER)
  workspace_id             0

omnigent_conversation_metadata
  id                       新会话 id
  kind                     1  (CONVERSATION_KIND["default"])
  workspace_id             0
```

> `bundle_location` 是 artifact store 的键，格式 `<agent_id>/<sha256>`。
> artifact 目录由 `--artifact-location` 指定。

### 1.5 错误行为（实测，A9/A11 的判据）

| 请求 | 状态码 | 错误码 / 信息 |
|---|---|---|
| JSON `{}` | 422 | `detail[].loc = ["agent_id"]`, `msg = "Field required"` |
| JSON `{"title":"x"}` | 422 | 同上（title 不够） |
| JSON `{"agent_id":"000...0"}` | 404 | `{"error":{"code":"not_found","message":"Agent not found: '...'"}}` |
| JSON `{"agent_id":"not-a-uuid"}` | 404 | `{"error":{"code":"not_found","message":"Not found."}}` |
| multipart 用 `config.yaml` | 400 | `invalid_input: config.yaml missing required field: spec_version` |

**注意**：格式错误的 uuid 返回 404 而**不是** 400 —— 与 `GetSession` 的映射一致
（不泄露「格式错」还是「行不存在」）。

---

## 2. 重构计划

### R1 会话创建（解锁「能用」）

| # | 任务 | 说明 | 估时 |
|---|---|---|---|
| R1.1 | artifact store | 写 bundle 到磁盘、算 sha256、生成 `bundle_location` | 0.5d |
| R1.2 | bundle 解析 | 解 tar.gz、取 `.yaml`、按 compat 语义读 `executor.harness` | 0.5d |
| R1.3 | 创建事务 | 4 表写入 + 回滚；`next_position=0`、`root_conversation_id=自身` | 0.5d |
| R1.4 | multipart 路由 | `mime/multipart`：`metadata` part + `bundle` part | 0.5d |
| R1.5 | JSON 路由 | `agent_id` 必填（缺失 422 / 不存在 404） | 0.25d |
| R1.6 | 响应构造 | 3 字段（CreatedSessionResponse）/ 53 字段（SessionResponse） | 0.25d |

**小计 2.5 天。完成后：能新建会话并聊天。**

### R2 会话管理

| # | 任务 | 估时 |
|---|---|---|
| R2.1 | `PATCH /v1/sessions/{id}` — 标题、归档、置顶 | 0.5d |
| R2.2 | `DELETE /v1/sessions/{id}` — 级联删 items/labels/permissions | 0.5d |
| R2.3 | `GET /v1/hosts` — 空列表（无 runner 时的诚实答案） | 0.25d |
| R2.4 | `GET /v1/projects` — 同上 | 0.25d |

**小计 1.5 天。完成后：日常使用完整。**

### R3 Fork

| # | 任务 | 估时 |
|---|---|---|
| R3.1 | `POST /v1/sessions/{id}/fork` — 复制 items，`parent_conversation_id` 指向源 | 1d |
| R3.2 | fork 的 items 位置重排 + `response_id` 重写 | 0.5d |

**小计 1.5 天。完成后：会话树。**

### R4 Sub-agent（另一个量级）

| # | 任务 | 依赖 | 估时 |
|---|---|---|---|
| R4.1 | 工具桥注册 `omnigent_spawn_agent` | 已有桥 | 1d |
| R4.2 | spawn 时创建带 `parent_conversation_id` 的子会话 | R1 | 1d |
| R4.3 | 子会话轮次执行 + 结果回传父会话 | R4.2 | 1.5d |
| R4.4 | 子会话事件转发到父会话流（`session.child_session.updated`） | R4.3 | 1d |
| R4.5 | `/v1/harnesses` 的 `subagents: true` | R4.1 | 0.25d |

**小计 4.75 天。完成后：sub-agent 可派发。**

### 总工期

| 阶段 | 累计 | 完成后能用什么 |
|---|---|---|
| R1 | 2.5d | **新建会话并聊天** |
| R1+R2 | 4.0d | 日常使用完整 |
| +R3 | 5.5d | 会话树（fork） |
| +R4 | **10.25d** | **sub-agent** |

---

## 3. 验收计划

### 三层，逐层加严

#### L1 契约层（秒级，CI 必跑）

| ID | 判据 | 方法 | 现状 |
|---|---|---|---|
| A1–A7 | 已有 | `run-contract.sh` | ✅ 全绿 |
| **A8** | `POST /v1/sessions` 两条路径的响应**字段集**与 Python 一致 | 扩展 `compare-servers.py` | ⬜ 待建 |
| **A9** | 创建后 4 表快照与 Python **逐字段**一致 | 新增 `compare-create.py` | ⬜ 待建 |

**A8/A9 比字段集，不比值** —— 时间戳和 id 每次不同，但字段名、类型、
null 语义必须一致。

#### L2 双后端对拍（分钟级）

| ID | 判据 |
|---|---|
| **A10** | 同一份 bundle，两个后端创建 → 响应字段集相同、DB 行形状相同 |
| **A11** | 同一份非法 body（缺 agent_id / 不存在的 agent_id / 空 bundle）→ 两者状态码与错误码相同 |

#### L3 浏览器验收（真用户流程）

在现有 5 步（`browser-acceptance.py`）基础上扩展：

| ID | 步骤 | 判据 | 依赖 |
|---|---|---|---|
| B1 | boot | SPA 启动 | ✅ 已绿 |
| B2 | sidebar | 列表渲染 | ✅ 已绿 |
| **B6** | **新建会话** | 点「New session」→ 填 prompt → 新会话出现 | R1 |
| **B7** | 新会话聊天 | 发消息 → 流式回复 | R1 |
| **B8** | 新会话刷新 | 刷新后历史仍在 | R1 |
| B3 | 打开已有会话 | | ✅ 已绿 |
| B4 | 发消息 | | ✅ 已绿 |
| E1 | 重命名 | PATCH 生效且 UI 更新 | R2 |
| E2 | 删除 | 会话消失 | R2 |
| **E6** | **fork** | 父子都可见、历史独立 | R3 |

**阶段门槛**：
- R1 完成 → B6/B7/B8 必须绿
- R2 完成 → E1/E2 必须绿
- R3 完成 → E6 必须绿

---

## 4. 执行顺序（用户指定）

```
A8/A9 → R1.1 → R1.2 → R1.3 → R1.4 → R1.5 → R1.6 → B6/B7/B8
```

**每个子项做完立刻验证，不攒到最后。** 理由：`POST /sessions` 的空契约
就是这样暴露的 —— 早写捕获能早发现。

---

## 5. 验收判据的三条铁律

从已有教训总结（B4 reload 断言、fixture error code）：

1. **只断言服务器负责的事。**
   客户端缓存行为（如「刚发的消息刷新后在不在」）如果两个后端都不满足，
   就不是 Go 的判据。例子：`C-BROWSER-ACCEPTANCE.md` 里 reload 断言的修正。

2. **示例数据必须真实。**
   fixture 里不能出现真实构建不会写的值。例子：`fixture_error` 这个 code
   让正确代码看起来是坏的（见 `bisect-items.py` 的结论）。

3. **负向用例同等重要。**
   缺 `agent_id` 必须 422、不存在的 agent 必须 404 —— 只测 happy path 的
   验收会漏掉整个错误处理层。A11 为此存在。

---

## 6. 现有工具（直接复用，不要重写）

| 工具 | 用途 |
|---|---|
| `run-contract.sh` | A1–A7 契约检查，秒级 |
| `compare-servers.py` | 双后端对拍（P1 端点已全绿） |
| `browser-acceptance.py` | 浏览器 5 步验收 |
| `capability-probe.py` | 探测某端点是否实现（501 检测） |
| `bisect-items.py` | 二分定位哪个 item 类型让前端崩溃 |
| `make-reference-db.py` | 重建 fixture（`go/testdata/reference.db`） |
| `probe-pi-rpc.py` | 驱动真实 Pi 抓协议 |
| `capture-sse.py` | 抓 SSE 帧参考 |
| `capture-create-session.py` | 抓创建会话契约 |

## 7. 环境前提（每次都要）

```bash
export PYTHONUTF8=1
export NO_PROXY=127.0.0.1,localhost
unset HTTP_PROXY HTTPS_PROXY ALL_PROXY http_proxy https_proxy all_proxy
export OMNIGENT_PI_MODEL=openai/deepseek-v4.1-flash   # 能被 provider 服务的模型
```

**端口陷阱**：Windows 的动态端口保留区间会占用 6xxxx 段。手工调试用
`18080`–`18199`，或用 `free_port()` 绑定 0。

前端要先构建：`cd web && pnpm exec vite build`（Go server 服务 `web/dist`）。
