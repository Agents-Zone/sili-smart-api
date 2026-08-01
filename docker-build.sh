#!/usr/bin/env bash
# 打包 sili-smart-trace 镜像，并产出离线交付包（自包含，无需 .env）。
#
# 版本号规则：当日日期（sili-YYYYMMDD）。
#   - 临时写入 VERSION，供 Dockerfile 经 ldflags 注入二进制的 common.Version；
#   - 用同一值打 image tag，保证「二进制内版本」与「镜像 tag」一致；
#   - 把 TAG 同步到 .env，供根目录模板 compose 本地开发引用；
#   - 脚本退出时还原 VERSION 为空，保持 git 工作区干净。
#
# 离线交付产物（位于 release/）：
#   - sili-smart-trace-YYYYMMDD.tar   自建镜像（load 到服务器）
#   - docker-compose-clickhouse.yml   image tag 已固化的交付 compose（load 后直接 up）
#
# 同一天多次构建会覆盖同一 tag；如需区分同日多次构建，把 %Y%m%d 改成 %Y%m%d-%H%M。
set -euo pipefail

TAG="sili-$(date +%Y%m%d)"
TAG_SUFFIX="${TAG#sili-}"          # 20260801，用于 tar 命名
RELEASE_DIR="release"

# 还原 VERSION（空文件入库），避免 build 产物污染 git 工作区
trap 'printf "" > VERSION' EXIT

echo ">> 写入 VERSION=$TAG"
echo "$TAG" > VERSION

echo ">> 构建镜像 sili-smart-trace:${TAG}"
docker build -t "sili-smart-trace:${TAG}" .

echo ">> 同步 TAG 到 .env（供根目录模板 compose 本地开发引用）"
echo "TAG=${TAG}" > .env

echo ">> 产出离线交付包到 ${RELEASE_DIR}/"
mkdir -p "$RELEASE_DIR"

TAR_NAME="sili-smart-trace-${TAG_SUFFIX}.tar"
echo "  - 导出镜像 ${TAR_NAME}"
docker save "sili-smart-trace:${TAG}" -o "${RELEASE_DIR}/${TAR_NAME}"

echo "  - 固化 compose（image tag 写死为 ${TAG}）"
sed 's|\${TAG:-local}|'"${TAG}"'|g' docker-compose-clickhouse.yml > "${RELEASE_DIR}/docker-compose-clickhouse.yml"

cat <<EOF
✓ 完成。
  镜像：sili-smart-trace:${TAG}（版本号已编进二进制，VERSION 已还原为空）

  离线交付（${RELEASE_DIR}/）：
    ${TAR_NAME}
    docker-compose-clickhouse.yml
  服务器加载并启动：
    docker load -i ${TAR_NAME}
    docker compose -f docker-compose-clickhouse.yml up -d
EOF
