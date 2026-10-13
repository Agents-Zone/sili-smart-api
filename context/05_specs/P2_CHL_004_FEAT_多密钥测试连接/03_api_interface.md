# 多密钥测试连接 接口设计文档

## 文档信息

| 项目 | 内容 |
|---|---|
| Feature ID | P2_CHL_004_FEAT_多密钥测试连接 |
| 模块前缀 | chl_multikey（复用现有渠道测试与多密钥管理资源） |
| 文档版本 | v1.0 |
| 上游文档 | 01_功能需求规格说明书.md、AGENTS_DATABASE_API_RULE.md、context/03_architecture/architecture.md |
| 数据变更 | 无新增表、无新增字段 |

## 1. 设计概述

本 Feature 为多密钥渠道提供指定密钥的连接测试接口。管理员在多密钥管理弹框的密钥列表操作列点击测试连接，前端携带渠道 ID 与密钥序号（零起索引）调用后端，后端绕过随机与轮询选钥逻辑，直接使用该序号对应密钥发起一次真实上游请求，结果即时返回。

接口为单密钥维度的只读探测，与渠道级手动测试 `GET /api/channel/test/:id` 共用测试链路（测试模型选择、请求构造、响应校验、消费日志），仅替换密钥来源为指定序号。测试通过或失败均不改变密钥状态与渠道响应时间统计，测试动作不占用多密钥管理的渠道轮询锁。

路径与协议决策：采用 `GET /api/channel/test/:id/:key_index`，与渠道级测试同前缀同方法，路径追加密钥序号维度。测试为只读探测，符合项目 GET 查询约定；响应沿用渠道级测试的 `success/message/time/error_code` 结构，前端复用既有耗时展示逻辑。

## 2. 接口基础约定

| 配置项 | 约定 |
|---|---|
| 协议 | HTTP GET |
| 路径 | `/api/channel/test/:id/:key_index` |
| 认证 | Bearer Token，沿用 JWT 会话，挂载渠道路由组 `AdminAuth` |
| 授权 | `RequirePermission(ChannelOperate)`，与渠道级测试一致 |
| 响应 | `{success, message, time, error_code?}`，HTTP 200，沿用渠道级测试格式 |
| 分页 | 单次探测，无分页 |
| 限流 | 沿用渠道路由组既有配置，单密钥测试无额外限流中间件；重复点击由前端按钮 loading 拦截 |
| 审计 | 只读探测，沿用渠道级手动测试口径，无操作审计；请求链路产生的消费日志见 §4.1 |

路由注册于 `router/channel-router.go` 的 `channelPermissionRoutes`，与 `/test/:id` 相邻。Gin 路由树中 `/test/:id` 与 `/test/:id/:key_index` 通行先例（如 `/:id/codex/usage`）一致，无冲突。

## 3. 接口清单

| 序号 | 接口 | 方法 | 路径 | 需求追溯 |
|---|---|---|---|---|
| 1 | 测试指定密钥连接 | GET | `/api/channel/test/:id/:key_index` | SSOT 2.2、3.2、4.1.2、4.1.3、4.1.4 规则 1～6、4.1.5 |

## 4. 接口详细设计

### 4.1 测试指定密钥连接

对多密钥渠道的指定序号密钥发起一次真实上游连接测试。

**请求参数：**

| 参数 | 位置 | 类型 | 必填 | 说明 |
|---|---|---|---|---|
| id | path | integer | 是 | 渠道 ID |
| key_index | path | integer | 是 | 密钥零起索引，与多密钥管理弹框序号列展示值减一对应；与 `multi_key/manage` 各 action 使用的 `key_index` 同一坐标系 |

无查询参数。规格规则 3 约定弹框内点击即测，接口不接收 model、endpoint_type、stream，测试固定按非流式执行，与渠道级手动测试默认行为一致。

**成功响应：**

```json
{
  "success": true,
  "message": "",
  "time": 1.234
}
```

**上游测试失败响应：**

```json
{
  "success": false,
  "message": "上游返回的错误原因",
  "time": 2.1,
  "error_code": "bad_response"
}
```

**响应字段：**

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| success | boolean | 是 | 测试是否成功 |
| message | string | 是 | 成功为空串；失败为原因文案 |
| time | number | 是 | 本次测试耗时，单位秒，毫秒精度；业务校验失败为 0.0，上游测试失败为实际耗时 |
| error_code | string | 否 | 上游链路错误携带 `types.NewAPIError` 错误码；业务校验失败省略 |

密钥明文仅在后端使用，任何响应都不得携带密钥内容或前缀（规格 SSOT 2.2）。

**服务端处理流程：**

