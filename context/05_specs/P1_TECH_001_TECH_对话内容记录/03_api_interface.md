# 对话内容记录（ConversationLog）— 接口设计文档

## 文档信息

| 项目 | 内容 |
|------|------|
| Feature | P1_TECH_001_TECH_对话内容记录 |
| 模块前缀 | `conversation` |
| 接口协议 | HTTP（简化 GET/POST 模式） |
| Base URL | `/api`（规则文件 §2.1，管理接口前缀） |
| 设计依据 | `01_功能需求规格说明书.md`（SSOT，技术组件能力清单 能力1~能力7） |
| 规则文件 | `AGENTS_DATABASE_API_RULE.md`（项目根目录） |

---

## 基础信息

**模块前缀：** `conversation`

**接口协议：** HTTP（简化的 GET/POST 模式）

**认证方式：** Bearer Token + `middleware.AdminAuth()`，接口挂管理员路由组，仅管理员可访问（规则文件 §2.1）。

**统一响应格式：** 成功经 `common.ApiSuccess(c, data)` 返回 `{success, message, data}`；错误经 `common.ApiError` / `common.ApiErrorI18n` 返回 `{success, message}`。分页数据走 `common.PageInfo`（`page` / `page_size` / `total` / `items`）。

### 核心设计原则

| 方法 | 用途 | 请求体 | 说明 |
|------|------|--------|------|
| GET | 查询数据 | 无 | 会话列表、会话详情 |
| POST | 变更数据 | 有 | 本组件无对外变更接口：对话捕获与写库由 relay 链路上的 middleware 内部完成，不暴露任何 HTTP 变更入口 |

> 本组件是纯查询型技术组件：捕获、解析、会话识别、持久化均发生在 middleware/service 内部，对管理后台只开放两条只读 REST 接口。符合简化 GET/POST 模式中 GET 仅查询的约定。

---

## 接口列表

| 序号 | 方法 | 路径 | 描述 | 需求追溯 |
|------|------|------|------|---------|
| 1 | GET | `/api/conversation/` | 会话列表：按 session_key 聚合分页，仅聚合字段，不含消息内容 | [需求：能力7-管理员查询API、能力6-会话查询] |
| 2 | GET | `/api/conversation/:session_key` | 会话详情：`{session, messages, turns}`；`turns` 为逐轮元数据，完整对话序列统一由 `messages` 返回 | [需求：能力7-管理员查询API、能力6-会话查询] |

### 1.1 会话列表

**接口路径：** `GET /api/conversation/`

**需求追溯：** [需求：能力7-管理员查询API、能力6-会话查询]

**权限：** 仅管理员（`middleware.AdminAuth()`，管理员路由组）

**查询参数：**

| 参数 | 类型 | 必填 | 说明 |
|------|------|------|------|
| token_name | string | 否 | 令牌名称过滤（对齐 logs 查询参数） |
| username | string | 否 | 用户名过滤 |
| model_name | string | 否 | 模型名称过滤 |
| start_timestamp | int64 | 否 | 起始时间戳（Unix 秒级），过滤轮次 `created_at` |
| end_timestamp | int64 | 否 | 结束时间戳（Unix 秒级） |
| p | integer | 否 | 页码，默认 1（`common.GetPageQuery`） |
| page_size | integer | 否 | 每页数量，上限 100（`common.GetPageQuery`） |

**请求示例：**

```
GET /api/conversation/?token_name=my-token&page_size=20&start_timestamp=1781234567&end_timestamp=1781237890
```

**响应示例（成功）：**

```json
{
  "success": true,
  "message": "",
  "data": {
    "page": 1,
    "page_size": 20,
    "total": 1,
    "items": [
      {
        "session_key": "conv_8f3a2b",
        "first_turn_time": 1781234567,
        "last_turn_time": 1781237890,
        "turn_count": 3,
        "token_name": "my-token",
        "username": "user1",
        "user_id": 2,
        "model_name": "gpt-4o"
      }
    ]
  }
}
```

**说明：**
- 以 `session_key` 为聚合键，GROUP BY 每会话一行，聚合首末轮时间与轮数。
- 过滤维度对齐 logs 查询参数（`token_name` / `username` / `model_name` / `start_timestamp` / `end_timestamp`）。
- 时间过滤直接作用于轮次 `created_at`，即时间窗口内有过轮次的会话；首末轮时间与轮数是窗口内轮次的统计，非会话全量统计。
- 列表按末轮时间倒序返回。
- 仅聚合字段（`session_key`、首末轮时间、轮数、`token_name`、`username`、`user_id`、`model_name`），不含消息内容，不回传 `ip` / `channel_id` / `token_id` / `use_time` 等整表维度字段。

### 1.2 会话详情

**接口路径：** `GET /api/conversation/:session_key`

**需求追溯：** [需求：能力7-管理员查询API、能力6-会话查询]

**权限：** 仅管理员（`middleware.AdminAuth()`，管理员路由组）

**路径参数：**

| 参数 | 类型 | 必填 | 说明 |
|------|------|------|------|
| session_key | string | 是 | 会话标识，列表接口返回的 `session_key` |

**查询参数：** 无（详情全量返回，不分页）。

**请求示例：**

