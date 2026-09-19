# 渠道亲和性绑定查看 接口设计文档

## 文档信息

| 项目 | 内容 |
|---|---|
| Feature ID | P2_CHL_003_FEAT_渠道亲和性绑定查看 |
| 模块前缀 | chl_affinity（复用现有渠道亲和性运行时资源） |
| 文档版本 | v1.0 |
| 上游文档 | 01_功能需求规格说明书.md、AGENTS_DATABASE_API_RULE.md、context/03_architecture/architecture.md |
| 数据变更 | 无新增表、无新增字段 |

## 1. 设计概述

本 Feature 为根管理员提供当前有效亲和性缓存的渠道与令牌绑定明细。接口读取现有 `new-api:channel_affinity:v1` HybridCache，Redis 已启用时使用 Redis 数据，Redis 未启用时使用进程内缓存，不合并两个来源。缓存值是渠道 ID，缓存键按已启用规则的 `token_id` 亲和键解析令牌 ID。

接口只读，不修改缓存、渠道、令牌或规则，不提供分页、搜索、导出、解绑和清理能力。一个渠道在响应中只出现一次，同渠道令牌按 token_id 升序，渠道按渠道名称升序。

## 2. 接口基础约定

| 配置项 | 约定 |
|---|---|
| 协议 | HTTP GET |
| 路径 | `/api/option/channel_affinity_bindings` |
| 认证 | Bearer Token，沿用 JWT 会话 |
| 授权 | `RootAuth`，仅根管理员 |
| 响应 | `{success, message, data}`，沿用 `common.ApiSuccess` 语义 |
| 分页 | 不分页，返回完整有效绑定数组 |
| 限流 | 沿用 option 路由组的管理接口限流 |

路由挂载在现有 `apiRouter.Group("/option")` 下，与 `channel_affinity_cache` 统计和清空接口使用同一根管理员认证中间件。

## 3. 接口清单

| 序号 | 接口 | 方法 | 路径 | 需求追溯 |
|---|---|---|---|---|
| 1 | 查询渠道亲和性绑定明细 | GET | `/api/option/channel_affinity_bindings` | SSOT 3.2、4.2、7.1、8.1～8.2 |

## 4. 接口详细设计

### 4.1 查询渠道亲和性绑定明细

**请求参数：** 无。未知查询参数忽略。

**成功响应：**

```json
{
  "success": true,
  "message": "",
  "data": [
    {
      "channel_id": 1,
      "channel_name": "主渠道",
      "tokens": [
        { "token_id": 101, "token_name": "生产令牌" },
        { "token_id": 102, "token_name": "备用令牌" }
      ]
    }
  ]
}
```

**响应字段：**

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| data | array | 是 | 按渠道聚合的有效绑定；无数据时为 `[]` |
| data[].channel_id | integer | 是 | 亲和缓存值对应的渠道 ID |
| data[].channel_name | string | 是 | 渠道名称；空值按 `-` 返回 |
| data[].tokens | array | 是 | 同渠道令牌列表，至少一项 |
| data[].tokens[].token_id | integer | 是 | 从缓存键解析出的 token_id |
| data[].tokens[].token_name | string | 是 | 令牌名称；空值按 `-` 返回 |

**服务端处理流程：**

1. 从 HybridCache 列出当前未过期键。Redis 模式读取 Redis，内存模式读取本地缓存；列键失败返回错误响应并记录 SysError。
2. 仅接受包含 `channel_affinity` 命名空间、能按当前规则解析的键。规则必须使用 `token_id` 作为键源；无法解析规则名、模型、分组或 token_id 的键忽略。
3. 读取键对应的渠道 ID。渠道记录不存在时忽略该绑定；渠道查询失败时返回错误响应，不返回部分不确定数据。按渠道 ID 去重。
4. 批量读取令牌模型。令牌记录不存在时忽略该绑定；令牌查询失败时返回错误响应，不返回部分不确定数据。同一渠道内按 token_id 去重。
5. 将令牌绑定聚合到渠道行，按 token_id 升序；渠道名称升序，相同名称时按 channel_id 升序。
6. 返回统一成功响应。渠道或令牌名称为空时使用 `-`，保留 ID。

**错误响应：**

| 场景 | HTTP 状态 | 响应 |
|---|---:|---|
| 非根管理员访问 | 403 | 沿用 `RootAuth` 无权限响应，不返回绑定数据 |
| 缓存键枚举失败 | 200 | `{"success":false,"message":"渠道亲和性绑定读取失败"}`，记录 SysError |
| 缓存值读取失败 | 200 | `success:false`，记录 SysError |
| 渠道或令牌查询失败 | 200 | `success:false`，记录 SysError；不返回部分不确定数据 |
| 没有有效绑定 | 200 | `success:true`，`data:[]` |

缓存内容为空属于正常空状态，不作为错误。错误文案沿用管理接口主流程错误格式，不新增错误码。

## 5. 内部服务契约

建议在 `service/channel_affinity.go` 增加以下只读契约，在 controller 中保持薄封装：

| 函数 | 返回 | 职责 |
|---|---|---|
| `GetChannelAffinityBindings()` | `([]ChannelAffinityBinding, error)` | 枚举有效缓存、解析 token_id、读取渠道与令牌、聚合排序 |
| `ChannelAffinityBinding` | `ChannelID int`, `ChannelName string`, `Tokens []ChannelAffinityToken` | API 数据结构 |
| `ChannelAffinityToken` | `TokenID int`, `TokenName string` | API 数据结构 |

该函数不得写入缓存，不得触发渠道状态更新、令牌访问时间更新或亲和性续期。缓存过期判断由 HybridCache 完成，服务层不自行延长 TTL。

## 6. 安全、并发与一致性

- 授权完全复用 `RootAuth`，普通管理员、普通用户和匿名请求均无法读取。
- 响应只返回渠道名称、令牌 ID 和令牌名称，不返回 token key、渠道 key、亲和原始值或缓存键指纹。
- 查询期间缓存可能发生新增、过期或迁移，结果表示一次读取时刻的快照；不要求跨请求强一致。
- 先完成缓存快照和模型关联，再排序返回，避免向前端暴露部分结果。
- 单次查询不分页。缓存条目数量受既有 `max_entries` 限制，目标管理接口响应时间小于 500ms；实现应批量读取渠道和令牌，避免 N+1 查询。

## 7. 验证对照

| 检查项 | 结论 |
|---|---|
| 页面功能覆盖 | 缓存数量点击、加载、数据、空状态、错误、关闭均由该 GET 接口支撑 |
| 方法规范 | GET 仅查询 |
| 响应格式 | 遵循 `{success,message,data}` |
| 权限 | RootAuth，仅根管理员 |
| 排序与聚合 | 渠道名称升序；同渠道 token_id 升序；渠道单行聚合 |
| 数据边界 | 忽略过期、无法解析、渠道不存在、令牌不存在的条目 |
| 数据库变更 | 无 |
| SSOT 一致性 | 字段与第 4.2.2 节一致，未新增分页、筛选或变更操作 |

---

**文档版本：** v1.0
**最后更新：** 2026-09-19
**作者：** lixuetao
