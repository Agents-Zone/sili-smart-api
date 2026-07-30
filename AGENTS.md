# AGENTS.md：new-api 项目约定

只输出必要内容，不发多余评论。

## 概述

这是一个用 Go 构建的 AI API 网关/代理。它在统一 API 之后聚合了 40 多家上游 AI 提供商（OpenAI、Claude、Gemini、Azure、AWS Bedrock 等），并提供用户管理、计费、限流与管理后台。

## 技术栈

- **后端**：Go 1.22+、Gin Web 框架、GORM v2 ORM
- **前端**：React 19、TypeScript、Rsbuild、Base UI、Tailwind CSS
- **数据库**：SQLite、MySQL、PostgreSQL（三者均须支持）
- **缓存**：Redis（go-redis）+ 内存缓存
- **认证**：JWT、WebAuthn/Passkeys、OAuth（GitHub、Discord、OIDC 等）
- **前端包管理器**：Bun（优先于 npm/yarn/pnpm）

## 架构

分层架构：Router -> Controller -> Service -> Model

```
router/        HTTP 路由（API、relay、dashboard、web）
controller/    请求处理器
service/       业务逻辑
model/         数据模型与数据库访问（GORM）
relay/         AI API 中转/代理，含各提供商适配器
  relay/channel/ 提供商专用适配器（openai/、claude/、gemini/、aws/ 等）
middleware/    认证、限流、CORS、日志、分发
setting/       配置管理（ratio、model、operation、system、performance）
common/        通用工具（JSON、加密、Redis、env、rate-limit 等）
dto/           数据传输对象（请求/响应结构体）
constant/      常量（API 类型、渠道类型、context key）
types/         类型定义（中转格式、文件来源、错误）
i18n/          后端国际化（go-i18n，en/zh）
oauth/         OAuth 提供商实现
pkg/           内部包（cachex、ionet）
web/           前端（React 19、Rsbuild、Base UI、Tailwind）
  src/i18n/    前端国际化（i18next，en/zh/zh-TW/fr/ru/ja/vi）
```

## 国际化（i18n）

### 后端（`i18n/`）
- 库：`nicksnyder/go-i18n/v2`
- 语言：en、zh

### 前端（`web/src/i18n/`）
- 库：`i18next` + `react-i18next` + `i18next-browser-languagedetector`
- 语言：en（基准）、zh（回退）、zh-TW、fr、ru、ja、vi
- 翻译文件：`web/src/i18n/locales/{lang}.json`，扁平 JSON，键为英文源字符串
- 用法：`useTranslation()` hook，在组件中调用 `t('English key')`
- CLI 工具：`bun run i18n:sync`（在 `web/` 下执行）

## 规则

### 通用代码质量

- 新代码应直接、可读。优先使用提前返回、清晰分支和命名良好的局部变量，避免深层嵌套或层层叠加的控制流。
- 尽量减少嵌套函数定义。仅在回调 API 要求时，或把闭包留在原地明显比新增一个符号更简单时才使用。
- 避免新增只有一个调用方、且不表达稳定业务概念的包级或模块级辅助函数。把这类逻辑内联到调用处。
- 当函数代表可复用行为、必要的接口/框架回调、导出 API、测试夹具，或值得直接测试的复杂业务逻辑时，独立成函数是合适的。
- 若保留单次使用的辅助函数，其名称必须描述一个持久的领域概念，而非仅为缩短调用方而抽出的机械步骤。

### 后端规则

**relaykit 模块独立性：** `relaykit/` Go 模块必须保持可独立构建。

- `relaykit/` 下的代码严禁导入或依赖根 `new-api` 模块的包，也严禁依赖仅根模块才有的配置、生成文件或 workspace 接线。
- 任何影响 `relaykit/` 或其公开 API 的改动，必须用 `cd relaykit && GOWORK=off go build ./...` 验证；根模块构建成功并不足以证明。

**JSON 包：** 所有 JSON 序列化/反序列化操作必须使用 `common/json.go` 中的包装函数：

- `common.Marshal(v any) ([]byte, error)`
- `common.Unmarshal(data []byte, v any) error`
- `common.UnmarshalJsonStr(data string, v any) error`
- `common.DecodeJson(reader io.Reader, v any) error`
- `common.GetJsonType(data json.RawMessage) string`

业务代码中严禁直接导入或调用 `encoding/json`。`json.RawMessage`、`json.Number` 等 `encoding/json` 的类型定义仍可作为类型引用，但实际的序列化/反序列化调用必须经 `common.*` 完成。

**数据库兼容性：** 所有数据库代码必须同时兼容 SQLite、MySQL >= 5.7.8 与 PostgreSQL >= 9.6。

