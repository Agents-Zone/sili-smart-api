# 渠道批量编辑 数据库模型设计文档

## 文档信息

| 项目 | 内容 |
|------|------|
| Feature ID | P2_CHL_002_FEAT_渠道批量编辑 |
| 数据库 | 主库（SQLite / MySQL >= 5.7.8 / PostgreSQL >= 9.6，按 `SQL_DSN` 选择） |
| ORM | GORM v2 |
| 版本 | v1.5 |
| 创建日期 | 2026-09-10 |
| 设计依据 | 01_功能需求规格说明书.md（SSOT）、AGENTS_DATABASE_API_RULE.md、context/03_architecture/architecture.md |

---

## 1. 设计结论

**本功能不新增任何数据库表、不新增任何列、不修改任何索引。**

渠道批量编辑是对既有 `channels` 表记录的部分列统一更新，加上对既有 `abilities` 路由索引表的按渠道重建。所有可编辑字段（group、tag、remark、models、model_mapping、weight、priority、test_model、auto_ban）均为 `channels` 表既有列，类型与长度全部沿用现状（specs 8.3 偏离记录：字段长度沿用现有单个编辑的字段定义）。审计走既有 `logs` 表（`RecordOperationAuditLog`），独占索引清理走既有 Redis 键空间（P2_CHL_001），均无表结构变更。

以下文档给出受影响表的现状定义（作为批量更新的事务范围与回归对照）、字段生效映射、事务与一致性设计、并发控制，以及"零 DDL 变更"的验证清单。

---

## 2. ER 关系（受影响实体）

```
channels (1) ────── (N) abilities          [业务字段 channel_id 关联，无外键]
   │
   ├── (审计写入) logs                      [RecordOperationAuditLog，主库日志]
   └── (事务提交后) Redis 独占占用索引        [P2_CHL_001，非数据库实体]
```

---

## 3. 受影响表现状定义

### 3.1 channels（核心业务表，现状，无变更）

**表名：** `channels`

**用途：** 上游渠道主表。批量编辑按 ids 更新其中 9 个可编辑列（见 4.1 映射表），其余列保持原值。

**现状 DDL（GORM 模型 `model/channel.go` Channel struct 经 AutoMigrate 生成，以下为等价 MySQL 表达，仅列批量编辑涉及列与关键列）：**

```sql
-- 现状表结构（无本功能变更）。本块为 AutoMigrate 在 MySQL 下的等价产出，仅供对照，
-- 不是手工建表 DDL：主键自增由 GORM 按方言生成（规则文件 §1.2 禁止的是手工
-- AUTO_INCREMENT/SERIAL DDL），SQLite/PostgreSQL 的等价实现由 GORM 适配。
-- 空值约束：GORM 仅对显式 `gorm:"not null"` tag 的列生成 NOT NULL（key 列），
-- 其余列在 MySQL/PostgreSQL 下为可空列，靠应用层保证写入非空值；本块按此口径标注。
CREATE TABLE `channels` (
  `id`           BIGINT        NOT NULL AUTO_INCREMENT,       -- GORM 自增主键
  `type`         BIGINT        NULL DEFAULT 0,
  `key`          LONGTEXT      NOT NULL,
  `status`       BIGINT        NULL DEFAULT 1,
  `name`         VARCHAR(191)  NULL,
  `weight`       BIGINT UNSIGNED NULL DEFAULT 0,              -- uint32 值域，指针列
  `models`       LONGTEXT      NULL,                          -- 无长度 tag → longtext
  `group`        VARCHAR(64)   NULL DEFAULT 'default',        -- 保留字列，方言引用见索引说明
  `model_mapping` TEXT         NULL,
  `priority`     BIGINT        NULL DEFAULT 0,
  `auto_ban`     BIGINT        NULL DEFAULT 1,                -- 1=启用 0=停用（*int）
  `test_model`   LONGTEXT      NULL,                          -- 无显式长度 tag，列不设长；(0,255] 为应用层校验
  `tag`          VARCHAR(191)  NULL,                          -- 无长度 tag 但有 index → varchar(191)
  `remark`       VARCHAR(255)  NULL,
  `created_time` BIGINT        NULL DEFAULT 0,                -- Unix 秒级时间戳
  -- ... 其余列（key/status_code_mapping/setting/settings/param_override/header_override/
  --     channel_info/openai_organization/base_url/other/other_info/balance 等）与本功能无关
  PRIMARY KEY (`id`),
  KEY `idx_channels_name` (`name`),
  KEY `idx_channels_tag` (`tag`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
```

