# 渠道亲和性绑定查看 接口设计文档

## 文档信息

| 项目 | 内容 |
|---|---|
| Feature ID | P2_CHL_003_FEAT_渠道亲和性绑定查看 |
| 模块前缀 | chl_affinity（复用现有渠道亲和性运行时资源） |
| 文档版本 | v1.1 |
| 上游文档 | 01_功能需求规格说明书.md、AGENTS_DATABASE_API_RULE.md、context/03_architecture/architecture.md |
| 数据变更 | 无新增表、无新增字段 |

## 1. 设计概述

本 Feature 为根管理员提供当前有效亲和性缓存的渠道与令牌绑定明细，以及按单条绑定清除入口。查询接口读取现有 `new-api:channel_affinity:v1` HybridCache，Redis 已启用时使用 Redis 数据，Redis 未启用时使用进程内缓存，不合并两个来源。缓存值是渠道 ID，缓存键按已启用规则中 `context_int` 类型、键名为 `token_id` 的亲和键解析令牌 ID 与模型、分组维度。

查询接口只读，不修改缓存、渠道、令牌或规则，不提供分页、搜索、导出和全量解绑能力。单条删除接口按明细返回的缓存键后缀（`key_suffix`）精确删除一条绑定（某个令牌在某个模型/分组上下文对某渠道的亲和），复用内部单键清理路径（正向缓存、反向占用索引、最近绑定记录三批同清）。全量清空与按规则清空沿用渠道亲和性设置页既有的 `DELETE /api/option/channel_affinity_cache` 接口。一个渠道在响应中只出现一次，同渠道绑定按 token_id 升序、同令牌按键后缀升序，渠道按渠道名称升序。

## 2. 接口基础约定

| 配置项 | 约定 |
|---|---|
| 协议 | HTTP GET（查询）/ HTTP DELETE（删除单条绑定） |
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
| 2 | 删除单条亲和绑定 | DELETE | `/api/option/channel_affinity_bindings?key_suffix=` | SSOT 3.2、4.2.3、7.2、8.1 |

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
        {
          "token_id": 101,
          "token_name": "生产令牌",
          "model_name": "gpt-4",
          "key_suffix": "ruleA:gpt-4:101"
        },
        { "token_id": 102, "token_name": "备用令牌", "key_suffix": "ruleA:102" }
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
| data[].tokens | array | 是 | 同渠道绑定列表，每条对应一个缓存键，至少一项 |
| data[].tokens[].token_id | integer | 是 | 从缓存键解析出的 token_id |
| data[].tokens[].token_name | string | 是 | 令牌名称；空值按 `-` 返回 |
| data[].tokens[].model_name | string | 否 | 规则启用模型名键段时返回该绑定所属模型；未启用时省略 |
| data[].tokens[].using_group | string | 否 | 规则启用分组键段时返回该绑定所属分组；未启用时省略 |
| data[].tokens[].key_suffix | string | 是 | 缓存键后缀（去命名空间前缀），单条删除接口的定位参数 |

**服务端处理流程：**

1. 从 HybridCache 列出当前未过期键。Redis 模式读取 Redis，内存模式读取本地缓存；列键失败返回错误响应并记录 SysError。
2. 仅接受包含 `channel_affinity` 命名空间、能按当前规则解析的键。规则的全部键源必须是 `context_int` 类型且键名为 `token_id`；未启用规则名、模型名和分组名时允许单段 token_id 键。无法解析规则名、模型、分组或 token_id 的键忽略，字段值包含冒号时仍须保持可逆解析。
3. 读取键对应的渠道 ID。渠道记录不存在时忽略该绑定；渠道查询失败时返回错误响应，不返回部分不确定数据。按渠道 ID 去重。
4. 批量读取令牌模型。令牌记录不存在时忽略该绑定；令牌查询失败时返回错误响应，不返回部分不确定数据。每个缓存键独立成条，同一令牌在多个模型/分组上下文的绑定分别返回。
5. 将绑定聚合到渠道行，同渠道内按 token_id 升序、同令牌按键后缀升序；渠道名称升序，相同名称时按 channel_id 升序。
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

### 4.2 删除单条亲和绑定

**请求参数：**

| 参数 | 位置 | 类型 | 必填 | 说明 |
|---|---|---|---|---|
| key_suffix | query | string | 是 | 绑定缓存键后缀，取查询接口返回的 `data[].tokens[].key_suffix` 原值；须以 token_id 结尾 |

**成功响应：**

```json
{
  "success": true,
  "message": "",
  "data": {
    "deleted": 1
  }
}
```

**响应字段：**

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| data.deleted | integer | 是 | 实际删除的绑定数；键不存在时为 0 |