- 优先使用 GORM 方法（`Create`、`Find`、`Where`、`Updates` 等），而非原生 SQL。
- 主键生成交由 GORM 处理；切勿直接使用 `AUTO_INCREMENT` 或 `SERIAL`。
- 在 `model/` 中用 GORM 查询方法构建的标准 `SELECT ... FOR UPDATE` 行锁，必须使用 `lockForUpdate(tx)`。切勿使用 GORM v1 的旧写法 `tx.Set("gorm:query_option", "FOR UPDATE")`，因为 GORM v2 会静默忽略它，导致加不上锁。切勿在调用处重复写 `clause.Locking{Strength: "UPDATE"}`；共享 helper 对 MySQL/PostgreSQL 发出 `FOR UPDATE`，对不支持该语法的 SQLite 则跳过。语义不同的方言专用锁（如 MySQL 的 next-key/gap lock）仅在显式按数据库类型分支、且每种支持的数据库都有有效回退时，才可使用原生 SQL。
- 当原生 SQL 不可避免时，需考虑方言差异：
  - PostgreSQL 用 `"column"` 引号，MySQL/SQLite 用 `` `column` ``。
  - 对 `group`、`key` 等保留字列，使用 `model/main.go` 中的 `commonGroupCol`、`commonKeyCol`。
  - 布尔值使用 `commonTrueVal`/`commonFalseVal`。
  - 主库分支用 `common.UsingMainDatabase(...)`，日志库分支用 `common.UsingLogDatabase(...)`。
- 未经跨库回退处理，不得使用数据库专用特性，包括 MySQL 专有函数、PostgreSQL 专有操作符、SQLite 不支持的 `ALTER COLUMN`，以及缺少 `TEXT` 回退的数据库专用 JSON 列类型。
- 迁移必须在三种数据库上都生效。对 SQLite，使用 `ALTER TABLE ... ADD COLUMN` 而非 `ALTER COLUMN`（模式参考 `model/main.go`）。
- 当默认值已是代码强制执行的业务规则时，避免使用 `gorm:"default:true"` 这类 GORM 布尔默认 tag。MySQL 与 PostgreSQL 对布尔默认值的规范化方式不同，会导致 GORM `AutoMigrate` 在每次重启时反复发出 `ALTER TABLE`。优先在请求/模型归一化、hook、构造函数或 service 逻辑中设置这些默认值；除非已在 SQLite、MySQL、PostgreSQL 上验证过行为，否则勿将 `default:true` 替换为 `default:1`。

**中转与提供商行为：**

- 实现新渠道时，先确认该提供商是否支持 `StreamOptions`；若支持，将该渠道加入 `streamSupportedChannels`。
- 对于从客户端 JSON 解析后又重新序列化发往上游提供商的请求结构体，可选标量字段必须使用带 `omitempty` 的指针类型（如 `*int`、`*uint`、`*float64`、`*bool`）。
- 在上游中转请求 DTO 中保留显式的零值：客户端 JSON 中缺失的字段须变为 `nil` 并省略；显式的 `0`、`0.0` 或 `false` 则须保持非 `nil` 并发送至上游。
- 可选请求参数避免使用带 `omitempty` 的非指针标量，否则零值会在序列化时被静默丢弃。

**计费表达式系统：** 处理分层/动态计费（基于表达式定价）时，必须先阅读 `pkg/billingexpr/expr.md`。该文档说明了设计理念、表达式语言、完整架构、token 归一化规则、配额换算与表达式版本管理。所有计费表达式改动必须遵循该文档。

**计费安全不变量：** 配额/计费代码绝不能因算术溢出或未校验输入而产生负值扣费（即变相赠送额度）。须做纵深防御：