> 时间戳为 Unix 秒级 int64（规则文件 §1.7）。上表 `created_time` 为存量列名：规则文件 §1.4 的 `created_at` 命名规范适用于新增表结构，不追溯存量列。本功能不改动该列。

**字段说明（仅批量编辑涉及列）：**

| 字段名 | 类型（MySQL） | 类型（PostgreSQL） | 类型（SQLite） | 必填 | 默认值 | 说明 |
|--------|------|------|------|--------|--------|------|
| id | BIGINT | BIGINT | INTEGER | 是 | GORM 生成 | 主键，批量更新的定位键（ids） |
| group | VARCHAR(64) | VARCHAR(64) | TEXT | 是（应用层） | 'default' | 分组，逗号分隔多值串 [长度来源：需求规格说明书 4.2.2 A 区] |
| tag | VARCHAR(191) | VARCHAR(191) | TEXT | 否 | NULL | 标签；批量填写时统一替换 [长度来源：需求规格说明书 4.2.2 A 区 (0,191]，应用层校验与列长对齐] |
| remark | VARCHAR(255) | VARCHAR(255) | TEXT | 否 | NULL | 备注 [长度来源：需求规格说明书 (0,255]] |
| models | LONGTEXT / TEXT | TEXT | TEXT | 是（应用层） | - | 模型列表，逗号分隔串；批量语义为统一替换（无追加） [长度来源：需求规格说明书 4.2.2 B 区] |
| model_mapping | TEXT | TEXT | TEXT | 否 | NULL | 模型重定向 JSON 对象序列化串（禁止 JSON 列类型，规则文件 §1.5） |
| weight | BIGINT UNSIGNED | BIGINT | INTEGER | 否 | 0 | 权重，0-4294967295（uint32 值域，GORM `*uint`） |
| priority | BIGINT | BIGINT | INTEGER | 否 | 0 | 优先级，int64 值域 |
| test_model | LONGTEXT / TEXT | TEXT | TEXT | 否 | NULL | 渠道测试模型；(0,255] 为应用层校验边界，列本身不限长 [长度来源：需求规格说明书 (0,255]] |
| auto_ban | BIGINT | BIGINT | INTEGER | 否 | 1 | 自动封禁开关：1=启用 0=停用 [枚举：auto_ban（1/0）] |
| created_time | BIGINT | BIGINT | INTEGER | 否 | 0 | 创建时间，Unix 秒级时间戳（现状，不改动） |

> 必填口径说明：数据库层仅 key 列有 NOT NULL 约束（GORM 仅对显式 `not null` tag 生成），group/models 的必填为应用层校验保证（生效值 trim 后非空才写入）。

**索引说明（现状，无变更）：**

| 索引名 | 类型 | 字段 | 用途 |
|--------|------|------|------|
| PRIMARY | PRIMARY KEY | id | 主键；批量更新按 id 定位行 |
| idx_channels_name | INDEX | name | 名称查询（既有） |
| idx_channels_tag | INDEX | tag | 标签查询/批量设标签（既有；批量编辑改 tag 后仍命中） |

**业务规则（本功能相关）：**

- 批量更新仅写 4.1 映射表中"生效"的列，以 GORM `Updates` + 列白名单构造，未生效列不进入 SET 子句（空即跳过在 SQL 层的实现方式）
- 上述 `Updates` 必须使用 map 形式（`Updates(map[string]any)`）。weight、priority、auto_ban 的合法生效值均包含 0（接口 2.2.5/2.2.7），而 GORM 对 struct 形式的 `Updates` 会静默丢弃零值字段，导致"权重归零""停用自动封禁"无报错失效；若采用 struct 形式，须配合 `Select(生效列)` 强制写入
- `group` 为保留字列，原生 SQL 场景按 `commonGroupCol` 方言约定引用（规则文件 §1.4）；本功能走 GORM 方法，无需手工引用
- 批量更新不触碰 key、type、base_url 等敏感列与 channel_info（多 Key 状态），与 D 区排除清单一致

### 3.2 abilities（路由索引表，现状，无变更）

**表名：** `abilities`

