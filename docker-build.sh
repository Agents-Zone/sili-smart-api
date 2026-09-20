#!/usr/bin/env bash
# 打包 sili/sili-smart-api 镜像，并固化交付用 compose。
#
# 版本号规则：当日日期（sili-YYYYMMDD）。
#   - 临时写入 VERSION，供 Dockerfile 经 ldflags 注入二进制的 common.Version；
#   - 用同一值打 image tag，保证「二进制内版本」与「镜像 tag」一致；
#   - 把 TAG 同步到 .env，供本地开发引用；
#   - 脚本退出时还原 VERSION 为空，保持 git 工作区干净。
#
# 交付产物：
#   - docker-compose-clickhouse.yml        从模板固化 image tag 的交付 compose（根目录）
#   - release/sili-smart-api-YYYYMMDD.tar   自建镜像
#
# 模板 docker-compose-clickhouse.template.yml（含 ${TAG:-local} 占位符）入库；
# 固化后的 docker-compose-clickhouse.yml 被 .gitignore 忽略，每次构建重新生成。
#
# 同一天多次构建会覆盖同一 tag；如需区分同日多次构建，把 %Y%m%d 改成 %Y%m%d-%H%M。
set -euo pipefail

TAG="sili-$(date +%Y%m%d)"
TAG_SUFFIX="${TAG#sili-}"          # 20260801，用于 tar 命名
RELEASE_DIR="release"
TEMPLATE="docker-compose-clickhouse.template.yml"
COMPOSE="docker-compose-clickhouse.yml"

# 还原 VERSION（空文件入库），避免 build 产物污染 git 工作区
trap 'printf "" > VERSION' EXIT

echo ">> 写入 VERSION=$TAG"
echo "$TAG" > VERSION

# 本地若已有同 tag 镜像，先删除，避免 build 后旧镜像变 dangling
if docker image inspect "sili/sili-smart-api:${TAG}" >/dev/null 2>&1; then
  echo ">> 本地已有 sili/sili-smart-api:${TAG}，先删除"
  docker rmi "sili/sili-smart-api:${TAG}" >/dev/null 2>&1 \
    || echo "  （删除失败，可能有容器占用；build 后旧镜像将变 dangling）"
fi

echo ">> 构建镜像 sili/sili-smart-api:${TAG}"
docker build -t "sili/sili-smart-api:${TAG}" .

echo ">> 同步 TAG 到 .env（供本地开发引用）"
echo "TAG=${TAG}" > .env

echo ">> 固化交付 compose（${COMPOSE}，image tag 写死为 ${TAG}）"
sed 's|\${TAG:-local}|'"${TAG}"'|g' "${TEMPLATE}" > "${COMPOSE}"

echo ">> 导出镜像到 ${RELEASE_DIR}/"
mkdir -p "$RELEASE_DIR"
TAR_NAME="sili-smart-api-${TAG_SUFFIX}.tar"
docker save "sili/sili-smart-api:${TAG}" -o "${RELEASE_DIR}/${TAR_NAME}"

cat <<EOF
✓ 完成。
  镜像：sili/sili-smart-api:${TAG}（版本号已编进二进制，VERSION 已还原为空）

  交付物：
    ${COMPOSE}                          （image 已固化）
    ${RELEASE_DIR}/${TAR_NAME}          （镜像）
  服务器加载并启动：
    docker load -i ${TAR_NAME}
    docker compose -f docker-compose-clickhouse.yml up -d
EOF
