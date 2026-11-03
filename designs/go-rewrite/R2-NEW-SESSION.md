# R2: 首页新建会话闭环

**状态：完成。F1–F9 全部通过，首页可手动验收。**

```
F1 host selected               PASS    a host is selected
F2 agent available             PASS    agent: Pi
F3 send enables                PASS    send is enabled
F4 session starts              PASS    session 15cca2a3
F5 prompt shown                PASS    the prompt is visible
F6 reply arrives               PASS    the assistant replied
F7 reload keeps history        PASS    history survived a reload
F8 landing console clean       PASS    no failed requests on the landing page
F9 session page clean          PASS    no 501 on the session page
```

**目标：用户打开首页 → 选 agent → 输入 prompt → 点发送 → 真的建出会话并开始聊天。**

这是**手动验收的门槛**。下面记录开工前的根因分析与最终实现之间的差异。

---

## 0. 为什么现在不能用（实测根因）

截图上的 ④ `No host selected`。追到代码：

```typescript
// NewChatDialog.tsx:2903
if (managedSandboxesEnabled) { setSandboxSelected(true); return; }
const firstOnline = (hosts ?? []).find((h) => h.status === "online");
if (firstOnline) setSelectedHostId(firstOnline.host_id);
```

```typescript
// NewChatDialog.tsx:2452
const noExecutionTargetSelected =
  !sandboxSelected && selectedHostId === null && !executionTargetSelectionPending;
// NewChatDialog.tsx:6382
disabled={noExecutionTargetSelected || workspaceLoading}
```

**执行目标只有两个来源**，Go 版都是空：

| 来源 | 条件 | Go 现状 |
|---|---|---|
| 已连接的 host | `/v1/hosts` 里有一条 `status: "online"` | 返回 `{"hosts": []}` |
| 托管沙箱 | `/v1/info` 的 `managed_sandboxes_enabled: true` | `false` |

所以 `selectedHostId` 永远是 null，按钮永远 disabled。

**注意**：参考 fixture 的 `managed_sandboxes_enabled` 也是 `false`，所以 Python 版
在同样条件下**同样 disabled**。Go 要支持首页建会话，就必须自己提供 host —— 这是
"Go 直接在本机 spawn harness" 这个架构决定的，不是照抄 Python 能解决的。

---

## 1. 实测到的契约（勿凭猜测）

### `/v1/hosts` 的 host 形状

```typescript
// web/src/hooks/useHosts.ts:14
interface Host {
  host_id: string;
  name: string;
  owner: string;
  status: "online" | "offline";
  sandbox_provider?: string | null;
  configured_harnesses?: Record<string, boolean | string> | null;
  harness_accounts?: ... | null;
  harness_accounts_supported?: boolean;
  harness_login_supported?: boolean;
  ...                          // 都是可选，缺省视为 unknown
}
```

数据库表 `hosts` 的列：

```
workspace_id, host_id, user_id, name, status, created_at, updated_at,
token_hash, token_expires_at, sandbox_provider, sandbox_id,
configured_harnesses, terminating_sandbox_id, deleted_at, account_generation
```

### 客户端创建会话时发什么

```typescript
// sessionsApi.ts:502 createSession(agentId, initialItems, options)
body = { agent_id, initial_items: [] }
// 子 agent 路径才带 parent_session_id / sub_agent_name
```

**注意**：首页路径走 `ensureBoundSession` → `createSession(agentId, [])`，
**不发 host_id**（chatStore.ts:4520 注释说明）。host 只是 UI 的前置门槛。

---

## 2. 任务拆解

### R2.1 本机 host（解锁发送按钮）—— 关键

| # | 任务 | 说明 |
|---|---|---|
| a | `GET /v1/hosts` 返回一条本机 host | `host_id` 稳定派生，`status: "online"`，`owner: "local"` |
| b | `configured_harnesses` 报告 pi 可用 | 客户端据此判断能否用该 host 跑 pi |
| c | 首启动写入 `hosts` 表 | 与 Python 的 host_store 同形状，便于互操作 |