**用途：** 渠道-模型-分组路由能力索引。批量编辑涉及 models/group/weight/priority/tag 任一生效时，事务内对每个受影响渠道执行 delete（by channel_id）+ 重建 insert（复用 `UpdateAbilities(tx)`）。

**现状 DDL（GORM 模型 `model/ability.go` Ability struct，联合主键）：**

```sql
CREATE TABLE `abilities` (
  `group`      VARCHAR(64)  NOT NULL,
  `model`      VARCHAR(255) NOT NULL,
  `channel_id` BIGINT       NOT NULL,
  `enabled`    BOOLEAN      NULL,           -- GORM bool，无默认值 tag
  `priority`   BIGINT       NULL DEFAULT 0,
  `weight`     BIGINT       NULL DEFAULT 0,
  `tag`        VARCHAR(191) NULL,
  PRIMARY KEY (`group`, `model`, `channel_id`),
  KEY `idx_abilities_channel_id` (`channel_id`),
  KEY `idx_abilities_priority` (`priority`),
  KEY `idx_abilities_weight` (`weight`),
  KEY `idx_abilities_tag` (`tag`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
```

**字段说明（现状，无变更）：**

| 字段名 | 类型（MySQL） | 类型（PostgreSQL） | 类型（SQLite） | 必填 | 默认值 | 说明 |
|--------|------|------|------|--------|--------|------|
| group | VARCHAR(64) | VARCHAR(64) | TEXT | 是（主键） | - | 分组，联合主键之一；取自 channels.group 拆分 [长度来源：与 channels.group 同源] |
| model | VARCHAR(255) | VARCHAR(255) | TEXT | 是（主键） | - | 模型名，联合主键之一；取自 channels.models 拆分 |
| channel_id | BIGINT | BIGINT | INTEGER | 是（主键） | - | 关联 channels.id（业务字段关联，无外键，规则文件 §1.5） |
| enabled | BOOLEAN | BOOLEAN | INTEGER | 是（应用层） | - | 随渠道 status 派生（status==1 为 true） |
| priority | BIGINT | BIGINT | INTEGER | 否 | 0 | 冗余自渠道 priority，路由排序用 |
| weight | BIGINT | BIGINT | INTEGER | 是（应用层） | 0 | 冗余自渠道 weight（uint32 值域） |
| tag | VARCHAR(191) | VARCHAR(191) | TEXT | 否 | NULL | 冗余自渠道 tag |

**索引说明（现状，无变更）：**

| 索引名 | 类型 | 字段 | 用途 |
|--------|------|------|------|
| PRIMARY | PRIMARY KEY | (group, model, channel_id) | 联合主键，路由查询核心 |
| idx_abilities_channel_id | INDEX | channel_id | 按渠道重建/删除（本功能逐渠道 delete 的命中路径） |
| idx_abilities_priority | INDEX | priority | 路由排序 |
| idx_abilities_weight | INDEX | weight | 路由加权 |
| idx_abilities_tag | INDEX | tag | 按标签操作 |

**业务规则（本功能相关）：**

- 重建在批量编辑事务内逐渠道执行（`UpdateAbilities(tx)`：先 delete by channel_id，再按新 models×group 笛卡尔积 insert，chunk 50、OnConflict DoNothing），失败即整体回滚（specs 4.2.4 规则3/4 同一口径）
- 路由字段（models/group/weight/priority/tag）全部未生效时，跳过该渠道的 abilities 重建（无变更无写入）
- enabled 取渠道当前 status 派生，批量编辑不修改渠道启停状态（specs 6.1 无状态流转）

### 3.3 logs（审计日志表，现状，无变更）

**表名：** `logs`（主库/日志库，按既有路由）

**用途：** 批量编辑成功后经 `recordManageAudit(c, "channel.update_batch", params)` 写入一条管理审计日志，params 进 `Other` 结构化参数（含 count、channel_ids、updated_fields、values 所填值全量原文）。

**涉及列（现状）：** `user_id`、`content`（英文兜底模板渲染）、`other`（结构化参数，内嵌 `op.action` 与 `op.params`，含 count、channel_ids、updated_fields、values 所填值全量原文）、`ip`、`created_at` 等，均既有定义，无变更。本功能仅在 `controller/audit.go` 的 `auditContentTemplates` 表中登记 `channel.update_batch` 模板项（代码常量，无 DDL）。