1. 解析路径参数 `id` 与 `key_index`，解析失败返回错误响应。
2. 读取渠道：先查渠道缓存，未命中回退数据库查询，均失败返回业务校验失败。
3. 校验 `channel.ChannelInfo.IsMultiKey`，非多密钥模式返回业务校验失败，不发上游请求。
4. 校验密钥序号：`key_index < 0` 或 `key_index >= MultiKeySize` 返回越界；`GetKeys()` 实际长度短于序号时同样按越界处理，防御编辑边界下元数据与密钥列表短暂不一致。
5. 取 `GetKeys()[key_index]` 作为测试密钥。取键直接按序号索引，绕过 `GetNextEnabledKey` 的随机与轮询调度，因此轮询索引保持原值，密钥状态列表保持原值（规格规则 1）。禁用中的密钥（手动禁用、自动禁用）按序号直接可达，同样可测（规格规则 2）。
6. 调用既有测试链路（`testChannel` 等价路径）执行探测：测试模型按渠道 `test_model`、渠道模型列表第一个、系统默认模型的顺序取值；渠道类型不支持测试时（Midjourney、Midjourney Plus、Suno、Kling、Jimeng、DoubaoVideo、Vidu）返回类型不支持文案（规格规则 3、SSOT 4.1.5）。测试用户沿用渠道级测试的发起人解析：当前管理员 ID，上下文缺失时回退根用户。
7. 测试链路内部行为与渠道级手动测试一致：产生一条消费日志（token 名与内容为模型测试口径），按测试模型计费结算。差异项见第 8 步。
8. 单密钥测试的独占约束：测试结果仅返回前端展示，不调用密钥禁用或启用路径（规格规则 2、4）；不调用 `UpdateResponseTime`，渠道响应时间统计保持原值（规格规则 5）；不写回 `channel_info`，轮询索引、状态列表、禁用原因与禁用时间保持原值（规格规则 1、2）。
9. 返回统一响应，`time` 为本次请求实测耗时（秒）。

**错误响应：**

| 场景 | HTTP 状态 | 响应 |
|---|---:|---|
| 非管理员访问 | 401 | 沿用 `AdminAuth` 未登录响应 |
| 无渠道操作权限 | 403 | 沿用 `RequirePermission` 无权限响应，按钮侧表现为禁用并悬浮提示（SSOT 2.2） |
| 路径参数非数字 | 200 | `{"success":false,"message":"<解析错误>","time":0.0}` |
| 渠道不存在或已删除 | 200 | `{"success":false,"message":"渠道不存在","time":0.0}` |
| 渠道非多密钥模式 | 200 | `{"success":false,"message":"该渠道不是多密钥模式","time":0.0}` |
| 密钥序号越界 | 200 | `{"success":false,"message":"密钥索引超出范围","time":0.0}` |
| 渠道类型不支持测试 | 200 | `{"success":false,"message":"<渠道类型名> channel test is not supported","time":0.0}` |
| 上游连接失败 | 200 | `{"success":false,"message":"<上游原因>","time":<实际耗时>,"error_code":"<错误码>"}` |

业务校验文案沿用多密钥管理弹框既有口径（渠道不存在、该渠道不是多密钥模式、密钥索引超出范围），类型不支持与上游失败文案沿用渠道级测试链路。前端将 `message` 原样展示到 toast（SSOT 4.1.2、4.1.4 规则 6）。

## 5. 与既有接口的关系

| 接口 | 关系 |
|---|---|
| `GET /api/channel/test/:id` | 渠道级手动测试，按随机或轮询策略自动选钥。本接口与其共用测试执行链路与响应结构，密钥来源改为指定序号，且不更新渠道响应时间 |
| `POST /api/channel/multi_key/manage` | 多密钥启停删与状态查询，全程持有渠道轮询锁。本接口独立于该端点，测试期间不持有轮询锁，避免数秒级探测阻塞密钥管理操作 |
| 定时测试任务（`performChannelTests`） | 自动禁用与自动启用的唯一触发方。本接口为手动探测，与该任务互不干扰（规格规则 4） |

## 6. 需求追溯矩阵

| 规格条款 | 接口行为落点 |
|---|---|
| SSOT 2.2 权限：测试按钮要求 ChannelOperate，密钥明文不达前端 | 路由 `RequirePermission(ChannelOperate)`；响应无密钥字段 |
| SSOT 3.2 流程：点击测试连接后 toast 展示结果，弹框保持打开 | 单次 GET 探测，响应即时返回，无列表刷新 |
| SSOT 4.1.2 测试结果为临时反馈，不写入列表 | 响应仅 success/message/time/error_code，前端 toast 展示 |
| SSOT 4.1.3 测试连接按钮与 loading 拦截 | 单密钥测试无服务端幂等锁，重复请求由前端按钮状态拦截（SSOT 4.1.5） |
| 规则 1 指定密钥测试、不推进轮询索引 | 按 `key_index` 直接索引取键，绕过 `GetNextEnabledKey`，不写回 `channel_info` |
| 规则 2 禁用密钥可测试、测试后仍需手动启用 | 取键跳过状态过滤；响应不触发启用或禁用 |
| 规则 3 测试模型沿用渠道现有逻辑 | 复用渠道级测试的模型兜底顺序，接口无模型参数 |
| 规则 4 测试不联动状态变更 | 手动测试路径无 `processChannelError` 与 `EnableChannel` 调用 |
| 规则 5 不计入渠道响应时间 | 不调用 `UpdateResponseTime` |
| 规则 6 参数与状态校验 | §4.1 服务端处理流程第 2～4 步与错误响应表 |

## 7. 交付声明

| 产物 | status | path | basis |
|---|---|---|---|
| API 接口设计 | applicable | context/05_specs/P2_CHL_004_FEAT_多密钥测试连接/03_api_interface.md | 规格 SSOT 4.1.3 新增测试连接功能，需后端接口承载指定密钥探测；既有渠道级测试与多密钥管理接口均无法指定密钥 |

---

**文档版本：** v1.0
**最后更新：** 2026-10-13
**作者：** lixuetao
