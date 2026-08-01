# AGENTS_DATABASE_API_RULE.md：new-api 项目数据库与 API 接口设计规则

本文件是本项目数据库与接口设计的项目级规则来源，优先级高于技能内置默认值。基于项目实际代码约定（GORM v2、双库分离、ClickHouse 日志库）编写，所有新增表与接口设计必须遵守本文件。

---

## 一、数据库规则

### 1.1 技术选型

| 配置项 | 值 | 说明 |
|-------|------|------|
| ORM | GORM v2 | 模型定义与数据库访问统一走 GORM |
| 主数据库 | SQLite / MySQL >= 5.7.8 / PostgreSQL >= 9.6 | 按 `SQL_DSN` 选择，默认 SQLite；承载业务数据 |
| 日志数据库 | ClickHouse（可选，`LOG_SQL_DSN`） | 承载 `logs`、`conversation_turns` 等日志数据，物理隔离于主库 |
| 字符集 | utf8mb4 | MySQL 主库 |
| 方言分支 | `common.UsingMainDatabase` / `common.UsingLogDatabase` | 按数据库类型显式分支，日志库类型独立于主库维护 |
| JSON 序列化 | 经 `common.Marshal` / `common.Unmarshal` | 业务代码禁止直接导入 `encoding/json` |

### 1.2 主键策略

| 配置项 | 值 | 说明 |
|-------|------|------|
| 主键类型 | BIGINT（GORM `int64`/`int`） | 主数据库表 |
| 生成方式 | GORM 自动生成 | 禁止手工 `AUTO_INCREMENT` / `SERIAL` DDL |
| ClickHouse 日志表 | `id Int64 DEFAULT 0` | ClickHouse 无自增，展示用 id 由查询侧按序回填，业务唯一标识用 `request_id` |

### 1.3 公共字段

| 字段名 | 类型 | 必填 | 说明 |
|-------|------|------|------|
| created_at | int64 | 是 | 创建时间，Unix 秒级时间戳（`common.GetTimestamp()`） |
| updated_at | int64 | 否 | 更新时间，仅主库业务表需要；日志库表不设 |

注意：本项目不使用 `deleted`（逻辑删除）、`creator`、`updater`、`tenant_id` 等通用审计字段。审计/历史类语义按各模块既有约定实现（如 logs 表的 `Other.admin_info` 嵌套）。

### 1.4 字段命名规范

| 用途 | 规范命名 | 禁止使用 |
|------|---------|---------|
| 创建时间 | created_at | created_time, createTime |
| 更新时间 | updated_at | updated_time, updateTime |
| 会话标识 | session_key | sessionKey |
| 请求标识 | request_id | requestId |
| 分组 | group | 保留字，列名按 `logGroupCol` 方言约定处理 |

- 全部字段名使用 snake_case（下划线分隔）。
- 数据库保留字不得直接作列名；`group` 列统一经 `logGroupCol` 方言约定（PostgreSQL 双引号，其余反引号）。

### 1.5 字段类型约束

| 约束项 | 值 | 说明 |
|-------|------|------|
| 禁止 JSON 类型 | 是 | 结构化内容用 String/TEXT 列存序列化 JSON（如 `messages`），经 `common.Marshal`/`common.Unmarshal` 读写 |
| 禁止外键约束 | 是 | 禁止 `FOREIGN KEY` / `REFERENCES`，表关联仅通过业务字段（`request_id`、`token_id`、`user_id` 等）实现 |
| 布尔字段 | ClickHouse 用 `UInt8`，GORM 用 `bool` | 无布尔默认值 tag（避免 MySQL/PostgreSQL 布尔默认值规范化差异触发反复 ALTER） |
| 枚举字段 | 整数或短字符串，代码内定义常量 | 见 1.8 字典数据设计 |

### 1.6 VARCHAR 长度标准

| 字段类型 | 长度 | 典型用途 | 示例 |
|---------|------|---------|------|
| 短标识 | VARCHAR(64) / String | request_id、session_key | `request_id` |
| 常规文本 | VARCHAR(128) / String | token_name、username、model_name、group、ip | `token_name` |
| 长标识 | VARCHAR(128) / String | upstream_request_id | `upstream_request_id` |
| 文本域 | TEXT / String | 消息序列、长描述 | `messages` |

