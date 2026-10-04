# 令牌统计分析 API 接口设计

## 基础信息

- **Feature ID**：P2_TKN_001_FEAT_令牌统计分析
- **适用性**：applicable
- **规格依据**：`01_功能需求规格说明书.md` §5.1 令牌维度消费数据查询接口（非页面功能，需 API 承载）；§4.1.2 查询字段与显示字段；§4.1.4 业务规则
- **模块前缀**：`tkn_analysis`（仅用于文档分组；项目规则 §1.9 约定表名不强制模块前缀，接口路径沿用现有 `/api/data/*` 路由组）
- **接口协议**：HTTP（项目规则 §2.1：管理接口 GET 查询，沿用既有看板数据接口的 GET 模式）
- **版本**：v1（无独立版本段，沿用 `/api` 前缀，与既有数据看板接口一致）
- **Base URL**：`/api`（项目规则 §2.1 管理接口前缀）

## 接口总览

| 接口 | 方法 | 路径 | 认证 | 需求追溯 |
|---|---|---|---|---|
| 查询令牌维度消费聚合数据 | GET | `/api/data/tokens` | AdminAuth | [需求：§5.1 令牌维度消费数据查询接口] |

本 Feature 仅此一个接口。规格 §4.1.3 中的筛选切换、Top 数量截取、图表渲染均为前端行为（Top N 由前端从同一份聚合结果截取，规格 §4.1.4 规则1），无需 API 承载；§4.1.3 明确"切换 Top 数量不重新请求接口"。

## 1. 查询令牌维度消费聚合数据

**接口路径**：`GET /api/data/tokens`

**需求追溯**：[需求：§5.1.1 功能概述]、[需求：§4.1.4 规则1 聚合维度]、[需求：§4.1.4 规则2 令牌名称关联与回退]、[需求：§4.1.4 规则3 时间跨度限制]

**路由挂载**：`router/api-router.go` 既有 `dataRoute` 组（`apiRouter.Group("/data")`）下新增一行，与 `/api/data/users` 并列，挂 `middleware.AdminAuth()`。

### 1.1 认证与权限

- 认证方式：JWT Session（管理后台登录态），经 `middleware.AdminAuth()` 校验，仅管理员（含根管理员）可访问。
- 权限依据：[需求：§2.1 数据权限]、[需求：§2.2 页面访问权限]（后端接口挂 AdminAuth 中间件）。
- 非管理员请求：由 AdminAuth 中间件直接拒绝（HTTP 401/403，按既有中间件行为），不进入 handler。

### 1.2 请求参数

Query 参数（无请求体）：

| 参数 | 类型 | 必填 | 校验规则 | 说明 |
|---|---|---|---|---|
| start_timestamp | integer (int64) | 是 | 十进制整数、> 0 | 统计窗口起始，Unix 秒级时间戳 |
| end_timestamp | integer (int64) | 是 | 十进制整数、> 0、>= start_timestamp、跨度 <= 2592000 秒 | 统计窗口结束，Unix 秒级时间戳 |

参数解析与校验复用既有 `parseFlowQuotaTimeRange` 模式（`controller/usedata.go`），跨度上限 2592000 秒（1 个月）与既有看板接口 `GetUserQuotaDates` / `GetUserFlowQuotaDates` 一致，满足 [需求：§4.1.4 规则3]（预设选项卡最大 29 天，此校验仅保护异常入参）。

**请求示例**：

```
GET /api/data/tokens?start_timestamp=1759276800&end_timestamp=1759363200
```

### 1.3 响应格式

统一响应格式（项目规则 §2.2）：HTTP 200，`success` / `message` / `data` 三段。

**成功响应**：

```json
{
  "success": true,
  "message": "",
  "data": [
    {
      "token_id": 101,
      "token_name": "key-prod-01",
      "created_at": 1759276800,
      "count": 42,
      "quota": 120500,
      "token_used": 3821000
    },
    {
      "token_id": 0,
      "token_name": "",
      "created_at": 1759276800,
      "count": 3,
      "quota": 2400,
      "token_used": 95000
    }
  ]
}
```

**data 数组元素字段说明**：

| 字段 | 类型 | 说明 | 规格依据 |
|---|---|---|---|
| token_id | integer | 令牌 ID；0 表示无令牌消费（如渠道模型测试），聚合数值照常计入 | §4.1.4 规则2 |
| token_name | string | 令牌名称，按 token_id 关联 tokens 表取得；令牌已删除（软删除）或名称为空时返回空字符串，由前端回退显示 `#token_id`；token_id 为 0 时恒为空字符串，前端显示「无令牌消费」 | §4.1.4 规则2、§4.1.2 显示字段 |
| created_at | integer (int64) | 聚合时间点，Unix 秒级时间戳，按小时对齐（quota_data 写入时已归一到整点，`created_at - created_at % 3600`） | §5.1.1（小时级聚合记录） |
| count | integer | 该令牌在该小时的请求次数合计（sum） | §5.1.2 步骤2 |
| quota | integer | 该令牌在该小时的消费配额合计（sum），前端按系统汇率换算金额展示 | §5.1.2 步骤2、§4.1.2 显示字段 |
| token_used | integer | 该令牌在该小时的 token 用量合计（sum） | §5.1.2 步骤2 |