---

## 4. 字段生效映射与事务设计

### 4.1 请求字段 → 列更新映射

| 请求字段 | channels 列 | 生效条件 | 写入方式 | 触发 abilities 重建 |
|---------|------------|---------|---------|-------------------|
| group | group | 字段出现且非 null，trim 后非空 | SET group = :v | 是 |
| tag | tag | 字段出现且非 null，trim 后非空（留空即跳过，不清空；清空走既有 /api/channel/batch/tag 留空逻辑） | SET tag = :v | 是 |
| remark | remark | 字段出现且非 null，trim 后非空 | SET remark = :v | 否 |
| models | models | 字段出现且非 null，trim 后非空 | SET models = :v | 是 |
| model_mapping | model_mapping | 字段出现且非 null，且为非空合法 JSON 对象（空串拒绝，见接口文档 2.2.6） | SET model_mapping = :v | 否 |
| weight | weight | 字段出现且非 null | SET weight = :v | 是 |
| priority | priority | 字段出现且非 null | SET priority = :v | 是 |
| test_model | test_model | 字段出现且非 null，trim 后非空 | SET test_model = :v | 否 |
| auto_ban | auto_ban | 字段出现且非 null（0/1） | SET auto_ban = :v | 否 |

注：model_mapping 不参与 abilities 生成（abilities 仅由 models×group×路由权重派生），故不触发重建；tag 参与 abilities 行冗余，故触发。

### 4.2 事务流程（specs 4.2.4 规则3/4）

```
BEGIN
  ├─ SELECT channels WHERE id IN (ids)            -- 事务内读目标渠道
  ├─ for each channel:
  │    ├─ UPDATE channels SET <生效列>  WHERE id = ?   -- 列白名单，仅生效列
  │    └─ if 路由列生效: UpdateAbilities(tx)            -- delete by channel_id + insert
  │       └─ 任一步失败 → ROLLBACK，返回触发回滚的失败渠道与原因（首个失败即回滚）
  └─ COMMIT
事务后（非事务内，失败不阻断结果）:
  ├─ model.InitChannelCache()                     -- 刷新渠道缓存
  ├─ 清理受影响渠道独占运行时数据（Redis, P2_CHL_001）：反向索引条目 + 最近绑定记录 + 该渠道正向绑定三批同清（受影响渠道指本次有路由相关字段 models/group/weight/priority/tag 生效的渠道，全部未生效时跳过清理）；失败重试一次，仍失败 SysError + TTL 收敛
  └─ recordManageAudit("channel.update_batch")    -- 审计日志
```

> **依赖状态（独占运行时数据清理）**：该能力由 P2_CHL_001 经 service 层导出函数提供，函数名与签名待其落地后回填；P2_CHL_001 当前 `dev_exec` 为 in_progress，service 层尚无按渠道清理的导出函数。本 Feature 的 dev_plan 须将该项列为阻塞前置。

单次上限 200 条渠道（specs 4.2.4 规则3），事务规模受控（200 × (1 UPDATE + abilities delete/insert 分片)），现有 `BatchDeleteChannels`/`BatchSetChannelTag` 已验证同规模事务在三库可行。

### 4.3 零 DDL 变更验证

| 检查项 | 结论 |
|--------|------|
| 新增表 | 无 |
| channels 新增/修改列 | 无（10 个请求字段全部映射到既有列） |
| abilities 新增/修改列或索引 | 无（重建复用既有结构） |
| 字典表 INSERT | 无（auto_ban 为 0/1 代码常量枚举，规则文件 §1.8 不生成字典语句） |
| AutoMigrate 影响 | 无（模型 struct 无改动） |
| 三库兼容 | 无新增 SQL 方言面（GORM Updates + 既有 UpdateAbilities） |

---

## 5. 一致性与并发控制

