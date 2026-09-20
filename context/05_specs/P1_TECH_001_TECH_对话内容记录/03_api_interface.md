# 对话内容记录查询 API

供外部平台经集成密钥拉取 new-api 网关记录的多轮对话原文。

## 接入

**Base URL：** `http(s)://<部署地址>/api`

**认证：** 每个请求携带请求头 `Authorization: Bearer <集成密钥>`。集成密钥由部署方通过环境变量 `CONVERSATION_LOG_INTEGRATION_KEY` 配置后提供给你。

**调用流程：** 先调会话列表拿到 `session_key`，再用 `session_key` 调会话详情拿到完整对话内容。

## 通用响应

成功：

```json
{ "success": true, "message": "", "data": ... }
```

业务错误（HTTP 200）：

```json
{ "success": false, "message": "错误描述" }
```

认证错误（HTTP 401/403）：

```json
{ "success": false, "code": "...", "message": "..." }
```

分页响应的 `data` 为 `{ page, page_size, total, items }`。

## 错误码

| HTTP | code | 场景 |
|------|------|------|
| 403 | `CONVERSATION_LOG_KEY_NOT_CONFIGURED` | 服务端未配置集成密钥 |
| 401 | `CONVERSATION_LOG_INVALID_KEY` | 未带密钥或密钥错误 |
| 400 | — | 参数非法（分页越界、时间范围倒置等） |
| 200 | — | 业务错误（记录不存在 / 内部错误），`success=false` |

## 接口

### 1. 会话列表

`GET /api/conversation-log/`

按 `session_key` 聚合分页返回会话，仅含聚合字段，不含对话内容。

**查询参数：**

| 参数 | 类型 | 必填 | 说明 |
|------|------|------|------|
| token_name | string | 否 | 令牌名称过滤 |
| username | string | 否 | 用户名过滤 |
| model_name | string | 否 | 模型名称过滤 |
| start_timestamp | int64 | 否 | 起始时间戳（Unix 秒） |
| end_timestamp | int64 | 否 | 结束时间戳（Unix 秒） |
| p | integer | 否 | 页码，默认 1 |
| page_size | integer | 否 | 每页数量，上限 100 |

**请求示例：**

```bash
curl -H "Authorization: Bearer <集成密钥>" \
  "http://<部署地址>/api/conversation-log/?page_size=20&start_timestamp=1781234567&end_timestamp=1781237890"
```

**响应示例：**

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

**items 字段：**

| 字段 | 说明 |
|------|------|
| session_key | 会话标识，用于调详情 |
| first_turn_time / last_turn_time | 首末轮时间戳（Unix 秒） |
| turn_count | 时间窗口内的轮次数 |
| token_name / username / user_id | 调用方信息 |
| model_name | 模型 |

时间过滤作用于轮次时间，返回窗口内有过轮次的会话；首末轮时间与轮数为窗口内统计，非会话全量。结果按末轮时间倒序。

### 2. 会话详情

`GET /api/conversation-log/:session_key`

返回会话的完整对话内容，全量返回、不分页。

**路径参数：**

| 参数 | 说明 |
|------|------|
| session_key | 会话标识，取自列表接口 |

**请求示例：**

```bash
curl -H "Authorization: Bearer <集成密钥>" \
  "http://<部署地址>/api/conversation-log/conv_8f3a2b"
```

**响应示例：**

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
      { "id": 1, "created_at": 1781234567, "request_id": "req_1", "turn_kind": "first" },
      { "id": 2, "created_at": 1781236220, "request_id": "req_2", "turn_kind": "normal" },
      { "id": 3, "created_at": 1781237890, "request_id": "req_3", "turn_kind": "tool_round" }
    ],
    "messages": [
      { "role": "user", "kind": "text", "text": "你好" },
      { "role": "assistant", "kind": "text", "text": "你好，有什么可以帮你？" },
      { "role": "user", "kind": "text", "text": "帮我查天气" },
      { "role": "assistant", "kind": "tool_use", "text": "get_weather args=17" },
      { "role": "tool", "kind": "tool_result", "text": "tool_result result=18" },
      { "role": "assistant", "kind": "text", "text": "北京今天晴天，气温 25 度。" }
    ]
  }
}
```

**data 字段：**

| 字段 | 说明 |
|------|------|
| session | 会话聚合元数据，字段同列表 items |
| turns | 逐轮元数据（id / created_at / request_id / turn_kind），全量不分页 |
| messages | 完整对话序列，按时间升序，全量不分页 |

**messages 字段：**

| 字段 | 说明 |
|------|------|
| role | `user` / `assistant` / `tool` / `system` |
| kind | `text` / `tool_use` / `tool_result` |
| text | 消息内容；`text` 存原文，`tool_use` 存「工具名 args=参数字节数」元信息，`tool_result` 存「工具名 result=结果字节数」元信息（OpenAI/Claude 协议无工具名字段，标签退化为 `tool_result`；Gemini `functionResponse` 带工具名，保留原名）。字节数为 UTF-8 字节长度，工具原始载荷不落库。注意：args 字节数为请求侧与响应侧各自序列化的原始字节数，同一调用走流式与非流式路径记出的数值可能不同（map 键序等因素），属可接受的口径偏差，不做归一化 |

**turn_kind 枚举：** `first`（新建会话首轮）、`normal`（常规轮）、`tool_round`（含工具调用）。
