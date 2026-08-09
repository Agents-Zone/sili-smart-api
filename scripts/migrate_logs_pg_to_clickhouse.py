#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
把 logs 表从 PostgreSQL 迁移到 ClickHouse。

适用场景：new-api / sili-smart-api 日志库从主库（PostgreSQL）切换到 ClickHouse，
把存量 logs 记录搬到 ClickHouse。日志库分工参考 docker-compose-clickhouse.yml：
主库存业务数据，ClickHouse 存 logs 与 conversation_turns。

依赖：
    pip install psycopg2-binary clickhouse-connect

连接配置（环境变量）：
    PG_DSN        主库连接串，必填，如 postgresql://root:123456@localhost:5432/new-api
    CH_HOST       ClickHouse 主机，默认 localhost
    CH_PORT       ClickHouse 端口，默认 8123（HTTP）
    CH_DB         ClickHouse 库名，默认 new_api_logs
    CH_USER       ClickHouse 用户，默认 default
    CH_PASSWORD   ClickHouse 密码，默认空
    CH_NATIVE     设为 1 走原生协议，端口改 9000
    CH_SECURE     设为 1 启用 TLS

用法：
    export PG_DSN='postgresql://root:123456@localhost:5432/new-api'
    export CH_HOST=localhost CH_PORT=8123 CH_DB=new_api_logs CH_USER=default CH_PASSWORD=123456

    # 1) 先看存量规模（不写任何数据）
    python3 scripts/migrate_logs_pg_to_clickhouse.py --dry-run

    # 2) 正式迁移（按 id 分批，自动断点续传，可重复运行）
    python3 scripts/migrate_logs_pg_to_clickhouse.py

    # 3) 校验两边行数
    python3 scripts/migrate_logs_pg_to_clickhouse.py --verify

行为说明：
    - 默认自动在 ClickHouse 创建 logs 表（IF NOT EXISTS，结构与 model/main.go 的
      clickHouseLogCreateTableSQL 一致，无 TTL；应用配置 TTL 后重启会自动 MODIFY TTL）。
    - 以 PG 自增 id 为水位，按 id 分批推进。断点写 checkpoint 文件，默认自动续跑。
    - 水位同时从 ClickHouse 侧推断（max(id) WHERE id > 0），checkpoint 丢失也不会重复迁。
    - 应用切到 ClickHouse 后新写的日志 id 恒为 0，与迁移过来的老数据（id > 0）天然隔离，
      互不冲突。迁移全程可正常切换，无需停服。
