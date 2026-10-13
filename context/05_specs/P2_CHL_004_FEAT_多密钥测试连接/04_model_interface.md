# 多密钥测试连接 数据库模型设计文档

## 文档信息

| 项目 | 内容 |
|---|---|
| Feature ID | P2_CHL_004_FEAT_多密钥测试连接 |
| 模型前缀 | chl_multikey（复用既有 `channels` 表） |
| 文档版本 | v1.0 |
| 数据库变更 | 无 |
| 设计依据 | 01_功能需求规格说明书.md、AGENTS_DATABASE_API_RULE.md、context/03_architecture/architecture.md |

## 1. 结论

**适用性：not_applicable。**

规格依据：

- SSOT 4.1.2 明确本功能无新增录入或展示字段，测试结果为 toast 即时反馈的临时信息，不写入列表。
- SSOT 6 明确本功能不新增数据状态、不产生状态流转，测试为只读探测操作。
- SSOT 4.1.4 规则 1、2、5 约定测试不推进轮询索引、不修改密钥禁用信息、不更新渠道响应时间统计，全程无持久化写入。

本 Feature 无新增表、字段、索引、迁移、字典数据与数据修正语句。密钥及其运行时状态存储于既有 `channels` 表，本功能仅读取。

## 2. 既有存储结构说明

密钥与多密钥状态为 `channels` 表既有列，本功能按下列方式只读使用：

| 存储位置 | 内容 | 本功能使用方式 |
|---|---|---|
| `channels.key` | 多密钥模式下按换行分隔的密钥列表 | 经 `GetKeys()` 按零起序号索引取得测试密钥，仅在后端测试链路中使用 |
| `channels.channel_info`（序列化 JSON 列，`model.ChannelInfo`） | `is_multi_key` 多密钥标记、`multi_key_size` 密钥数量、`multi_key_mode` 调度模式、`multi_key_status_list` 密钥状态、`multi_key_disabled_time` 禁用时间、`multi_key_disabled_reason` 禁用原因、`multi_key_polling_index` 轮询索引 | 校验 `is_multi_key` 与序号上界 `multi_key_size`；状态、禁用信息、轮询索引仅作展示上下文，测试过程不读取为调度条件，测试结束不写回 |
| `channels.test_model`、`channels.models` | 测试模型兜底来源 | 沿用渠道级测试链路的模型取值顺序 |

序号坐标系与 `POST /api/channel/multi_key/manage` 的 `key_index` 一致：`channels.key` 换行分隔后的零起索引。

## 3. 运行时数据交互

| 数据 | 交互 | 说明 |
|---|---|---|
| `multi_key_status_list`、`multi_key_disabled_time`、`multi_key_disabled_reason` | 只读跳过 | 指定序号直接取键，禁用中的密钥同样可测；测试结果不改变这些字段（规格规则 2、4） |
| `multi_key_polling_index` | 保持原值 | 绕过 `GetNextEnabledKey` 调度，轮询游标无任何推进（规格规则 1） |
| 渠道响应时间统计 | 保持原值 | 测试链路外不调用 `UpdateResponseTime`（规格规则 5） |
| 消费日志（`logs` 表） | 沿用既有写入 | 测试请求经中转链路产生的消费日志按渠道级手动测试既有行为记录，无结构变更 |

## 4. 索引、迁移与字典

无 DDL、无新增索引、无外键、无公共字段新增、无字典字段，不生成插入语句。`channels` 表既有索引与 `channel_info` JSON 列存储方式继续由现有模型维护，SQLite、MySQL、PostgreSQL 均无需迁移。

## 5. SSOT 验证

| 项目 | 结论 |
|---|---|
| 新增持久化数据 | 无；规格 4.1.2 与第 6 章均排除 |
| 密钥明文 | 仅后端测试链路使用，无任何返回前端的路径 |
| 状态流转 | 无；密钥状态转换规则维持渠道模块既有定义 |
| 三库兼容 | 只读访问既有列，无方言差异操作 |

---

**文档版本：** v1.0
**最后更新：** 2026-10-13
**作者：** lixuetao