**`host_id` 怎么定**：必须**跨进程稳定**（否则重启后客户端记住的选择失效）。
用 `sha256("local:" + 机器标识)[:32]`，与 `builtin_agent_id` 同思路。

### R2.2 内置 agent（解锁 agent 选择器）

截图 ② 是空的 agent 选择器（只显示 "Agent-server"，那是 fixture 的假数据）。

| # | 任务 | 说明 |
|---|---|---|
| a | 打包一个默认 agent | 单文件 YAML，`harness: pi`，随二进制 `go:embed` |
| b | 启动时种进 `agents` 表 | id = `sha256("builtin:"+name)[:32]`（已实现算法） |
| c | `GET /v1/agents` 默认列表返回它 | 现在返回 `kind=server` 的行，种进去就有了 |

**不做完整 seeding**（Python 的 17 个打包 agent）。只种 1 个能跑 pi 的，
够手动验收。其余是 R4 的事。

### R2.3 首页 → 会话的完整链路

| # | 任务 | 说明 |
|---|---|---|
| a | 验证 `initial_items: []` 不报错 | 已实现（去掉 DisallowUnknownFields） |
| b | 创建后客户端能订阅 SSE | 已实现（P2） |
| c | 首条消息落库并渲染 | 已实现（A 持久化，刚修 input_text） |

**这条链路代码已通**，只差 R2.1/R2.2 解锁 UI。

### R2.4 会话页剩余 501（7 个）

打开会话时仍报错的端点：

| 端点 | 处理 |
|---|---|
| `GET /v1/sessions/{id}/agent` | 返回绑定的 agent 详情 |
| `GET /v1/sessions/{id}/resources/terminals` | `{"data": []}`（V1 不做终端） |
| `PUT /v1/sessions/{id}/read-state` | 记录已读，返回 200 |
| `GET /v1/sessions/{id}/resources/environments` | `{"data": []}` |
| `GET /v1/sessions/{id}/child_sessions` | `{"data": []}`（无 sub-agent） |
| `GET /v1/sessions/{id}/resources/github` | `null` 或空对象 |
| `GET /v1/sessions/{id}/pull-requests` | `{"data": []}` |

这些不影响"能否建会话"，但影响 console 干净度和会话页完整度。

---

## 3. 验收计划

### L1 契约层（秒级）

| ID | 判据 | 方法 |
|---|---|---|
| **A12** | `GET /v1/hosts` 形状与 Python 一致（含 host 字段集） | 扩展 `compare-servers.py` |
| **A13** | `GET /v1/agents` 含种入的默认 agent，字段集一致 | 同上 |
| **A14** | host_id 跨进程稳定（两次启动相同） | 新增单测 |

### L2 Go 单测

| ID | 判据 |
|---|---|
| **U1** | `hosts` 表首启动写入，二次启动不重复 |
| **U2** | 默认 agent 种入，id == `sha256("builtin:name")[:32]` |
| **U3** | `/v1/hosts` 输出的 `configured_harnesses` 反映 pi 是否真的可用 |

### L3 浏览器验收（**手动验收的自动化等价物**）

新增步骤，全部在首页完成：

| ID | 步骤 | 判据 |
|---|---|---|
| **F1** | 首页加载无 host 警告 | 不出现 "No host selected" |
| **F2** | 发送按钮在输入后 enable | `disabled == false` |
| **F3** | agent 选择器有可选项 | 至少有 1 个 |
| **F4** | 输入 prompt 点发送 → 跳转到 `/c/<id>` | URL 变化 |
| **F5** | 新会话里看到自己发的消息 | 气泡非空 |
| **F6** | 收到 assistant 回复 | 出现非空回复 |
| **F7** | 刷新后历史仍在 | 两条消息都在 |
| **F8** | 首页 console 零错误 | 无 `error` 级日志 |
| **F9** | 会话页 console 零 501 | 无失败请求 |

**F1–F9 全绿 = 可以手动验收。**