分页：无。聚合记录全量返回（时间窗口最大 29 天 × 每小时一行，量级受时间跨度校验约束），排行与趋势由前端基于同一份结果计算，Top N 截取在前端完成（规格 §4.1.4 规则1、§4.1.3 切换 Top 数量不重新请求接口）。

**失败响应**（HTTP 200，`success: false`，无 `data`，沿用 `common.ApiErrorMsg`）：

| 场景 | message | 规格依据 |
|---|---|---|
| start_timestamp 缺失、非整数或 <= 0 | `invalid start_timestamp` | §5.1.5 时间参数非法 |
| end_timestamp 缺失、非整数或 <= 0 | `invalid end_timestamp` | §5.1.5 时间参数非法 |
| end_timestamp < start_timestamp | `invalid time range` | §5.1.5 时间参数非法 |
| 跨度超过 2592000 秒 | `时间跨度不能超过 1 个月` | §4.1.4 规则3、§5.1.5 跨度超限 |
| 数据库查询失败 | 通用错误信息（经 `common.ApiError`），后端记录日志 | §5.1.5 数据库查询失败 |

失败响应示例：

```json
{
  "success": false,
  "message": "时间跨度不能超过 1 个月"
}
```

### 1.4 处理流程

对应 [需求：§5.1.2 处理流程]，逐条落实：

1. Controller 解析并校验 start_timestamp、end_timestamp（非法或跨度超限按上表返回错误）。
2. Model 层查询 quota_data 表：`WHERE created_at >= ? AND created_at <= ?`，按 `token_id, created_at` 分组，聚合 `sum(count) as count, sum(quota) as quota, sum(token_used) as token_used`。仅分组字段由既有 `GetQuotaDataGroupByUser` 的 `username` 变为 `token_id`，查询形态与索引利用不变（[需求：§5.1.4 业务规则]）。
3. 汇总结果中的去重 token_id（含 0），关联 tokens 表 `SELECT id, name FROM tokens WHERE id IN ?` 补全名称。tokens 为 GORM 软删除模型，默认查询自动过滤已删除行，名称解析不到即保持空字符串，交由前端回退（与既有 `fillFlowTokenNames` 同一模式）。
4. 组装 `success` / `message` / `data` 响应（`c.JSON`，与本文件既有看板 handler 风格一致）返回聚合记录列表。

金额换算说明：接口只返回 quota 原始值，按系统汇率换算为展示金额的逻辑在前端完成（`renderQuotaCompat`），与用户统计一致（§1.2 业务目标 2、§4.1.2 显示字段）。

### 1.5 实现落点（供开发计划引用）

| 层 | 文件 | 改动 |
|---|---|---|
| Router | `router/api-router.go` | dataRoute 组新增 `dataRoute.GET("/tokens", middleware.AdminAuth(), controller.GetQuotaDatesByToken)` |
| Controller | `controller/usedata.go` | 新增 `GetQuotaDatesByToken` handler（参数解析、跨度校验、调用 model、组装响应） |
| Model | `model/usedata.go` | 新增 `GetQuotaDataGroupByToken(startTime, endTime)` 聚合查询与令牌名称关联 |

## 2. 安全说明

- **认证授权**：AdminAuth 中间件（JWT Session + 角色校验），仅管理员可见页签、可调用接口（[需求：§2.1、§2.2]）。前端页签对非管理员隐藏仅是展示层控制，接口侧由中间件强制。
- **限流**：沿用 `/api` 路由组既有全局限流配置，本接口为低频只读统计查询，无接口级独立限流需求（规格无非功能限流要求）。
- **敏感数据**：响应不含令牌密钥（token key），仅含 token_id 与 token_name；key 在系统内加密存储且日志与统计不落明文 key（架构文档 §4.2 敏感数据保护，与 quota_data 既有口径一致）。
- **输入验证**：时间参数强类型整数校验 + 正数校验 + 跨度上限，聚合查询经 GORM 参数化，无注入面。
- **HTTPS**：传输加密由部署层终止（架构既有约定），接口自身无额外要求。

## 3. 验证清单

- [x] 需要 API 承载的需求功能均有对应契约（§5.1 查询接口）；纯前端行为（筛选切换、Top 截取、图表渲染）有排除依据（§4.1.3）
- [x] HTTP 方法符合项目规则（§2.1 管理接口 GET 查询，与既有 `/api/data/*` 一致）
- [x] 请求/响应格式统一且完整（规则 §2.2 success/message/data 封装）
- [x] 错误场景覆盖：参数非法、时间倒序、跨度超限、数据库失败（§5.1.5 全部场景有对应 message）
- [x] 认证与权限说明完整（AdminAuth，§2 权限矩阵）
- [x] 无分页需求（聚合全量返回，量级受跨度上限约束，规格 §4.1.4 规则1 前端截取）
