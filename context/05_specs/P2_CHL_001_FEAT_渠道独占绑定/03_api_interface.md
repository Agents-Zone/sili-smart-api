# 渠道独占绑定 接口设计文档

## 文档信息

| 项目 | 内容 |
|------|------|
| Feature ID | P2_CHL_001_FEAT_渠道独占绑定 |
| 模块前缀 | chl_affinity（复用现有渠道亲和性配置区，无新资源前缀） |
| 文档版本 | v1.8 |
| 创建日期 | 2026-08-31 |
| 上游文档 | 01_功能需求规格说明书.md（SSOT）、AGENTS_DATABASE_API_RULE.md、context/03_architecture/architecture.md |

---

## 1. 设计概述

### 1.1 功能定位

本 Feature 在现有渠道亲和性能力上叠加渠道独占绑定语义。核心改动有三处：

1. 亲和规则结构新增 `exclusive_bind` 布尔开关（随现有规则编辑保存链路提交）
2. 现有缓存统计接口 `GET /api/option/channel_affinity_cache` 向后兼容扩展三个指标字段
3. 现有缓存清空接口 `DELETE /api/option/channel_affinity_cache` 清空范围扩展到反向占用索引与最近绑定记录

本 Feature 新增反向占用索引、最近绑定记录两项运行时数据，均落在 cachex.HybridCache（Redis 优先、内存回退），无新增业务数据表；持久化层面仅扩展现有 `options` 表中 `channel_affinity_setting.rules` 配置项的 JSON 结构。降级审计标记随消费日志 `other.admin_info.channel_affinity` 写入，无新增日志表列。

### 1.2 设计边界（沿用现状，SSOT 4.1.3 / 5.3.4）

- 无新增独立接口。前端开关字段经现有 `PUT /api/option/` 保存；统计指标经现有 `GET /api/option/channel_affinity_cache` 返回；清空行为经现有 `DELETE /api/option/channel_affinity_cache` 触发
- relay 中转路径（`/v1/*`）无接口变更，独占选路逻辑内嵌于渠道分发阶段，对客户端透明
- 权限沿用现状：`/api/option/*` 管理接口挂管理员路由组，仅超级管理员可读写；不新增权限点

### 1.3 接口协议与基础配置

| 配置项 | 值 | 来源 |
|-------|------|------|
| 接口协议 | HTTP，管理接口沿用现有 GET/PUT/DELETE 语义 | 规则文件 §2.1、现有代码 router/api-router.go:193-197 |
| Base URL | `/api` | 规则文件 §2.1 |
| 认证方式 | Bearer Token（JWT 会话），管理接口走管理员鉴权 | 规则文件 §2.1 |
| 权限 | 仅超级管理员（root），沿用现有 optionRoute 权限组 | SSOT 2.2 |
| 统一响应格式 | `{success, message, data}`，经 `common.ApiSuccess` / 错误返回 HTTP 200 + `success:false` | 规则文件 §2.2 |

---

## 2. 接口清单

### 2.1 接口总表

| 序号 | 接口 | 方法 | 路径 | 变更类型 | 需求追溯 |
|-----|------|------|------|---------|---------|
| 1 | 保存亲和性规则配置（含独占开关） | PUT | `/api/option/` | 存量接口，请求体 JSON 结构扩展 | SSOT 4.1（F-规则编辑）、5.1.4 规则1（开关切换清空绑定） |
| 2 | 查询亲和缓存统计（含独占指标） | GET | `/api/option/channel_affinity_cache` | 存量接口，响应体扩展 3 个字段 | SSOT 4.2（F-设置区统计行）、5.3.3 |
| 3 | 清空亲和缓存 | DELETE | `/api/option/channel_affinity_cache` | 存量接口，清空范围扩展 | SSOT 5.2.2 第4条 |
| 4 | 查询亲和键上游缓存命中统计 | GET | `/api/log/channel_affinity_usage_cache` | 无变更，列出以完整性 | 现状（SSOT 4.2 为另一弹窗，本功能不改） |
| 5 | 读取选项配置（含 `channel_affinity_setting.rules` 初始值） | GET | `/api/option/` | 无变更，全站通用存量读取链路（前端设置页 defaultValues 加载），列出以完整性 | 现状（接口 1 保存值的读取侧） |