---

## 4. 执行顺序

```
A12（先建对拍）→ R2.1（host，解锁按钮）
              → R2.2（内置 agent）
              → 浏览器 F1–F4（验证解锁）
              → R2.4（7 个 501）
              → 浏览器 F5–F9（完整验收）
```

每步做完立刻验证，不攒到最后。

---

## 5. 已知约束（不要违反）

1. **host 必须由 Go 自己提供。** fixture 里 `managed_sandboxes_enabled: false`，
   Python 版同样 disabled。照抄 Python 的 `/v1/hosts` 空响应永远解不开这个门槛。

2. **host_id 必须跨进程稳定。** 客户端会记住用户选的 host；随机 id 会让重启后的
   记住值失效，表现为"每次都要重新选"。

3. **不要为了过验收去改前端。** 前端一行不改是契约测试的价值所在。如果前端需要
   什么字段，让 Go 提供，而不是改客户端。

4. **空响应也要形状正确。** `/v1/hosts` 是 `{"hosts": []}` 不是 `{"data": []}`；
   已验证过，猜错会让客户端静默失败。


---

## 6. 实现结果与踩到的坑

### 解锁发送按钮需要三样，不是两样

计划里只列了 host 和 agent。实际卡了第三样：**`GET /v1/hosts/{id}/filesystem`**。
workspace 选择器要列目录，501 时 `workspaceLoading` 为真，按钮仍然 disabled。

### `go:embed` 的 schema 不能直接重复执行

见 `eb9673d7f`。Python 导出的 schema 没有 `IF NOT EXISTS`（它靠 migrator 记进度），
每次启动执行会撞"表已存在"。在执行时加保护，不改源文件 —— 它要忠实记录两版共享的形状。

### 客户端靠 create token 认领自己建的会话

**这是最隐蔽的一条。** 首页建会话走 navigate-first：

1. 先导航到乐观 id `/c/temp:<token>`
2. 发 `POST /v1/sessions`，body 里带 `labels.omnigent.client_create_token = <token>`
3. 用这个 token 在会话列表里匹配到真实 id，再 `navigate` 替换

Go 版忽略 `labels`，所以第 3 步永远匹配不上，URL 卡在 `temp:`。修了两处：

- `createSessionForAgent` 把 labels 落库（`insertLabels`）
- `sessionListItemFor` 从 `conversation_labels` 读回来（之前硬编码 `map[string]string{}`）

第二处尤其关键：**存了但读不回来等于没存**。

### 按钮在 Python 上同样 disabled

对照验证过：Python 后端在没有真 runner 连接时，"Start session" 也 disabled
（`managed_sandboxes_enabled: false` + `/v1/hosts` 需要真 websocket 连接）。

**Go 版反而是唯一能用的**，因为它 in-process spawn，`/v1/hosts` 直接报本机 online。

### 一个被我自己误导的排查

`page.url` 在 React Router 导航完成前就返回了值，导致我以为 `temp:` 没被替换。
实际上 URL 一直是对的，是我的采样时机太早。教训：**验证导航要轮询 URL，不能读一次**。

### 新增的端点

| 端点 | 说明 |
|---|---|
| `GET /v1/hosts` | 本机 host，`host_id` = `sha256("local-host:"+hostname)[:16]`，跨进程稳定 |
| `GET /v1/hosts/{id}/filesystem` | 列本机目录，隐藏 dotfiles，目录优先 |
| `GET /v1/hosts/{id}/worktrees` | 空列表（V1 不建 worktree） |
| `GET /v1/skills` | 空列表（无 skill 库） |

### 内置 agent

只种 1 个（`pi`），id = `sha256("builtin:pi")[:32]`，`kind=server`。
Python 种的 17 个不在 V1 范围 —— 机制已就位，加名字即可。

### 新增验收脚本

```bash
python designs/go-rewrite/landing-acceptance.py          # F1–F9
python designs/go-rewrite/landing-acceptance.py --headed # 看着它跑
```
