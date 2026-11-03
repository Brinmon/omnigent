# R1: 会话创建

**状态：完成。** `POST /v1/sessions` 两条路径与 Python 逐字段对齐，浏览器验收 8 步全绿。

## 验收结果

```
A8   multipart create -> HTTP response          OK   3 field(s) agree
A9   agents: 10 column(s) agree                 OK
     conversations: 11 column(s) agree          OK
     omnigent_conversation_metadata: 19 agree   OK
     session_permissions: 1 agree               OK
A8b  json create -> 53 field(s) agree           OK
A11  missing agent_id    422/validation:agent_id  OK
     unknown agent_id    404/not_found            OK
     malformed agent_id  404/not_found            OK
```

```
boot                     PASS    Omnigent
sidebar                  PASS    session row rendered
open-session             PASS    opened 8355b792
send-message             PASS    assistant reply rendered
history-after-reload     PASS    stored transcript rendered after reload
new-session              PASS    created df1c1e42
new-session-listed       PASS    created session is listed in the sidebar
new-session-opens        PASS    opened df1c1e42
```

## 实现

| 组件 | 位置 |
|---|---|
| blob 存储 | `go/internal/artifact/store.go` — 原子写、拒绝路径逃逸 |
| bundle 解析 | `go/internal/bundle/parse.go` — tar.gz → Spec（含 skills 前端解析） |
| 创建事务 | `go/internal/server/session_create.go` — 4 表一次提交 |
| HTTP 路由 | `go/internal/server/session_create_route.go` — multipart + JSON 分派 |
| agent 列表 | `go/internal/server/agents.go` — `GET /v1/agents` |

**契约没有护栏**：`openapi.json` 的 `requestBody.content` 为空、201 schema 是 `{}`，`oapi-codegen` 给 `interface{}`。所以 `compare-create.py` 是唯一防线，它先于实现写出来。

## 验收网抓出的 4 个真实缺陷

这些都不是"实现完了顺手修"，而是对拍报出来的。

### 1. `metadata` 是表单字段，不是文件

我用了 `r.FormFile("metadata")`，对客户端正确发送的请求回 422。Python 的 `form.get("metadata")` 是普通字符串。

```go
metadataRaw := r.FormValue("metadata")   // 不是 FormFile
```

### 2. 422 的 `loc` 不该带 `body` 前缀

Python 这条路由拿裸 `Request` 自己解析，所以报 `["agent_id"]`；FastAPI 的默认是 `["body","agent_id"]`。加上前缀会让同一个失败在两个后端显示不同。

### 3. `agents.description` 应为 NULL

Python 的创建不写 description 列，即使 YAML 里有。我写了，A9 立刻报 `'bytes[50]' vs 'NULL'`。

### 4. `DisallowUnknownFields` 会打断真实客户端

客户端发 `{agent_id, initial_items: []}`。我加了 `DisallowUnknownFields`，真实请求会被拒。**这条只有在浏览器里跑才会暴露** —— 我的对拍只发了最小 body。

## 浏览器暴露的两个 UI 门槛（都不是 Go 缺陷）

### "Start session" 按钮永远 disabled

追到 `disabled={noExecutionTargetSelected || workspaceLoading}`，而 `noExecutionTargetSelected` 需要 host 或 managed sandbox。

**用 Python 后端对照，按钮同样 disabled** —— `managed_sandboxes_enabled: false` + `/v1/hosts: {"hosts": []}` 时客户端就是不让点。

所以 B6/B7/B8 改成：create 走 HTTP 路由（客户端用的同一条），浏览器验证**用户能看到的那半**（列表出现、能打开）。

### `keyboard.type` 不触发 React 状态

按钮 disabled 的真实原因之一。`fill()` 才派发 `input` 事件。

## fixture 的两处修正

### `created_by` 必须是 `local`

fixture 里 user agent 的 `created_by` 是 `fixture-user`，而单用户服务身份是 `local`，所以 `GET /v1/agents?scope=user` 返回空 —— **新建会话无 agent 可选**。改成 `local`。

### `scope=user` 不是 `kind=user`

Python 的 `list_user_agents` 是 `kind=user AND created_by=owner`。只看 kind 会在共享 workspace 里列出别人的 agent。

## 已知缺口（不是 R1，但影响完整度）

**内置 agent seeding 没做。** Python 启动时把 17 个打包 agent 种进数据库（id = `sha256("builtin:<name>")[:32]`），`/v1/agents` 默认列这些。Go 版只有 fixture 里那 1 条。

影响：客户端能选到 agent、能建会话，但**看不到内置的 jcode / debby / polly 等**。

`builtin` 字段的算法已按 Python 实现（`id == sha256("builtin:"+name)[:32]`），所以 seeding 一旦补上，这个字段自动正确。

## 工具

```bash
python designs/go-rewrite/compare-create.py        # A8/A9/A11/A8b
python designs/go-rewrite/browser-acceptance.py    # 8 步浏览器验收
python designs/go-rewrite/capability-probe.py      # 哪些端点还是 501
python designs/go-rewrite/probe-start-button.py    # 按钮为什么 disabled
```

## 剩余 501

```
PATCH  /v1/sessions/{id}        R2
DELETE /v1/sessions/{id}        R2
POST   /v1/sessions/{id}/fork   R3
GET    /v1/hosts                R2（返回 {"hosts": []} 即可）
GET    /v1/projects             R2
GET    /v1/sessions/projects    R2（返回 []）
GET    /v1/projects/order       R2（返回 {sort_mode, ordered_project_ids}）
GET    /v1/extensions           R2（返回 {object, data: []}）
POST   /v1/agents               R2（安装 agent bundle）
```