### 2.2 接口关系说明

接口 1 保存规则后，若任一规则的 `exclusive_bind` 相对保存前值发生变化，后端在同一保存事务语义内同步清空该规则的正向亲和缓存、反向占用索引与最近绑定记录（SSOT 4.1.4 规则1）。清空动作内嵌于保存处理，无独立清空接口。

接口 3 的全部清空（`all=true`）与按规则清空（`rule_name=xxx`）两种模式，均同步清理正向缓存、反向占用索引、最近绑定记录三个命名空间（SSOT 5.2.4 规则2）。

---

## 3. 接口详细设计

### 3.1 保存亲和性规则配置（含独占开关）

**接口路径：** `PUT /api/option/`

**方法：** PUT（沿用现有 option 更新语义，规则文件 §2.1 允许的存量接口形态）

**需求追溯：** SSOT 4.1（规则编辑弹窗独占绑定开关）、4.1.4 规则1、5.1.4 规则5（开启前提醒为前端交互，后端不校验）

**认证与权限：** 管理员路由组，仅超级管理员。

**变更类型：** 存量接口。请求体中 `channel_affinity_setting.rules` 的 JSON 数组元素结构扩展一个布尔字段 `exclusive_bind`，其余字段与现状一致。旧客户端提交的规则 JSON 缺失该字段时按 `false` 解析，行为等同于关闭独占，向后兼容。

**请求参数：**

```json
{
  "key": "channel_affinity_setting.rules",
  "value": "[{\"name\":\"claude cli trace\",\"model_regex\":[\"^claude-.*$\"],\"path_regex\":[\"/v1/messages\"],\"key_sources\":[{\"type\":\"gjson\",\"path\":\"metadata.user_id\"}],\"value_regex\":\"\",\"ttl_seconds\":0,\"skip_retry_on_failure\":true,\"include_using_group\":true,\"include_model_name\":false,\"include_rule_name\":true,\"exclusive_bind\":true}]"
}
```

**请求字段说明：**

| 字段 | 类型 | 必填 | 校验规则 | 说明 |
|------|------|------|---------|------|
| key | string | 是 | 固定值 `channel_affinity_setting.rules`，其它 key 走现有 option 逻辑 | 配置项键名 |
| value | string | 是 | JSON 数组字符串，逐元素反序列化为规则结构；解析失败行为见错误场景表（沿用现有 option 配置加载链路的静默跳过语义）。源码侧 OptionUpdateRequest.Value 声明为 any、经 switch 归一化为 string 落库（存量接口形态，实际链路始终提交字符串） | 规则数组序列化结果 |
| value[].exclusive_bind | boolean | 否 | 布尔，缺省 false | 新增字段。启用后该规则的亲和绑定执行渠道独占语义 [需求：SSOT 4.1.2] |

`value[].exclusive_bind` 之外的规则字段沿用现状（name、model_regex、path_regex、user_agent_include、key_sources、value_regex、ttl_seconds、param_override_template、skip_retry_on_failure、include_using_group、include_model_name、include_rule_name），本设计不重复定义。

**响应示例（成功）：**

```json
{
  "success": true,
  "message": ""
}
```

**服务端处理逻辑（保存成功后的联动，SSOT 4.1.4 规则1 / 5.2.2）：**

