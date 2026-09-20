# scripts/ 使用手册

本目录是日志存量迁移工具，用于把 `logs` 表从 PostgreSQL 主库搬到 ClickHouse 日志库。配套仓库根目录的 `docker-compose-clickhouse.yml`，对应其中的数据分工：主库 PostgreSQL 存业务数据，日志库 ClickHouse 存 `logs` 与 `conversation_turns`。

迁移是为「日志库从 PG 切到 ClickHouse」这一变更服务的。新应用实例切到 ClickHouse 后会持续写入新日志，本工具负责把 PG 里的存量历史日志搬过去，全程不要求停服。

## 文件清单

| 文件 | 类型 | 作用 |
|------|------|------|
| `migrate_logs_pg_to_clickhouse.py` | Python 3 | 迁移本体。读 PG、写 ClickHouse，按 id 分批推进，支持断点续传、校验、dry-run。 |
| `run_migrate_in_docker.sh` | Bash | 在 `python:3.10` 容器里跑上面的脚本。规避宿主机 Python 版本过低、PG 端口未映射两个问题，是推荐入口。 |
| `.migrate.env` | 配置 | 本地连接配置，含数据库密码。已被 `.gitignore` 忽略，不进仓库，手册里给出的模板用于自行填写。 |

## 前置条件

迁移走的是 Docker 方式，需要：

1. 机器上装有 `docker`。
2. `docker-compose-clickhouse.yml` 已经启动，`postgres` 与 `clickhouse` 两个服务都在 `sili-network` 内可用。脚本会自动按服务名（`postgres` / `clickhouse`）寻找网络并直连两端，数据全程在 compose 内网流动，不落地宿主机。

如果 `sili-network` 不存在，脚本会退出并提示先启动 compose：

```bash
docker compose -f docker-compose-clickhouse.yml up -d
```

## 快速上手（Docker 方式，推荐）

所有命令都在仓库根目录执行。

第一步，准备本地连接配置。复制下面的模板，填上你实际的数据库密码，存为 `scripts/.migrate.env`：

```sh
PG_DSN='postgresql://用户名:密码@postgres:5432/new-api'
CH_HOST='clickhouse'
CH_PORT='8123'
CH_DB='new_api_logs'
CH_USER='default'
CH_PASSWORD='你的 ClickHouse 密码'
```

`run_migrate_in_docker.sh` 启动时会自动加载这个文件，优先级高于脚本默认值。密码没改过时，可以省略此文件，直接用脚本内置的默认值（与 `docker-compose-clickhouse.template.yml` 一致）。

第二步，先 dry-run 看存量规模，不写任何数据：

```bash
bash scripts/run_migrate_in_docker.sh --dry-run
```

第三步，正式迁移。按 id 分批推进，可重复运行，中断后重跑会自动续上：

```bash
bash scripts/run_migrate_in_docker.sh
```

第四步，校验两边行数：

```bash
bash scripts/run_migrate_in_docker.sh --verify
```

`run_migrate_in_docker.sh` 会把命令行参数原样透传给 Python 脚本，因此上面用到的 `--dry-run`、`--verify` 以及后面参数表里的所有参数都能直接用。

## 环境变量参考

`.migrate.env` 写的就是这些键，传给容器内的迁移脚本。

| 变量 | 必填 | 默认值 | 说明 |
|------|------|--------|------|
| `PG_DSN` | 是 | 无 | PostgreSQL 连接串，示例 `postgresql://root:pass@postgres:5432/new-api`。Docker 方式下主机填 compose 服务名 `postgres`。 |
| `CH_HOST` | 否 | `localhost`（脚本默认）/ `clickhouse`（Docker 脚本覆盖） | ClickHouse 主机。 |
| `CH_PORT` | 否 | `8123` | ClickHouse 端口，默认走 HTTP。 |
| `CH_DB` | 否 | `new_api_logs` | ClickHouse 库名。 |
| `CH_USER` | 否 | `default` | ClickHouse 用户。 |
| `CH_PASSWORD` | 否 | 空 | ClickHouse 密码。 |
| `CH_NATIVE` | 否 | 未设 | 设为 `1` 走原生协议，端口随之改用 `9000`。 |
| `CH_SECURE` | 否 | 未设 | 设为 `1` 启用 TLS。 |

Docker 方式下，`CH_HOST` 默认被覆盖为 `clickhouse`，`PG_DSN` 的主机默认走 `postgres`，对应 compose 内网服务名。

## 命令行参数参考

透传给 `migrate_logs_pg_to_clickhouse.py` 的参数。

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `--table` | `logs` | 源表与目标表名，两端同名。 |
| `--batch-size` | `100000` | 每批从 PG 拉取并写入 CH 的行数。 |
| `--start-id` | `0` | 强制从指定 id 开始，覆盖 checkpoint。排查问题时偶尔用。 |
| `--checkpoint` | `.logs_migration_checkpoint.json` | 断点文件路径。注意 Docker 方式下不持久，见下文「迁移机制」。 |
| `--no-checkpoint` | 关 | 不读写断点文件。 |
| `--no-create-table` | 关 | 不在 ClickHouse 自动建表。 |
| `--dry-run` | 关 | 只看 PG 存量规模，不写数据。 |
| `--verify` | 关 | 校验 PG 与 ClickHouse 的行数。 |