"""

import argparse
import json
import os

# ClickHouse 建表列，与 model/main.go 的 clickHouseLogCreateTableSQL 一一对应。
# `group` 是保留字，但 clickhouse_connect 的 column_names 须传裸列名，库会自动加反引号引用；
# 若传 "`group`"，insert 前的列校验会把它当字面字符串，报 Unrecognized column。
CH_COLUMNS = [
    "id", "user_id", "created_at", "type", "content", "username",
    "token_name", "model_name", "quota", "prompt_tokens", "completion_tokens",
    "use_time", "is_stream", "channel_id", "token_id", "group",
    "ip", "request_id", "upstream_request_id", "other",
]

# 对应 PostgreSQL 列。group 在 PG 也是保留字，须用双引号。
PG_COLUMNS = [
    "id", "user_id", "created_at", "type", "content", "username",
    "token_name", "model_name", "quota", "prompt_tokens", "completion_tokens",
    "use_time", "is_stream", "channel_id", "token_id", '"group"',
    "ip", "request_id", "upstream_request_id", "other",
]

# 数值列：NULL 归 0；其余字符串列 NULL 归空串。is_stream 单独处理（bool -> 0/1）。
NUM_COLS = {
    "id", "user_id", "created_at", "type", "quota", "prompt_tokens",
    "completion_tokens", "use_time", "channel_id", "token_id",
}

# 与 clickHouseLogCreateTableSQL(0) 一致，无 TTL 版本。
CREATE_TABLE_SQL = """
CREATE TABLE IF NOT EXISTS logs (
    id Int64 DEFAULT 0,
    user_id Int32 DEFAULT 0,
    created_at Int64 DEFAULT 0,
    type Int32 DEFAULT 0,
    content String DEFAULT '',
    username String DEFAULT '',
    token_name String DEFAULT '',
    model_name String DEFAULT '',
    quota Int32 DEFAULT 0,
    prompt_tokens Int32 DEFAULT 0,
    completion_tokens Int32 DEFAULT 0,
    use_time Int32 DEFAULT 0,
    is_stream UInt8 DEFAULT 0,
    channel_id Int32 DEFAULT 0,
    token_id Int32 DEFAULT 0,
    `group` String DEFAULT '',
    ip String DEFAULT '',
    request_id String DEFAULT '',
    upstream_request_id String DEFAULT '',
    other String DEFAULT ''
)
ENGINE = MergeTree()
PARTITION BY toYYYYMM(toDateTime(created_at))
ORDER BY (created_at, request_id)
"""


def env_bool(name):
    return os.environ.get(name, "").strip().lower() in ("1", "true", "yes")


def get_ch_client():
    import clickhouse_connect

    native = env_bool("CH_NATIVE")
    return clickhouse_connect.get_client(
        host=os.environ.get("CH_HOST", "localhost"),
        port=int(os.environ.get("CH_PORT", "9000" if native else "8123")),
        username=os.environ.get("CH_USER", "default"),
        password=os.environ.get("CH_PASSWORD", ""),
        database=os.environ.get("CH_DB", "new_api_logs"),
        interface="native" if native else "http",
        secure=env_bool("CH_SECURE"),
    )


def get_pg_conn():
    import psycopg2

    dsn = os.environ.get("PG_DSN")
    if not dsn:
        raise SystemExit("缺少 PG_DSN 环境变量，示例：export PG_DSN='postgresql://root:123456@localhost:5432/new-api'")
    return psycopg2.connect(dsn)


def pg_total(conn, table):
    with conn.cursor() as cur:
        cur.execute(f'SELECT count(*) FROM "{table}"')
        return cur.fetchone()[0]


def pg_min_max_id(conn, table):
    with conn.cursor() as cur:
        cur.execute(f'SELECT COALESCE(min(id), 0), COALESCE(max(id), 0) FROM "{table}"')
        return cur.fetchone()


def pg_fetch_batch(conn, table, after_id, limit):
    cols = ", ".join(PG_COLUMNS)
    with conn.cursor() as cur:
        cur.execute(
            f'SELECT {cols} FROM "{table}" WHERE id > %s ORDER BY id LIMIT %s',
            (after_id, limit),
        )
        return cur.fetchall()


def normalize_row(row):
    out = []
    for col, val in zip(PG_COLUMNS, row):
        if val is None:
            out.append(0 if col in NUM_COLS else "")
        elif col == "is_stream":
            out.append(1 if val else 0)
        elif col in NUM_COLS:
            out.append(int(val))
        else:
            out.append(str(val))
    return out


def ch_table_exists(ch, table):
    rows = ch.query(f"SELECT count() FROM system.tables WHERE database = currentDatabase() AND name = '{table}'")
    return rows.result_rows[0][0] > 0


def ch_max_migrated_id(ch, table):
    try:
        rows = ch.query(f"SELECT max(id) FROM {table} WHERE id > 0")
        return int(rows.result_rows[0][0] or 0)
    except Exception:
        return 0  # 表不存在或为空


def read_checkpoint(path):
    try:
        with open(path, encoding="utf-8") as f:
            return json.load(f)
    except (OSError, ValueError):
        return {}


def write_checkpoint(path, last_id):
    tmp = path + ".tmp"
    with open(tmp, "w", encoding="utf-8") as f:
        json.dump({"last_id": last_id}, f)
    os.replace(tmp, path)


def do_dry_run(pg, table, batch_size):
    total = pg_total(pg, table)
    min_id, max_id = pg_min_max_id(pg, table)
    print(f"PG {table} 总行数: {total}")
    print(f"id 范围: {min_id} ~ {max_id}")
    if total:
        print(f"按 batch-size={batch_size} 约需 {max(1, -(-(max_id - min_id) // batch_size))} 批")
    print("dry-run 结束，未写入任何数据。")


def run_migration(pg, ch, table, batch_size, start_id, checkpoint, no_checkpoint):
    total = pg_total(pg, table)
    min_id, max_id = pg_min_max_id(pg, table)
    print(f"PG {table} 总行数: {total}, id 范围 {min_id} ~ {max_id}")

    if total == 0:
        print("PG logs 表为空，无需迁移。")
        return

    # 起始水位：显式 --start-id > checkpoint > ClickHouse 侧已迁最大 id
    watermark = start_id
    if not no_checkpoint:
        watermark = max(watermark, read_checkpoint(checkpoint).get("last_id", 0))
    watermark = max(watermark, ch_max_migrated_id(ch, table))
    print(f"起始水位 id: {watermark}")

    migrated = 0
    last_id = watermark - 1
    while True:
        rows = pg_fetch_batch(pg, table, last_id, batch_size)
        if not rows:
            break
        ch.insert(table, [normalize_row(r) for r in rows], column_names=CH_COLUMNS)
        last_id = rows[-1][0]
        migrated += len(rows)
        print(f"已迁移 {migrated} 行（最新 id={last_id}, {migrated}/{total}）", flush=True)
        if not no_checkpoint:
            write_checkpoint(checkpoint, last_id)
        if len(rows) < batch_size:
            break

    print(f"迁移完成，共写入 {migrated} 行到 ClickHouse {table} 表。")


def do_verify(pg, ch, table):
    total = pg_total(pg, table)
    ch_count = ch_max_migrated_id(ch, table)
    # ch_count 统计的是 ClickHouse 里 id > 0 的行数（即迁移过来的老数据）。
    # 应用切到 ClickHouse 后新写日志 id=0，不参与统计。
    print(f"PG {table} 总行数:          {total}")
    print(f"ClickHouse 已迁移行数:     {ch_count}")
    if ch_count == total:
        print("一致。")
    elif ch_count < total:
        print(f"还差 {total - ch_count} 行，重新运行脚本即可续迁。")
    else:
        print(f"ClickHouse 比 PG 多 {ch_count - total} 行，可能存在重复迁移（例如 checkpoint 被重置后重跑）。")


def main():
    parser = argparse.ArgumentParser(
        description="迁移 logs 表：PostgreSQL -> ClickHouse",
        formatter_class=argparse.ArgumentDefaultsHelpFormatter,
    )
    parser.add_argument("--table", default="logs", help="源/目标表名")
    parser.add_argument("--batch-size", type=int, default=100000, help="每批行数")
    parser.add_argument("--start-id", type=int, default=0, help="强制从指定 id 开始（覆盖 checkpoint）")
    parser.add_argument("--checkpoint", default=".logs_migration_checkpoint.json", help="断点文件路径")
    parser.add_argument("--no-checkpoint", action="store_true", help="不写断点文件")
    parser.add_argument("--no-create-table", action="store_true", help="不在 ClickHouse 自动建表")
    parser.add_argument("--dry-run", action="store_true", help="只看规模，不写数据")
    parser.add_argument("--verify", action="store_true", help="校验 PG 与 ClickHouse 行数")
    args = parser.parse_args()

    pg = get_pg_conn()
    try:
        if args.dry_run:
            do_dry_run(pg, args.table, args.batch_size)
            return

        ch = get_ch_client()
        if args.verify:
            do_verify(pg, ch, args.table)
            return

        if not args.no_create_table:
            ch.command(CREATE_TABLE_SQL)
            print(f"已确认 ClickHouse {args.table} 表存在。")

        run_migration(pg, ch, args.table, args.batch_size,
                      args.start_id, args.checkpoint, args.no_checkpoint)
    finally:
        pg.close()


if __name__ == "__main__":
    main()