1. 反序列化新规则数组，与保存前的规则数组逐规则（按 name 对齐）比对 `exclusive_bind` 值
2. 对发生变化的规则，清除该规则作用域内的正向亲和缓存条目（键前缀含规则名时按前缀清除；规则未启用 include_rule_name 时按旧规则定义的键构成回放清除）、反向占用索引条目、最近绑定记录条目。工程边界：旧规则键构成（IncludeModelName/IncludeUsingGroup 组合 + 亲和键值）无法回放出可区分前缀时，退化为正向缓存与反向占用索引、最近绑定记录整体清空，并记 SysLog 声明该退化（清除范围扩大属工程兜底，保证旧语义数据不残留）。触发条件除 exclusive_bind 值变化外，还包括已启用独占的规则被删除（新列表无该名）或改名后旧名残留，此时对旧名规则同样执行上述清空（SSOT 4.1.4 规则1，v1.7 行为回写）
3. 清空失败时记录 SysError 日志，不影响保存结果返回（保存以配置落库为准）

**错误场景：**

| 场景 | HTTP 状态 | 响应 |
|------|----------|------|
| value JSON 解析失败 | 200 | `success:true`（保存成功）。沿用现有 option 配置加载链路：非法 JSON 字符串照常落库，但内存侧 Slice 字段反序列化失败时静默跳过该配置项的内存更新（生效值为落库前的旧规则），接口不返回错误。非法配置仅能通过服务端行为异常（规则不生效/回退旧值）间接发现，无专门告警。前端保存前的规则编辑器 JSON 结构化编辑使该场景实际难以触达 |
| 清空联动失败 | 200 | 保存成功，失败仅记服务端日志 |

### 3.2 查询亲和缓存统计（含独占指标）

**接口路径：** `GET /api/option/channel_affinity_cache`

**方法：** GET

**需求追溯：** SSOT 4.2（设置区统计行新增三个只读指标）、4.2.4 规则1（指标口径）、5.3.2 第3条

**认证与权限：** 管理员路由组，仅超级管理员。

**变更类型：** 存量接口，响应体 `data` 向后兼容扩展。新增 3 个字段：`exclusive_bindings`、`shared_bindings`、`degraded_reuse_total`。原有字段（enabled、total、unknown、by_rule_name、cache_capacity、cache_algo）保持不变。

**查询参数：** 无。

**响应示例：**

```json
{
  "success": true,
  "message": "",
  "data": {
    "enabled": true,
    "total": 128,
    "unknown": 0,
    "by_rule_name": {
      "codex cli trace": 64,
      "claude cli trace": 64
    },
    "cache_capacity": 100000,
    "cache_algo": "lru",
    "exclusive_bindings": 42,
    "shared_bindings": 7,
    "degraded_reuse_total": 13
  }
}
```

**新增响应字段说明：**

| 字段 | 类型 | 说明 |
|------|------|------|
| exclusive_bindings | integer | 独占绑定数。当前处于独占状态的绑定键数（渠道维度绑定键数为 1 的渠道上的键数总和）。按反向占用索引实时统计，跨规则全局口径 [需求：SSOT 4.2.2] |
| shared_bindings | integer | 复用绑定数。当前处于满载复用共享状态的绑定键数（绑定键数大于 1 的渠道上的键数总和）。同一渠道口径 [需求：SSOT 4.2.2] |
| degraded_reuse_total | integer | 累计降级次数。自进程启动以来满载复用降级的发生次数，进程内计数器，重启清零 [需求：SSOT 4.2.2] |

**统计口径实现说明（SSOT 4.2.4 规则1）：** 遍历反向占用索引全部渠道条目，渠道条目键数为 1 计入独占、大于 1 计入复用；`exclusive_bindings` 与 `shared_bindings` 之和应等于正向绑定中登记反向索引的键总数（正向缓存中可能存在极少量索引登记失败的条目，两口径允许存在短暂偏差，以索引为准展示）。索引条目为渠道级整体 TTL，正向绑定已过期但条目未整体过期时成员残留，口径偏高：静默渠道最多一个 TTL 周期，活跃渠道因条目 TTL 按登记续期而无固定上界，属设计内滞后（详见模型文档 4.1）。