- 任何会成为计费乘数的用户可控量（图片的 `n`、视频的 `seconds`/`duration`、分辨率/质量比率、批量计数）在进入配额计算前必须设有边界。在请求校验阶段以 400 拒绝越界值。既有边界：图片生成数量用 `dto.MaxImageN`，任务视频时长用 `relaycommon.MaxTaskDurationSeconds`，每种中转格式（OpenAI、Claude、Gemini、Responses）的 `max_tokens` 系列字段用 `relay/helper/valid_request.go` 中的 `maxTokensLimit`。复用这些常量，不要为同一概念另设临时上限。新增中转格式或请求 DTO 时，从第一天起就在其 validator 中为 max-tokens 与计数字段设定边界。
- 留意绕过校验的路径：透传字段（如 `Extra["parameters"]`）、任务 `metadata` map、multipart 表单字段都能携带同样的量，绕过标准 DTO 校验。任何从这类路径读取乘数的适配器，必须在本地强制执行同样的边界（或做 clamp）。
- 从媒体元数据解析出的时长同样受用户/上游控制：音频文件头（转写 token 计数、TTS 响应时长）与上游扣减数（如 Kling `FinalUnitDeduction`）可能给出离谱的值。在它们变为 token 计数前，必须以饱和运算转换。
- 切勿用裸类型转换把已算出的配额或 token 计数转为 `int`，如 `int(float64(quota) * ratio)`、对无界输入做 `int(math.Round(...))`，或 `int(decimal.IntPart())`。所有配额取整/换算集中在 `common/quota_math.go`；使用其中 helper：浮点乘积用 `common.QuotaFromFloat`（截断）、需要四舍五入处用 `common.QuotaRound`（向远离零方向舍入）、decimal 乘积用 `common.QuotaFromDecimal`。`billingexpr.QuotaRound` 委托给 `common.QuotaRound`。不得重新引入本地换算 helper 或裸转换。饱和边界取 int32，因为配额列（user/token/log）在数据库中是 32 位整数；每次 clamp 与 NaN 回退都经 `common.SysError` 记录日志，单个请求本不该接近这些边界。
- 饱和事件亦须审计：每个 helper 都有 `*Checked` 变体（`common.QuotaFromFloatChecked` / `QuotaRoundChecked` / `QuotaFromDecimalChecked`），发生 clamp 时会额外返回一个 `*common.QuotaClamp`。计算扣费的计费路径须将该 clamp 捕获到 `relayInfo.QuotaClamp`（或透传进任务结算），并在写入消费/任务日志前调用 `attachQuotaSaturation`（位于 `service/log_info_generate.go`），把标记嵌套进日志的 `other.admin_info.quota_saturation`，并发出一条与请求关联的 `logger.LogWarn`。嵌套在 `admin_info` 下即天然仅管理员可见（非管理员日志视图会剥离 `admin_info`）。新增计费路径时，使用 `*Checked` 变体并以同样方式暴露 clamp，使异常在管理员日志 UI 与后端日志两处都可审计。
- 乘数 map 经由 `types.PriceData.AddOtherRatio`，该方法会拒绝非正、NaN 与 +Inf 比率。切勿直接写 `PriceData.OtherRatios`，也勿削弱这些防护。
- 预扣费（预扣）与结算（结算/差额）都必须安全：饱和后过大的配额须在预扣阶段以余额不足失败，绝不能静默溢出回绕。新增计费路径（新中转格式、新任务平台、新调整 hook）时，须追踪整条链路：校验 → EstimateBilling/OtherRatios → 配额换算 → 预扣 → 结算/退款，确认每一步都守住这些不变量。
- 解析进无符号类型（`*uint`）的字段会接受超大正 JSON 数（如 `18446744073686646784`，实为负数回绕）；仅 `>= 0` 校验不够，必须设上限。
- 这些不变量的回归测试应放在它们守护的边界处（请求 validator、换算 helper）。参考 `relay/helper/openai_image_request_test.go`、`relay/common/relay_utils_test.go` 与 `common/quota_math_test.go` 的风格。

**后端测试质量：** 后端测试须守护真实行为、API 契约、计费/账务不变量、数据兼容性或回归路径。

- 不添加仅为提升覆盖率、仅证明代码能跑、或在无用户可见或跨模块契约时锁死实现细节的测试。
- 避免基于随机输入、超大循环次数、sleep、时间比较或仅断言日志的伪 fuzz/压力/冒烟/性能测试。
- 避免换名不换实质、覆盖同一分支却无新不变量的重复测试。
- 避免把错误的提供商/协议语义强塞进生产代码的测试。
- 当可观察行为已在别处覆盖时，避免断言私有常量、select 字段列表、helper 内部或文件布局的测试。
- 优先使用输入明确、期望输出精确的确定性表驱动测试。
- 测试需要数据库、请求 context、用户分组、设置或缓存状态时，在该测试夹具内显式初始化这些状态。
- 新增或大幅重写的 Go 后端测试，必须用 `github.com/stretchr/testify/require` 做初始化与致命断言，用 `github.com/stretchr/testify/assert` 做非致命值检查。
- 避免手写断言 helper，除非它编码了一个可复用的项目专有不变量。
- 清理测试时，保留有意义的回归覆盖。若被删测试间接覆盖了真实契约，用更小的测试直接断言该契约来替换。

### 前端规则

- 前端（`web/`）以 `bun` 为首选包管理器与脚本运行器：
  - `bun install` 安装依赖
  - `bun run dev` 启动开发服务器
  - `bun run build` 生产构建
  - `bun run i18n:*` i18n 工具
- 前端 UI 文本必须以 `i18next`/`react-i18next` 支持国际化。使用 `web/src/i18n/locales/{lang}.json` 中的扁平 JSON locale 文件，以英文源字符串为键。
- 在 React 组件中，使用 `useTranslation()` 并对面向用户的文本调用 `t('English key')`。
- 详细前端规范见 `web/AGENTS.md`，涵盖 TypeScript、组件结构、样式、可访问性、测试与构建检查。

## 本地启动

```bash
cd web && bun install && bun run build && cd ..
go run main.go     # http://localhost:3000，默认 SQLite
```

前后端分离开发：

```bash
go run main.go                        # 后端 :3000
```
```bash
cd web && bun install && bun run dev  # 前端 :8080，/api /mj /pg 代理到 :3000
```
后端端口用 `PORT=` 或 `-port` 覆盖；前端指向其它后端用 `VITE_REACT_APP_SERVER_URL`。
