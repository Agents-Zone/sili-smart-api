# 用户信息字典查询 API（变更：用户信息接口认证拓展）

供外部 AI 分析平台经集成密钥拉取 new-api 网关的用户名与 API key 名称目录，用于解析、展示和映射会话列表/详情中携带的 `user_id`、`username`、`token_name` 维度。

## 文档信息

| 项目 | 内容 |
|------|------|
| Feature | P1_TECH_002_RFC_用户信息接口 |
| 基线 Feature | P1_TECH_001_TECH_对话内容记录 |
| 模块前缀 | `conversation` |
| 设计依据 | `01_功能需求规格说明书.md`（SSOT，§2.2 输入定义、§2.3 输出定义、§2.4 能力8、§3.2 安全要求） |
| 规则文件 | `AGENTS_DATABASE_API_RULE.md`（项目根目录） |
| 变更范围 | 在 `/api/conversation-log` 集成密钥套件中新增「用户信息字典」查询接口，认证、响应格式、错误码与现有会话列表/详情一致；`/api/user/self` 登录态认证保留不动 |

---

## 接入

**Base URL：** `http(s)://<部署地址>/api`

**认证：** 每个请求携带请求头 `Authorization: Bearer <集成密钥>`。集成密钥由部署方通过环境变量 `CONVERSATION_LOG_INTEGRATION_KEY` 配置后提供，与会话列表/详情接口共用同一把密钥。

**调用关系：** 本接口独立于会话列表/详情，可单独调用。典型用法是外部平台先用本接口拿到用户名与 API key 名称目录，再解析会话列表/详情中的 `user_id`/`username`/`token_name` 维度。

---

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

---

## 错误码

> 业务错误统一返回 HTTP 200 + `success:false`（与项目 common 层、现有会话列表/详情接口一致）；鉴权错误由 `ConversationLogAuth` 中间件返回 HTTP 401/403。

| HTTP | code | 场景 |
|------|------|------|
| 403 | `CONVERSATION_LOG_KEY_NOT_CONFIGURED` | 服务端未配置集成密钥（中间件返回） |
| 401 | `CONVERSATION_LOG_INVALID_KEY` | 未带密钥或密钥错误（中间件返回） |
| 200 | — | 参数非法（`p` 为负、`page_size` 非数字等由 `common.GetPageQuery` 兜底容错，通常不触发；触发时 `success=false`） |
| 200 | — | 数据库内部错误，`success=false`，`message` 为 `MsgDatabaseError`（经 go-i18n 国际化） |
| 200 | — | 查询成功但无满足条件用户，`success=true`，返回空 `items`（`total=0`） |

错误响应与会话列表/详情接口一致：未配置密钥 403、密钥缺失或错误 401（复用 `middleware.ConversationLogAuth()`，由中间件 `c.AbortWithStatusJSON` 返回）；查询参数非法、数据库错误均为 HTTP 200 + `success:false`（与 common 层统一行为一致）；无满足条件结果仍为 200 + `success:true` 与空 `items`。

---

## 接口

### 1. 用户信息字典

`GET /api/conversation-log/users`

按用户聚合分页返回启用状态用户的 `username` 与名下启用 token 的 `token_name`，不含 key 明文与配额、邮箱等敏感字段。数据来源为主库 `users`/`tokens` 表（只读），与 ClickHouse 日志库无关。

**查询参数：**

| 参数 | 类型 | 必填 | 默认值 | 说明 |
|------|------|------|--------|------|
| `username` | string | 否 | — | 用户名包含匹配（`users.username LIKE '%x%'`，大小写按各数据库默认排序规则） |
| `token_name` | string | 否 | — | API key 名称包含匹配（`tokens.name LIKE '%x%'`），作用于用户名下的 token 维度 |
| `p` | integer | 否 | 1 | 页码，沿用 `common.GetPageQuery` |
| `page_size` | integer | 否 | 项目默认 | 每页用户数量，上限 100，越界按上限截断不报错 |

**请求示例：**

```bash
# 全量用户与 API key 名称字典（分页）
curl -H "Authorization: Bearer <集成密钥>" \
  "http://<部署地址>/api/conversation-log/users?page_size=100"

# 按用户名过滤
curl -H "Authorization: Bearer <集成密钥>" \
  "http://<部署地址>/api/conversation-log/users?username=user&page_size=20"

# 按 API key 名称过滤（定位某个 key 的归属用户）
curl -H "Authorization: Bearer <集成密钥>" \
  "http://<部署地址>/api/conversation-log/users?token_name=prod"
```

**响应示例：**

```json
{
  "success": true,
  "message": "",
  "data": {
    "page": 1,
    "page_size": 20,
    "total": 2,
    "items": [
      {
        "user_id": 2,
        "username": "user1",
        "tokens": [
          { "token_id": 10, "token_name": "my-token" },
          { "token_id": 11, "token_name": "prod-key" }
        ]
      },
      {
        "user_id": 3,
        "username": "user2",
        "tokens": [
          { "token_id": 12, "token_name": "test" }
        ]
      }
    ]
  }
}
```

**items 字段：**

| 字段 | 说明 |
|------|------|
| user_id | 用户 ID |
| username | 用户名 |
| tokens | 名下启用 token 数组；名下无启用 token 时为空数组 |

**tokens 字段：**

| 字段 | 说明 |
|------|------|
| token_id | API key（令牌）ID |
| token_name | API key 名称；key 明文一律不进入响应 |

`items` 按 `user_id` 升序分页；每个用户的 `tokens` 按 `token_id` 升序。

**过滤语义：**

- `username` 过滤用户：仅返回 `username` 模糊匹配的启用用户。
- `token_name` 只作用于用户名下的 token 维度：未指定时返回该用户全部启用 token；指定时只返回名下匹配的 token，并仅保留至少含一个匹配 token 的用户。
- LEFT JOIN 语义：只要 `users.status=1` 即返回该用户，名下无启用 token 时 `tokens` 为空数组（便于外部平台解析历史对话中残留的 `user_id`/`username` 维度）。

**注意事项：**

- 数据来源为主库 `users`/`tokens` 表（GORM 只读查询，三库兼容），非 ClickHouse 日志库；不读写 `conversation_turns`，与基线「存储仅 ClickHouse」约束不冲突（该约束针对 `conversation_turns` 的写入存储）。
- 仅返回启用状态：`users.status=1`、`tokens.status=1`，软删除记录由 GORM 自动过滤。
- 字段最小化：响应仅含 `user_id`/`username` 与 `token_id`/`token_name`，敏感字段（key 明文、密码、邮箱、AccessToken、配额用量等）一律不进入响应（SSOT §3.2）。
- 路由与 `/:session_key` 同层共存：Gin v1.9.1 静态路由优先匹配，`/users` 命中静态段，其余路径命中 `:session_key`；`session_key` 由 `common.NewRequestId()` 生成，值域不含裸字符串 `users`，无碰撞风险（项目 `adminRoute` 下 `/search` 与 `/:id` 共存已是先例）。
- 名称可能随用户/令牌编辑变更，不做缓存，每次查主库；查询开销与用户/token 规模成正比，外部平台按需低频调用。