## 迁移机制

理解下面几点，可以安全地中断、重跑、与业务切换并行。

水位推进。迁移以 PG 自增 id 为水位，每次取 `id > 上次水位` 的一批，写完后水位推进到本批最大 id。起始水位按优先级取最大值：显式 `--start-id` > 断点文件记录的 `last_id` > ClickHouse 侧 `max(id) WHERE id > 0`。最后这一项意味着即使断点文件丢失，重跑也不会重复迁已经搬过去的行，会自动从 CH 已有的最大 id 之后接着跑。

断点续传。每写完一批，把最新 id 落到 `--checkpoint` 指定的文件。Docker 方式下容器用 `--rm` 运行，且只把 Python 脚本以只读方式挂进去，断点文件写在容器内，容器结束即销毁。因此 Docker 方式实际依赖上面说的 ClickHouse 侧 `max(id)` 来续传，不再依赖断点文件。需要文件断点时，用「本地直接运行」方式并把断点文件留在宿主机。

id=0 隔离。应用切到 ClickHouse 后新写的日志 id 恒为 0（ClickHouse 没有自增主键，应用层不回填 id）。迁移搬过来的老数据 id 都大于 0，二者天然不冲突。迁移进行期间应用可以正常切换并持续写日志，互不影响。`--verify` 与 `max(id)` 推断也都只统计 `id > 0` 的老数据，不被新日志干扰。

自动建表。默认会在 ClickHouse 执行 `CREATE TABLE IF NOT EXISTS`，结构与 `model/main.go` 里的 `clickHouseLogCreateTableSQL` 一致（无 TTL 版本）。应用配置了 TTL 并重启后，会自行对该表 `MODIFY TTL`，二者不冲突。不想让脚本建表时加 `--no-create-table`。

行数校验。`--verify` 比对 PG 总行数与 ClickHouse 侧 `max(id)`（仅 `id > 0`）。在 PG 自增 id 连续无空洞的前提下，二者相等表示迁完。脚本会对差值给出提示：差行为正说明还需续迁，重跑即可；CH 多于 PG 通常意味着 checkpoint 被重置后整段重跑过一次，需人工确认。

## 本地直接运行（无 Docker）

宿主机有 Python 3.8 以上、且能同时连通 PG 与 ClickHouse 时，可以跳过 Docker 直接跑：

```bash
pip install psycopg2-binary clickhouse-connect

export PG_DSN='postgresql://root:pass@localhost:5432/new-api'
export CH_HOST=localhost CH_PORT=8123 CH_DB=new_api_logs CH_USER=default CH_PASSWORD='pass'

python3 scripts/migrate_logs_pg_to_clickhouse.py --dry-run
```

注意两点。其一，`docker-compose-clickhouse.yml` 默认没有把 PG 的 5432 映射到宿主机，本地直连需要先在 compose 里加端口映射，或通过容器内的服务名访问。其二，本地方式断点文件会留在宿主机，跨次运行可复用。

## 故障排查

找不到 `sili-network`。说明 compose 没启动，或网络名不含 `sili-network`。先 `docker compose -f docker-compose-clickhouse.yml up -d`，再重跑。

连不上 PG 或 ClickHouse。优先核对 `.migrate.env` 里的主机与密码。Docker 方式下主机必须填服务名（`postgres` / `clickhouse`），不能填 `localhost`。密码改过时，默认值会失效，需在 `.migrate.env` 覆盖。

报 `Unrecognized column` 之类列名错误。通常是手动改了列引用方式。脚本里 ClickHouse 列传裸列名（库会自动加反引号），PG 的保留字列 `group` 用双引号，二者不要混用。

`pip install` 慢或失败。容器内已默认走清华源。本地直连时可手动加 `-i https://pypi.tuna.tsinghua.edu.cn/simple`。宿主机 Python 3.6 及以下装不上这两个依赖的预编译 wheel，这也是 Docker 方式存在的主要原因，建议直接用 Docker 方式。

迁完 `--verify` 仍有差值。先重跑一次迁移，让它按 `max(id)` 续传。仍不对再检查 PG 是否有 id 空洞（历史删行）导致 `count(*)` 与 `max(id)` 本身不等，这种情况下 `--verify` 的近似比对会持续偏差，需要按 id 范围另做精确核对。

## 安全提示

`.migrate.env` 含数据库真实密码。仓库 `.gitignore` 已忽略 `scripts/.migrate.env`，保持忽略状态，不要提交、不要外传、不要把真实密码写进本手册或其他文档。生产环境的默认密码（见 `docker-compose-clickhouse.template.yml`）必须在部署时全部改掉。
