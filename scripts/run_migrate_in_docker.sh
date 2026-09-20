#!/usr/bin/env bash
#
# 在 python3.10 容器里跑 logs 迁移（PostgreSQL -> ClickHouse）。
#
# 为什么需要它：
#   - 宿主机 python 常为 3.6，psycopg2-binary / clickhouse-connect 已无 cp36 wheel，
#     装不上；3.6 也早已 EOL。
#   - docker-compose-clickhouse.yml 里 postgres 没把 5432 映射到宿主，宿主机直连
#     不到 PG，只能在 compose 内网用服务名访问。
#   - 用一个临时 python:3.10 容器接入 sili-network，用 postgres / clickhouse 服务名
#     直连两端，依赖全是预编译 wheel。数据全程在网络里流动，不落地。
#
# 用法（在项目根目录）：
#   bash scripts/run_migrate_in_docker.sh --dry-run     # 先看存量规模，不写数据
#   bash scripts/run_migrate_in_docker.sh               # 正式迁移，断点续传，可重复跑
#   bash scripts/run_migrate_in_docker.sh --verify      # 校验 PG 与 ClickHouse 行数
#
# 连接配置默认与 docker-compose-clickhouse.yml 一致；改过密码时用环境变量覆盖：
#   PG_DSN=postgresql://root:新密码@postgres:5432/new-api \
#   CH_PASSWORD=新密码 \
#   bash scripts/run_migrate_in_docker.sh --dry-run

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
MIGRATE_PY="$SCRIPT_DIR/migrate_logs_pg_to_clickhouse.py"

if [ ! -f "$MIGRATE_PY" ]; then
  echo "找不到迁移脚本: $MIGRATE_PY" >&2
  exit 1
fi

if ! command -v docker >/dev/null 2>&1; then
  echo "这台机器没有 docker。本脚本依赖 docker 运行 python3.10 容器。" >&2
  echo "没有 docker 的话，需在宿主机装 python38+ 并把 PG 端口映射出来，另行处理。" >&2
  exit 1
fi

# 找 compose 网络（形如 <project>_sili-network）
NET="$(docker network ls --format '{{.Name}}' | grep -F 'sili-network' | head -1 || true)"
if [ -z "$NET" ]; then
  echo "找不到 sili-network，请先启动 compose：" >&2
  echo "  docker compose -f docker-compose-clickhouse.yml up -d" >&2
  exit 1
fi
echo "使用网络: $NET"

# 本地连接配置（含密码），不提交 git；存在则自动加载，优先级高于默认值
if [ -f "$SCRIPT_DIR/.migrate.env" ]; then
  set -a
  . "$SCRIPT_DIR/.migrate.env"
  set +a
fi

PG_DSN="${PG_DSN:-postgresql://root:123456@postgres:5432/new-api}"
CH_HOST="${CH_HOST:-clickhouse}"
CH_PORT="${CH_PORT:-8123}"
CH_DB="${CH_DB:-new_api_logs}"
CH_USER="${CH_USER:-default}"
CH_PASSWORD="${CH_PASSWORD:-123456}"

# 参数透传给迁移脚本：bash -c '...' _ "$@" 里的 _ 占位 $0，"$@" 是透传的参数。
docker run --rm -t \
  --network "$NET" \
  -v "$MIGRATE_PY:/work/migrate.py:ro" \
  -w /work \
  -e PG_DSN="$PG_DSN" \
  -e CH_HOST="$CH_HOST" \
  -e CH_PORT="$CH_PORT" \
  -e CH_DB="$CH_DB" \
  -e CH_USER="$CH_USER" \
  -e CH_PASSWORD="$CH_PASSWORD" \
  python:3.10-slim \
  bash -c 'echo "[1/2] 安装依赖（清华源加速）..." && pip install -q -i https://pypi.tuna.tsinghua.edu.cn/simple psycopg2-binary clickhouse-connect && echo "[2/2] 依赖就绪，开始执行迁移脚本..." && exec python -u migrate.py "$@"' _ "$@"