```
GET /api/conversation/conv_8f3a2b
```

**响应示例（成功）：**

```json
{
  "success": true,
  "message": "",
  "data": {
    "session": {
      "session_key": "conv_8f3a2b",
      "first_turn_time": 1781234567,
      "last_turn_time": 1781237890,
      "turn_count": 3,
      "token_name": "my-token",
      "username": "user1",
      "user_id": 2,
      "model_name": "gpt-4o"
    },
    "turns": [
      {
        "id": 1,
        "created_at": 1781234567,
        "request_id": "req_1",
        "turn_kind": "first"
      },
      {
        "id": 2,
        "created_at": 1781236220,
        "request_id": "req_2",
        "turn_kind": "normal"
      },
      {
        "id": 3,
        "created_at": 1781237890,
        "request_id": "req_3",
        "turn_kind": "tool_round"
      }
    ],
    "messages": [
      { "role": "user", "kind": "text", "text": "你好" },
      { "role": "assistant", "kind": "text", "text": "你好，有什么可以帮你？" },
      { "role": "user", "kind": "text", "text": "介绍一下你自己" },
      { "role": "assistant", "kind": "text", "text": "我是基于 GPT 的 AI 助手。" },
      { "role": "user", "kind": "text", "text": "帮我查天气" },
      { "role": "assistant", "kind": "tool_use", "text": "get_weather({\"city\":\"北京\"})" },
      { "role": "tool", "kind": "tool_result", "text": "北京晴，25 度" },
      { "role": "assistant", "kind": "text", "text": "北京今天晴天，气温 25 度。" }
    ]
  }
}
```

**说明：**
- `session`：聚合元数据（`token_name`、`username`、`user_id`、`model_name`、首末轮时间、轮数）。
- `turns`：逐轮元数据（`id` / `created_at` / `request_id` / `turn_kind`），全量返回、不分页，不含每轮消息内容，避免与汇聚的 `messages` 重复。
- `messages`：当前返回 turns 内按 `created_at, request_id` 升序逐行 append 得到的完整 `[]MsgPart` 序列，即完整多轮对话，是详情响应的唯一消息内容来源；全量返回、不分页。
- 不回传整表维度字段（`ip` / `channel_id` / `token_id` / `use_time` 等不进入详情响应）。
- `turn_kind` 枚举取值：`first`（新建会话首轮）、`normal`（常规轮）、`tool_round`（含工具调用，优先级最高）。

---

## 错误码规范

本项目不采用数字错误码体系，错误通过 HTTP 语义 + `success=false` + `message` 表达（规则文件 §2.4）：

| 场景 | HTTP 状态码 | 响应体 | 说明 |
|------|------------|--------|------|
| 参数非法 | 400 | `{"success": false, "message": "..."}` | 分页越界（page_size 超上限）、时间范围倒置（start_timestamp > end_timestamp）等 |
| 权限不足 | 403 | `{"success": false, "message": "..."}` | 非管理员访问管理接口，`AdminAuth` 拦截 |
| 记录不存在 | 404 | `{"success": false, "message": "..."}` | 会话详情查询的 `session_key` 无记录 |
| 内部错误 | 500 | `{"success": false, "message": "..."}` | 服务端异常，错误信息经 go-i18n 国际化 |

**错误响应示例（404）：**

```json
{
  "success": false,
  "message": "会话不存在"
}
```

---

## 安全说明

### 认证与授权
- 两条查询接口均挂在管理员路由组，`middleware.AdminAuth()` 校验 Bearer Token，仅管理员可访问（规则文件 §2.1、§3.4 需求规格）。
- 不开放给普通用户，不暴露 `/api/conversation/` 到非管理员路径。

### 敏感数据保护
- token 明文 key 不落库、不回传，仅按 `token_name` 查询（与 logs 表惯例一致）。
- 会话详情不回传 `ip` / `channel_id` / `token_id` / `use_time` 等整表维度字段，避免泄露无关调用信息。

### 传输安全
- 生产环境经 HTTPS（TLS 由部署层终止，`ConfigureTrustedProxies` 处理可信代理）。

### 输入验证
- 查询参数类型与范围校验（时间戳、分页参数），越界返回 400。
- 列表与详情查询均只读，无 SQL 注入写入面；过滤走 GORM 参数化查询。

---

## 限流策略

本组件查询接口不新增独立限流策略。管理接口沿用项目既有的认证、CORS 与全局中间件，不设置接口级/用户级额外限流。查询针对 ClickHouse 日志库，沿用 logs 查询的慢查询与资源控制约定（`SQL_SLOW_THRESHOLD_MS` 告警）。

---

## 需求追溯总表

| 需求规格能力 | 覆盖接口 | 覆盖程度 |
|-------------|---------|---------|
| 能力6：会话查询（ListConversations / GetConversationTurns / MergeConversation） | GET `/api/conversation/`、GET `/api/conversation/:session_key` | 完整覆盖 |
| 能力7：管理员查询 API（ListConversations / GetConversation） | GET `/api/conversation/`、GET `/api/conversation/:session_key` | 完整覆盖 |
| 能力1~能力5（捕获/解析/会话识别/持久化） | 无 HTTP 接口 | 组件内部能力，middleware/service/model 实现，不对外暴露 |