**边界值：** 反向占用索引读取失败时，两指标返回 0 并记录 SysError；前端空值展示沿用通用规范显示 `-`（SSOT 4.2.5）。

**错误场景：**

| 场景 | HTTP 状态 | 响应 |
|------|----------|------|
| 索引键遍历失败 | 200 | `success:true`，独占/复用指标为 0，服务端记错误日志 |
| 进程内降级计数器读取 | 200 | 无失败场景（内存原子读） |

### 3.3 清空亲和缓存

**接口路径：** `DELETE /api/option/channel_affinity_cache`

**方法：** DELETE（沿用现有接口语义）

**需求追溯：** SSOT 5.2.2 第4条（主动清空三批同清）、5.2.4 规则2、4.2.3（清空按钮行为）

**认证与权限：** 管理员路由组，仅超级管理员。

**变更类型：** 存量接口，行为扩展。清空范围从单一正向缓存扩展为正向缓存、反向占用索引、最近绑定记录三个命名空间同批清空。请求参数与响应结构保持不变。

**查询参数：**

| 参数 | 类型 | 必填 | 说明 |
|------|------|------|------|
| all | string | 否 | `true` 时清空全部亲和缓存（含正向、反向索引、最近绑定记录全命名空间） |
| rule_name | string | 否 | 按规则名清空；与 all 互斥，all 优先 |

**响应示例（成功）：**

```json
{
  "success": true,
  "message": "",
  "data": {
    "deleted": 128
  }
}
```

`deleted` 语义沿用现状（正向亲和缓存删除条数）；反向索引与最近绑定记录的清理数量不单独返回，清理失败时按 SSOT 5.2.5 处理（重试一次，仍失败记错误日志，提示沿用现有反馈）。

**错误场景（沿用现状）：**

| 场景 | HTTP 状态 | 响应 |
|------|----------|------|
| 缺少 all 且缺 rule_name | 400 | `success:false` + 缺参提示 |
| rule_name 未匹配到规则 | 400 | `success:false` + 未知规则名 |
| 规则未启用 include_rule_name | 400 | `success:false` + 无法按规则清空 |
| 反向索引/最近绑定记录清理失败 | 200 | 正向清空成功返回，失败记 SysError（重试一次后） |

### 3.4 查询亲和键上游缓存命中统计（无变更）

**接口路径：** `GET /api/log/channel_affinity_usage_cache`

**方法：** GET

**需求追溯：** 现状接口，本 Feature 无变更。SSOT 4.2 的设置区统计行三指标经接口 3.2 返回；CacheStatsDialog（按亲和键的上游缓存命中详情弹窗）为另一现状能力，使用本接口查询，本功能对其无变更，此接口保持原样。

**说明：** 列出仅为接口清单完整性，请求参数（rule_name、using_group、key_fp）、响应结构与现状一致，设计不涉及。

---

## 4. 内部接口（进程内函数契约）

本 Feature 的核心行为内嵌于中转链路，无 HTTP 暴露。以下为 service 层新增/扩展的内部契约，作为 dev_plan 阶段的实现边界。

### 4.1 独占绑定选路（扩展 service/channel_affinity.go）

