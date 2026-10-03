# 令牌统计分析 数据模型设计

## 基础信息

- **Feature ID**：P2_TKN_001_FEAT_令牌统计分析
- **适用性**：not_applicable（无新增或变更持久化对象）
- **规格依据**：`01_功能需求规格说明书.md` §5.1.3 涉及数据（quota_data 与 tokens 均为既有表，只读消费）；§1.3 功能范围（数据采集链路 quota_data 写入为既有能力，不在本 Feature 范围内）；§6 数据状态定义（只读统计展示，无自有数据状态）
- **模块前缀**：`tkn_analysis`（本项目按规则 §1.9 不强制模块前缀，此处仅作文档分组标识）
- **数据库类型**：主库 SQLite / MySQL >= 5.7.8 / PostgreSQL >= 9.6（GORM v2，按 `SQL_DSN` 选择；本 Feature 无建表与迁移）

## 结论与依据

本 Feature 为纯只读统计查询：新增一个按 `token_id` 分组的聚合 SELECT，消费两张既有表，无任何 DDL、DML、索引变更或迁移语句。

| 对象 | 变更类型 | 依据 |
|---|---|---|
| quota_data 表 | 无变更（只读，新增一条聚合查询） | 规格 §5.1.3：既有统计表，含 token_id 字段；§5.1.4：聚合查询复用 quota_data 既有索引与分组方式 |
| tokens 表 | 无变更（只读，按 id IN 取 name） | 规格 §5.1.3：按 token_id 关联查询令牌名称 |

## 涉及表现状基线（供开发计划与验收核对）

以下为既有表的结构事实来源，非本次设计产出，无需执行任何语句。

### 1. quota_data（主库，既有）

**结构来源**：`model/usedata.go` `QuotaData` struct，经 GORM AutoMigrate 建表（`model/main.go`）。

**用途**：数据看板统计数据表，按小时粒度聚合记录消费（规格 §8.1 术语表）。

**字段说明**（GORM 模型即结构基线，三库统一由 GORM 生成类型；日期时间为 int64 Unix 秒级时间戳，规则 §1.7）：

| 字段名 | 类型（MySQL）| 类型（PostgreSQL）| 类型（SQLite）| 必填 | 默认值 | 说明 |
|--------|------|------|------|--------|--------|------|
| id | BIGINT | BIGINT | INTEGER | 是 | GORM 自动生成 | 主键（规则 §1.2） |
| user_id | BIGINT | BIGINT | INTEGER | 是 | - | 用户 ID，单列索引 |
| username | VARCHAR(64) | VARCHAR(64) | TEXT | 是 | '' | 用户名，组合索引 idx_qdt_model_user_name 第 2 列 |
| model_name | VARCHAR(64) | VARCHAR(64) | TEXT | 是 | '' | 模型名，组合索引 idx_qdt_model_user_name 第 1 列 |
| created_at | BIGINT | BIGINT | INTEGER | 是 | - | 聚合小时起点（写入时归一到整点），索引 idx_qdt_created_at |
| use_group | VARCHAR(64) | VARCHAR(64) | TEXT | 是 | '' | 用户分组，单列索引 |
| token_id | BIGINT | BIGINT | INTEGER | 是 | 0 | 令牌 ID；0 表示无令牌消费（如渠道模型测试），单列索引 |
| channel_id | BIGINT | BIGINT | INTEGER | 是 | 0 | 渠道 ID，单列索引 |
| node_name | VARCHAR(64) | VARCHAR(64) | TEXT | 是 | '' | 节点名，单列索引 |
| token_used | BIGINT | BIGINT | INTEGER | 是 | 0 | token 用量合计 |
| count | BIGINT | BIGINT | INTEGER | 是 | 0 | 请求次数合计 |
| quota | BIGINT | BIGINT | INTEGER | 是 | 0 | 消费配额合计（int32 语义边界由配额饱和 helper 守护，非本 Feature 改动） |

**索引说明**：

| 索引名 | 类型 | 字段 | 用途 |
|--------|------|------|------|
| idx_quota_data_user_id | INDEX | user_id | 按用户过滤 |
| idx_quota_data_token_id | INDEX | token_id | 按令牌过滤（本 Feature 聚合查询的分组维度，既有） |
| idx_quota_data_channel_id | INDEX | channel_id | 按渠道过滤 |
| idx_quota_data_node_name | INDEX | node_name | 按节点过滤 |
| idx_quota_data_use_group | INDEX | use_group | 按分组过滤 |
| idx_qdt_model_user_name | INDEX | model_name, username | 模型 + 用户组合查询 |
| idx_qdt_created_at | INDEX | created_at | 时间窗口过滤（本 Feature 查询的时间条件，既有） |

**本 Feature 查询形态**（新增聚合 SELECT，无结构变更）：

```sql
SELECT token_id, created_at,
       sum(count) AS count, sum(quota) AS quota, sum(token_used) AS token_used
FROM quota_data
WHERE created_at >= ? AND created_at <= ?
GROUP BY token_id, created_at
```

说明：查询以 `idx_qdt_created_at` 做时间范围过滤，分组由聚合完成，与既有 `GetQuotaDataGroupByUser`（按 username, created_at 分组）同形态，满足规格 §5.1.4「复用 quota_data 既有索引与分组方式，仅分组字段由 username 变为 token_id」。实现在 GORM 上构建（项目规则：优先 GORM 方法），上表 SQL 仅表达查询语义。

**业务规则**：写入侧（缓存合并、小时归一、SaveQuotaDataCache 定时落库）为既有能力，规格 §1.3 明确排除在范围外。

### 2. tokens（主库，既有）

**结构来源**：`model/token.go` `Token` struct，GORM 软删除模型（`DeletedAt gorm.DeletedAt`）。

**用途**：令牌（API Key）主数据表。本 Feature 仅读取 id 与 name 两列做展示标识关联。

**本 Feature 关联查询**：

```sql
SELECT id, name FROM tokens WHERE id IN (?)
```

GORM 默认作用域自动追加 `deleted_at IS NULL`，已删除令牌查不到名称，接口返回空 token_name，由前端回退显示 `#token_id`（规格 §4.1.4 规则2）。该行为与既有 `fillFlowTokenNames`（`model/usedata_flow.go`）一致。

**软删除语义**：删除令牌只影响展示标识，历史统计数值不变（规格 §6）。

## ER 关系（只读消费视角）

```text
tokens (1) ──业务字段 token_id── (N) quota_data   关联仅经业务字段，无外键（规则 §1.5 禁止外键约束）
```

## 设计验证

- [x] 所有需求实体都有对应表：令牌聚合消费数据由既有 quota_data 承载，规格 §1.1 明确「数据链路天然具备，只缺查询接口与前端展示」
- [x] 无 DDL 需求：不适用（无表结构产出），本文件为现状基线核对
- [x] 查询复用既有索引（idx_qdt_created_at 时间过滤 + token_id 分组），无新增索引需求
- [x] 三库兼容：聚合 SELECT 与 `id IN` 关联均为标准 SQL，经 GORM 构建无方言分支
- [x] 主键、公共字段、命名、类型约束：无变更对象，不涉及
- [x] 字典字段：无（token_id 为业务 ID，quota_data 无枚举字段）
- [x] 迁移方案：不适用，零迁移

## 遗留问题

无。模型侧零改动，全部实现工作在接口层（见 `03_api_interface.md` §1.5 实现落点）。