字段长度优先取需求规格说明书校验规则中明确给定的长度；未明确时使用上表标准。

### 1.7 日期时间字段类型

统一使用 **int64 Unix 秒级时间戳**（`common.GetTimestamp()`），禁止使用 DATETIME / DATE / TIME / TIMESTAMP 类型。

| 场景 | 类型 | 说明 |
|------|------|------|
| created_at | int64 | Unix 秒级时间戳 |
| updated_at | int64 | Unix 秒级时间戳（主库业务表） |

### 1.8 字典数据设计

| 枚举字段 | 取值 | 说明 |
|---------|------|------|
| turn_kind | first / normal / tool_round | 轮次类型，代码常量定义 |

涉及枚举的字段在字段说明中以「枚举：`turn_kind`（first/normal/tool_round）」格式标注，不生成字典 INSERT 语句。

### 1.9 表命名规范

- 表名全小写，使用下划线分隔。
- 业务表名直接使用实体名（如 `logs`、`conversation_turns`），不强制模块前缀（本项目按单模块单体仓库约定）。
- 命名保持与 GORM 模型 struct 名一致（默认复数蛇形）。

---

## 二、API 接口规则

### 2.1 基础配置

| 配置项 | 值 | 说明 |
|-------|------|------|
| Base URL（管理接口） | `/api` | 管理后台与日志查询接口前缀，如 `/api/conversation/` |
| Base URL（relay 中转） | `/v1` | OpenAI 兼容中转接口前缀 |
| 认证方式 | Bearer Token | 管理接口走 JWT + Casbin 权限矩阵；relay 走 API Key（`Authorization: Bearer`） |
| 管理接口权限 | 仅管理员 | 挂在管理员路由组（`RequireRole` / Casbin 权限点） |

### 2.2 统一响应格式

```json
{
  "success": true,
  "message": "",
  "data": {}
}
```

成功响应经 `common.ApiSuccess(c, data)`，`data` 可为任意对象或数组；无 `data` 可返回时（如登出、会话撤销接口），仅返回 `success`/`message` 两字段。错误响应体结构按错误来源存在差异：

主流程错误经 `common.ApiError` / `common.ApiErrorI18n` / `common.ApiErrorMsg`，HTTP 200，无 `data`、无错误码字段：

```json
{
  "success": false,
  "message": "错误消息"
}
```

后台认证与会话接口错误，携带语义 HTTP 状态码（400/401/403/404/409/429/500），响应体为 `code`（`AUTH_*`）+ `message`（HTTP 状态文本），无 `data`：

```json
{
  "success": false,
  "code": "AUTH_UNAUTHORIZED",
  "message": "Unauthorized"
}
```

安全验证中间件错误，HTTP 403，`message` 为中文错误文案、`code` 为 `SECURITY_PROOF_*`：

```json
{
  "success": false,
  "message": "需要安全验证",
  "code": "SECURITY_PROOF_REQUIRED"
}
```

relay 中转接口不使用 `success`/`message`/`data` 结构，成功与错误均输出 OpenAI 兼容格式（`error` 内含 `message`/`type`/`code`）：

```json
{
  "error": {
    "message": "错误消息",
    "type": "new_api_error",
    "code": ""
  }
}
```

分页数据走 `common.PageInfo`：

```json
{
  "success": true,
  "message": "",
  "data": {
    "page": 1,
    "page_size": 20,
    "total": 100,
    "items": []
  }
}
```

### 2.3 分页参数

| 参数 | 类型 | 必填 | 默认值 | 说明 |
|------|------|------|--------|------|
| p | integer | 否 | 1 | 页码 |
| page_size | integer | 否 | 每页默认 | 每页数量，上限 100，经 `common.GetPageQuery(c)` 统一解析 |

---

**文档版本：** v1.0
**创建日期：** 2026-08-01
**适用模块：** 全部新增表结构与接口设计（含对话内容记录 P1_TECH_001_TECH_对话内容记录）