| 函数 | 签名 | 职责 | 需求追溯 |
|------|------|------|---------|
| 独占绑定决策 | `GetPreferredChannelByAffinity` 扩展：亲和 miss 且规则启用 exclusive_bind 时，进入独占选路；返回值形态保持 `(channelID int, found bool)`，绑定模式（独占/复用）仅在复用降级发生时经请求上下文标记传递（供降级审计消费，见 4.3） | 依据反向占用索引筛空闲渠道、满载降级最少绑定数优先、最近绑定记录优先绑回 | SSOT 5.1.2 第2条 |
| 原子占位 | 进程内按亲和键分段锁串行化；Redis 模式下经原子操作（Lua/SetNX 类）实现先到先得 | 同键并发首次绑定仅首个生效，其余读胜者结果 | SSOT 5.1.2 第2条末 |
| 绑定迁移 | 失败切换成功时，反向索引旧渠道移除该键、登记新渠道，最近绑定记录更新；迁入豁免独占判定，按复用计 | SSOT 5.1.2 第4条 |
| 占位回滚 | 请求终态失败且未成功切换时，同批清除正向绑定、反向索引条目、最近绑定记录 | SSOT 5.1.2 第4条末 |
| 绑定登记 | `RecordChannelAffinity` 扩展：正向写入同时登记反向索引（同 TTL）与最近绑定记录（两个 TTL 周期） | SSOT 5.2.2 第1条 |

### 4.2 反向占用索引与最近绑定记录（新增，cachex.HybridCache 命名空间）

| 数据项 | 命名空间（约定） | 值结构 | TTL | 需求追溯 |
|-------|----------------|--------|-----|---------|
| 反向占用索引 | `new-api:channel_affinity_occupancy:v1` | 渠道 ID → 键指纹集合 | 与对应正向绑定一致 | SSOT 5.2.3 |
| 最近绑定记录 | `new-api:channel_affinity_last_bind:v1` | 键（含规则作用域）→ 渠道 ID + 绑定时间戳 | 正向绑定两个周期 | SSOT 5.1.3 |

索引内仅存亲和键指纹（沿用 `affinityFingerprint`，SHA1 前 16 位 hex），不存原始键值（SSOT 5.2.3）。

### 4.3 降级审计与计数（扩展）

| 行为 | 载体 | 内容 | 需求追溯 |
|------|------|------|---------|
| 降级审计标记 | 消费日志 `other.admin_info.channel_affinity` 追加 | 规则名、亲和键指纹、目标渠道 ID、该渠道当时绑定键数 | SSOT 5.3.2 第1条 |
| 警告日志 | `logger.LogWarn`，与请求关联 | 同上要素 | SSOT 5.3.2 第2条 |
| 降级计数 | 进程内原子计数器 | 累计降级次数，重启清零，供 3.2 接口读取 | SSOT 5.3.3 |

审计写入失败静默跳过，不影响请求与计费（SSOT 5.3.4 规则1、5.3.5）。

---

## 5. 错误码与异常汇总

本 Feature 无新增错误码枚举。管理接口错误沿用规则文件 §2.2 的两类形态：主流程业务错误（HTTP 200 + `success:false` + message）与参数校验错误（HTTP 400 + message），与现有 option/缓存接口一致。

运行时异常（无 HTTP 暴露）按 SSOT 5.1.5 / 5.2.5 / 5.3.5 落地为服务端日志：

| 异常场景 | 处理 | 日志级别 | 需求追溯 |
|---------|------|---------|---------|
| 反向占用索引读写失败 | 请求不阻塞，降级为现有软亲和 | SysError，含渠道与键指纹 | SSOT 5.1.5 |
| 原子占位竞争失败 | 读胜者绑定结果并复用同渠道 | 无 | SSOT 5.1.5 |
| 占位回滚失败 | 记录日志后放行，残留随 TTL 过期 | SysError，含渠道与键指纹 | SSOT 5.1.5 |
| 索引清空失败 | 重试一次，仍失败记录错误 | SysError | SSOT 5.2.5 |
| 降级审计写入失败 | 静默跳过 | SysError | SSOT 5.3.5 |

---

## 6. 安全说明