**服务端处理流程：**

1. 校验 `key_suffix`：去空格、去命名空间前缀后须非空且不含空白字符，最后一段须为正整数 token_id；不满足返回 HTTP 200 与 `success:false`。
2. 读取该键当前绑定的渠道 ID；键不存在或值非法时幂等返回 `deleted:0`。
3. 调用内部单键清理路径（Redis 模式为 Lua 原子脚本）：仅当键仍指向读取到的渠道时删除正向绑定，并同步清理该键的最近绑定记录与渠道占用索引成员。
4. 返回统一成功响应与实际删除数。

**错误响应：**

| 场景 | HTTP 状态 | 响应 |
|---|---:|---|
| `key_suffix` 缺失 | 200 | `{"success":false,"message":"缺少参数：key_suffix"}` |
| `key_suffix` 非法（空、含空白、不以 token_id 结尾） | 200 | `{"success":false,"message":"key_suffix 必须以 token_id 结尾"}` 等 |

`key_suffix` 使用查询接口返回的原值即可精确定位，含冒号的规则名、模型名、分组值不影响定位；前端无需自行拼接键。

## 5. 内部服务契约

建议在 `service/channel_affinity.go` 增加以下只读契约，在 controller 中保持薄封装：

| 函数 | 返回 | 职责 |
|---|---|---|
| `GetChannelAffinityBindings()` | `([]ChannelAffinityBinding, error)` | 枚举有效缓存、解析 token_id 与模型/分组维度、读取渠道与令牌、聚合排序 |
| `ChannelAffinityBinding` | `ChannelID int`, `ChannelName string`, `Tokens []ChannelAffinityToken` | API 数据结构 |
| `ChannelAffinityToken` | `TokenID int`, `TokenName string`, `ModelName string`, `UsingGroup string`, `KeySuffix string` | API 数据结构；`ModelName`/`UsingGroup` 仅展示用，`KeySuffix` 为删除定位参数 |
| `ClearChannelAffinityBindingByKeySuffix(keySuffix string)` | `(int, error)` | 校验键后缀、读取当前绑定渠道、复用内部单键清理路径删除该条绑定并同步清理历史记录与占用索引；返回实际删除数 |

`GetChannelAffinityBindings` 不得写入缓存，不得触发渠道状态更新、令牌访问时间更新或亲和性续期。缓存过期判断由 HybridCache 完成，服务层不自行延长 TTL。模型/分组维度的解析仅用于展示，值含冒号且两段同时启用时为近似切分；删除定位一律使用 `KeySuffix` 原键，单键清理路径不得放宽删除前的渠道一致性复核。

## 6. 安全、并发与一致性

- 授权完全复用 `RootAuth`，普通管理员、普通用户和匿名请求均无法读取或删除。
- 响应只返回渠道名称、令牌名称、模型/分组维度与缓存键后缀，不返回 token key、渠道 key 或亲和原始值。
- 查询期间缓存可能发生新增、过期或迁移，结果表示一次读取时刻的快照；不要求跨请求强一致。
- 单条删除与并发请求竞争时以单键清理路径的渠道一致性复核为准：删除前绑定已迁到其他渠道时不误删，残留随 TTL 收敛。
- 先完成缓存快照和模型关联，再排序返回，避免向前端暴露部分结果。
- 单次查询不分页。缓存条目数量受既有 `max_entries` 限制，目标管理接口响应时间小于 500ms；实现应批量读取渠道和令牌，避免 N+1 查询。

## 7. 验证对照

| 检查项 | 结论 |
|---|---|
| 页面功能覆盖 | 缓存数量点击、加载、数据、空状态、错误、关闭、单条绑定清除均由 GET 与 DELETE(key_suffix) 接口支撑 |
| 方法规范 | GET 仅查询；DELETE 仅删单条绑定，复用内部单键清理路径 |
| 响应格式 | 遵循 `{success,message,data}`；非法参数返回 HTTP 200 与 `success:false`（规则文件 §2.2 主流程错误口径） |
| 权限 | RootAuth，仅根管理员 |
| 排序与聚合 | 渠道名称升序；同渠道按 token_id 升序、同令牌按键后缀升序；渠道单行聚合，一键一行 |
| 数据边界 | 查询忽略过期、无法解析、渠道不存在、令牌不存在的条目；删除仅影响目标键 |
| 数据库变更 | 无 |
| SSOT 一致性 | 字段与第 4.2.2 节一致，未新增分页、筛选或全量变更操作 |

---

**文档版本：** v1.1
**最后更新：** 2026-10-13
**作者：** lixuetao