| 关注点 | 设计 |
|--------|------|
| 事务原子性 | 单事务覆盖 channels 更新与 abilities 重建，全成全败（GORM `DB.Begin/Commit/Rollback`，与 BatchSetChannelTag 同模式） |
| 行锁 | 批量更新按主键 id 逐行 UPDATE，依赖行级写锁自然串行化；无需显式 `lockForUpdate`（不做读-改-写竞争读，生效值来自请求而非读值回写） |
| 与单个编辑并发 | 两个路径均为按 id 的列更新，最后提交者胜出；abilities 重建各自事务内自洽，无跨表半状态 |
| 渠道缓存 | 事务提交后统一 `InitChannelCache()` 一次（200 条一次全量刷新，与现有批量接口一致），避免事务内多次刷新 |
| 独占绑定索引（P2_CHL_001） | 事务提交后按渠道三批同清（反向索引条目、最近绑定记录、该渠道正向绑定）；清理失败重试一次，仍失败记 SysError，残留随 TTL 自然过期收敛，不影响批量编辑结果（specs 4.2.4 规则4） |
| 审计失败 | 审计写入失败不影响业务结果（RecordOperationAuditLog 内部日志），与既有渠道接口口径一致 |

---

## 6. 性能与容量

| 项 | 评估 |
|----|------|
| 单次事务规模 | ≤200 渠道；abilities 重建量 = 渠道数 × models×group 笛卡尔积，chunk 50 批量 insert |
| 管理接口响应目标 | < 500ms（架构文档 4.1）；200 条上限即为此约束的批量保护 |
| 索引命中 | channels 按主键更新；abilities delete 命中 idx_abilities_channel_id |
| 无分表分库 | 沿用主库现状；渠道量为管理规模数据，无分片需求 |

---

## 7. 需求覆盖追溯

| specs 条目 | 数据库设计落点 |
|-----------|--------------|
| 4.2.2 A/B/C 区字段 | 3.1 字段说明（全部既有列，类型长度一致） |
| 4.2.2 D 区排除字段 | 3.1 业务规则（列白名单 SET，敏感列不触碰） |
| 4.2.4 规则1 空即跳过 | 4.1 生效条件列 |
| 4.2.4 规则3 事务全成全败 | 4.2 事务流程 |
| 4.2.4 规则4 abilities + 独占索引 | 3.2 业务规则 + 4.2 事务后步骤 + 5 一致性 |
| 4.2.4 规则5 审计日志 | 3.3 logs 表 + auditContentTemplates 登记 |
| 4.2.4 规则6 成功后清理 | 4.2 InitChannelCache（缓存侧） |
| 6.1 无状态流转 | auto_ban/status 列无状态机改动 |

---

**文档版本：** v1.5
**创建日期：** 2026-09-10
**作者：** lixuetao
**变更记录：** v1.1 监理修复：tag 行长度来源更新为规格说明书 (0,191]（应用层校验与列长对齐，消除校验 255 放行超列长值导致写库失败的风险）。v1.2 监理修复：3.1 DDL 块头注释澄清本块为 AutoMigrate 等价产出而非手工建表 DDL（消除与规则文件 §1.2 禁 AUTO_INCREMENT 的表面对撞）；3.1 时间戳注释补充 created_time 为存量列的命名规范豁免说明。v1.3 监理修复（模式二）：3.1 业务规则补充 Updates 必须用 map 形式（weight/priority/auto_ban 的 0 值为合法生效值，struct 形式会被 GORM 静默丢弃）；4.2 事务流程图与 5 一致性表的独占索引清理改为按渠道三批同清（对齐 P2_CHL_001 5.2.2 第7条与 5.2.4 规则2）；文档信息表版本号与文末对齐（原 v1.0 与文末 v1.2 不一致）。v1.4 监理修复（模式二第二轮）：3.1 字段说明表与 DDL 块的 test_model 类型订正为 LONGTEXT（原标 TEXT）、weight 订正为 BIGINT UNSIGNED（原标 BIGINT），对齐 GORM 实际生成；4.2 事务流程图失败分支收敛为"返回触发回滚的失败渠道"（串行执行遇首个失败即回滚，原"全部失败明细"不可实现），并补充"受影响渠道"范围定义与 P2_CHL_001 依赖未具名的阻塞标注。v1.5 监理修复（模式二第三轮）：3.3 logs 涉及列清单订正（Log 表无 action/param 列，action 与 params 经 op 结构内嵌 other 列）；3.1/3.2 DDL 块与字段说明表的 NOT NULL/必填标注按 GORM 实际生成行为订正（仅 key 列与联合主键有 NOT NULL，其余可空列靠应用层保证）；字段长度区间口径统一为闭区间 (0,191]/(0,255]；4.2/5 清理失败处理补"重试一次"（对齐 P2_CHL_001 5.2.5）