| 项目 | 说明 |
|------|------|
| 认证 | 管理接口沿用 Bearer Token（JWT 会话），管理员路由组校验 |
| 授权 | 仅超级管理员可读写亲和规则配置与清空缓存；降级审计信息仅随 admin_info 面板对管理员可见（现有剥离机制，非管理员日志视图剥离 admin_info） |
| 敏感数据 | 反向占用索引与审计标记仅存亲和键指纹（SHA1 截断 16 位 hex），不扩散原始键值 [需求：SSOT 5.2.3、8.1 键指纹] |
| 输入校验 | exclusive_bind 布尔类型经 JSON 反序列化天然约束；rules JSON 解析失败沿用现有配置加载链路的静默跳过语义（见 3.1 错误场景表） |
| 限流 | 沿用现有 option 管理接口的会话级限流策略，无接口级新增限流 |

---

## 7. 验证对照

| 验证项 | 结论 |
|-------|------|
| 所有页面功能有对应接口 | 4.1 开关保存→3.1；4.2 三指标→3.2；清空行为→3.3；5.1-5.3 引擎为内部契约→第 4 节 |
| GET/POST（存量 GET/PUT/DELETE）方法规范 | 沿用现有接口形态，无新增 REST 动词；本 Feature 未新增 HTTP 端点 |
| 请求/响应格式统一 | 遵循规则文件 §2.2 的 success/message/data 结构 |
| 错误码覆盖 | 沿用现有错误形态，场景见 3.1-3.3 错误表与第 5 节 |
| 认证与限流说明 | 见第 6 节 |
| 向后兼容 | 3.1 请求体新增字段缺省 false；3.2 响应体仅新增字段；3.3 请求响应结构不变 |

---

**文档版本：** v1.8
**最后更新：** 2026-09-04
**作者：** lixuetao

**变更记录：**

| 版本 | 日期 | 变更内容 |
|------|------|---------|
| v1.0 | 2026-08-31 | 初始版本 |
| v1.1 | 2026-09-01 | 监理扫描修复：3.2 统计口径补充索引成员级滞后窗口说明；3.4 参数清单补 key_hint（v1.5 复核订正：该接口实际无 key_hint 查询参数，删除） |
| v1.2 | 2026-09-01 | 监理扫描修复（随 SSOT v1.3）：需求追溯措辞从统计弹窗改为设置区统计行 |
| v1.3 | 2026-09-01 | 监理扫描修复：3.1 服务端处理逻辑第 2 条补充键构成不可回放时整体清空的工程边界（与开发计划 T7 对齐） |
| v1.4 | 2026-09-01 | 监理扫描修复：4.1 独占绑定决策行明确返回值形态不变、绑定模式经请求上下文传递（与开发计划 T4 对齐） |
| v1.5 | 2026-09-02 | 监理扫描修复（模式四）：3.4 参数清单订正为 rule_name/using_group/key_fp（对齐 controller 实际读取的查询参数，删 key_hint）；3.1 成功响应示例删去 data:null（实际响应无 data 键）；3.1 错误场景表经用户决策改为如实描述 value JSON 解析失败的现状行为（静默跳过内存更新、返回 success:true），删去未实现的 success:false 响应示例 |
| v1.6 | 2026-09-03 | 代码评审修复（第二轮）：4.2 键指纹长度由 SHA1 前 8 位订正为前 16 位 hex（实现同步加长，SSOT 偏离 D5）；3.2 统计口径滞后窗口上界陈述订正（活跃渠道条目按登记续期、无固定上界）；第 6 节安全敏感数据同步指纹长度 |
| v1.7 | 2026-09-04 | 监理扫描修复（模式四第二轮）：3.1 服务端处理逻辑第 2 条补记规则删除/改名后旧名残留的联动清空触发（v1.7 代码修复行为回写，对齐 SSOT v1.8）；3.1 value 字段说明补注源码 any 声明归一化落库的存量形态 |
| v1.8 | 2026-09-04 | 监理扫描修复（模式四第三轮）：2.1 接口总表补列 GET /api/option/（全站通用存量读取链路，前端设置页 defaultValues 加载侧，列出以完整性）；同步清理前端 getAffinityUsageCache 残留的 key_hint 查询参数（v1.5 文档订正后前端未同步，本次代码侧一并移除） |
