#!/usr/bin/env bash
# ========================================================================
# NeuToken 项目迁移 · 部署阶段（在目标 Mac 上跑）
# ========================================================================
# 前提: 已经把 pack.sh 生成的 tar.gz 解压到目标 Mac 的某个目录
# 用法: 在解压后的目录里跑
#       bash source/migrate/deploy.sh
# ========================================================================
set -e

PACKAGE_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
# 部署目标路径 — 可以通过 NEWAPI_TARGET 环境变量自定义，默认 ~/Downloads/newapi
# 例: NEWAPI_TARGET=~/work/newapi bash deploy.sh
TARGET_REPO="${NEWAPI_TARGET:-${HOME}/Downloads/newapi}"
IMAGE_NAME="newapi-hardened:latest"

# ─── 颜色 + helper ──────────────────────────────────────────────────
RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; BLUE='\033[0;34m'; NC='\033[0m'
ok()    { echo -e "${GREEN}✓${NC} $1"; }
warn()  { echo -e "${YELLOW}⚠${NC} $1"; }
fail()  { echo -e "${RED}✗${NC} $1"; exit 1; }
info()  { echo -e "${BLUE}ℹ${NC} $1"; }
phase() { echo ""; echo "═════════════════════════════════════════════════════════════"; echo "  $1"; echo "═════════════════════════════════════════════════════════════"; }
ask_continue() {
  echo ""
  read -p "  按 Enter 继续，Ctrl+C 中止 ... " _
}

# ============================================================
phase "阶段 1/9 :: 目标 Mac 环境预检"
# ============================================================

# OS 版本
OS_VER=$(sw_vers -productVersion)
info "macOS 版本: $OS_VER"

# CPU 架构（M1 Pro = arm64）
ARCH=$(uname -m)
[[ "$ARCH" == "arm64" ]] && ok "CPU 架构: arm64 (Apple Silicon)" || warn "CPU 架构: $ARCH (非 arm64，需确认源镜像兼容)"

# 内存
MEM_GB=$(sysctl -n hw.memsize | awk '{print int($1/1024/1024/1024)}')
[[ "$MEM_GB" -ge 16 ]] && ok "物理内存: ${MEM_GB}GB" || warn "物理内存 ${MEM_GB}GB，建议 ≥ 16GB"

# Docker 安装 + 运行
command -v docker >/dev/null 2>&1 || fail "docker 命令不存在 — 请先安装 Docker Desktop for Mac (Apple Silicon 版): https://www.docker.com/products/docker-desktop"
ok "docker 命令可用 ($(docker --version | head -1))"

docker ps >/dev/null 2>&1 || fail "Docker daemon 未运行 — 打开 Docker Desktop 应用并等它启动到绿灯"
ok "Docker daemon 运行中"

# Docker Desktop 内存配额（不易直接检测，给提醒）
warn "请确认 Docker Desktop → Settings → Resources → Memory ≥ 8 GB"
warn "（避免之后 build 时 OOM。M1 16GB 推荐 8GB 给 Docker）"

# 端口 3000 是否被占用
if lsof -nP -iTCP:3000 -sTCP:LISTEN 2>/dev/null | grep -q LISTEN; then
  warn "端口 3000 已被占用 — 部署后 newapi 容器可能起不来"
  lsof -nP -iTCP:3000 -sTCP:LISTEN
else
  ok "端口 3000 空闲"
fi

# 可用磁盘空间
FREE_GB=$(df -g / | awk 'NR==2 {print $4}')
[[ "$FREE_GB" -ge 5 ]] || fail "可用磁盘 ${FREE_GB}GB 不足，需要 ≥ 5GB"
ok "可用磁盘 ${FREE_GB}GB"

ask_continue

# ============================================================
phase "阶段 2/9 :: 验证迁移包完整性"
# ============================================================

cd "$PACKAGE_ROOT" || fail "找不到迁移包根目录"
info "迁移包根目录: $PACKAGE_ROOT"

# 必备文件
test -f MANIFEST.txt || fail "MANIFEST.txt 不存在 — 迁移包损坏"
test -f MANIFEST.sha256 || fail "MANIFEST.sha256 不存在"
test -d source || fail "source/ 目录不存在"
test -d db || fail "db/ 目录不存在"
test -d image || fail "image/ 目录不存在"
test -d config || fail "config/ 目录不存在"
test -f config/.env || fail "config/.env 不存在 — CRYPTO_SECRET 无处可寻"
test -f db/new-api.dump || fail "db/new-api.dump 不存在"
test -f image/newapi-hardened.tar.gz || fail "image/newapi-hardened.tar.gz 不存在"
ok "所有必备文件齐全"

echo ""
echo "[MANIFEST.txt 内容预览]"
head -20 MANIFEST.txt
echo ""

# 校验和检查（可能耗时 30 秒）
info "正在校验文件完整性... (约 30 秒)"
if shasum -a 256 -c MANIFEST.sha256 --status; then
  ok "所有文件校验和匹配，迁移包完整"
else
  warn "部分文件校验失败 — 可能传输中损坏。下面是详细对比："
  shasum -a 256 -c MANIFEST.sha256 | grep -v ': OK' | head -10
  ask_continue
fi

# ============================================================
phase "阶段 3/9 :: 部署源码到 ~/work/newapi/"
# ============================================================

if [[ -d "$TARGET_REPO" ]]; then
  warn "$TARGET_REPO 已存在 — 将备份现有内容到 ${TARGET_REPO}.bak-$(date +%H%M%S)"
  ask_continue
  mv "$TARGET_REPO" "${TARGET_REPO}.bak-$(date +%H%M%S)"
fi

mkdir -p "$(dirname "$TARGET_REPO")"
rsync -a "$PACKAGE_ROOT/source/" "$TARGET_REPO/"
ok "源码已部署到 $TARGET_REPO"

# 把配置文件搬回去
cp "$PACKAGE_ROOT/config/.env" "$TARGET_REPO/.env"
cp "$PACKAGE_ROOT/config/VERSION" "$TARGET_REPO/VERSION" 2>/dev/null || true
cp "$PACKAGE_ROOT/config/docker-compose.yml" "$TARGET_REPO/docker-compose.yml"
ok ".env + VERSION + docker-compose.yml 已就位"

# 创建 docker compose 期望的目录
mkdir -p "$TARGET_REPO/data" "$TARGET_REPO/logs"
ok "data/ + logs/ 目录创建"

ask_continue

# ============================================================
phase "阶段 4/9 :: 加载 Docker 镜像 (newapi-hardened)"
# ============================================================

if docker images "$IMAGE_NAME" --format '{{.ID}}' | head -1 | grep -q .; then
  warn "$IMAGE_NAME 镜像已存在，将覆盖"
  docker rmi -f "$IMAGE_NAME" 2>&1 | tail -2 || true
fi

info "加载镜像中... (约 1-2 分钟)"
gunzip -c "$PACKAGE_ROOT/image/newapi-hardened.tar.gz" | docker load 2>&1 | tail -3

docker images "$IMAGE_NAME" --format '{{.ID}}' | head -1 | grep -q . \
  || fail "镜像加载失败 — 文件可能损坏"
IMG_SIZE=$(docker images "$IMAGE_NAME" --format '{{.Size}}')
ok "镜像加载完成: $IMAGE_NAME ($IMG_SIZE)"

# 验证镜像里有关键字符串（确保不是空壳）
HIT=$(docker run --rm --entrypoint sh "$IMAGE_NAME" -c "grep -ac 'Prompt 历史\|prompt_archive' /new-api" 2>/dev/null || echo 0)
[[ "${HIT:-0}" -ge "1" ]] && ok "镜像含 prompt_archive 模块（命中 $HIT 次）" || warn "镜像里没找到 prompt_archive 字符串（命中 0）"

# ============================================================
phase "阶段 5/9 :: 加载 postgres + redis base 镜像"
# ============================================================

# 优先用本地打包的 tar（适用于无网或网络受限的目标机）
if [[ -f "$PACKAGE_ROOT/image/base-images.tar.gz" ]]; then
  info "从迁移包加载 base 镜像（无需网络）..."
  gunzip -c "$PACKAGE_ROOT/image/base-images.tar.gz" | docker load 2>&1 | tail -4
  docker images postgres:15-alpine --format '{{.ID}}' | grep -q . || fail "postgres 镜像加载失败"
  docker images redis:7-alpine --format '{{.ID}}' | grep -q . || fail "redis 镜像加载失败"
  ok "base 镜像从本地包加载成功（postgres + redis）"
else
  info "迁移包没有 base 镜像 tar，尝试从 Docker Hub 拉取..."
  docker pull postgres:15-alpine 2>&1 | tail -2 || fail "postgres 镜像拉取失败 — 检查网络或配置 Docker mirror"
  docker pull redis:7-alpine 2>&1 | tail -2 || fail "redis 镜像拉取失败"
  ok "基础镜像从 Docker Hub 就绪"
fi

# ============================================================
phase "阶段 6/9 :: 启动 postgres + redis"
# ============================================================

cd "$TARGET_REPO"
docker compose up -d postgres redis 2>&1 | tail -5

# 等 postgres healthy（M1 首次启动 + 大 volume 还原可能慢，延长到 3 分钟）
info "等 postgres 健康检查通过... (最多 180 秒)"
for i in $(seq 1 36); do
  STATE=$(docker inspect postgres --format='{{.State.Status}}' 2>/dev/null || echo "absent")
  HEALTH=$(docker inspect postgres --format='{{.State.Health.Status}}' 2>/dev/null || echo "absent")
  echo "  $((i*5))s: state=$STATE health=$HEALTH"
  if [[ "$STATE" == "exited" ]]; then
    warn "postgres 容器已退出，dump 最近 30 行日志诊断："
    docker logs postgres --tail 30
    fail "postgres 启动失败"
  fi
  if [[ "$HEALTH" == "healthy" ]]; then break; fi
  sleep 5
done
if [[ "$HEALTH" != "healthy" ]]; then
  warn "180s 内未 healthy，dump 日志诊断："
  docker logs postgres --tail 50
  echo ""
  echo "常见 3 种问题："
  echo "  1) volume 残留：docker compose down -v + docker volume rm newapi_pg_data"
  echo "  2) .env 未加载：检查 DB_PASSWORD 是否真传到容器"
  echo "  3) M1 首次启动慢：再多等 1-2 分钟手动 docker inspect postgres"
  fail "postgres 健康检查超时"
fi
ok "postgres 已就绪"

# ============================================================
phase "阶段 7/9 :: 还原 PostgreSQL 数据"
# ============================================================

DB_PASS=$(grep '^DB_PASSWORD=' "$TARGET_REPO/.env" | cut -d= -f2-)

# 检查 db 是否已有 new-api 库（pg image 初始化时创建的空库）
DB_EXISTS=$(docker exec -e PGPASSWORD="$DB_PASS" postgres \
  psql -U newapi -lqt | cut -d \| -f 1 | grep -cw 'new-api' || echo 0)

if [[ "$DB_EXISTS" -ge "1" ]]; then
  warn "目标 postgres 已有 new-api 库 — 将清空后导入"
  ask_continue
  docker exec -e PGPASSWORD="$DB_PASS" postgres \
    psql -U newapi -d postgres -c 'DROP DATABASE IF EXISTS "new-api"' 2>&1 | head -2
  docker exec -e PGPASSWORD="$DB_PASS" postgres \
    psql -U newapi -d postgres -c 'CREATE DATABASE "new-api"' 2>&1 | head -2
fi

info "导入 pg dump... (约 30 秒)"
docker cp "$PACKAGE_ROOT/db/new-api.dump" postgres:/tmp/new-api.dump
docker exec -e PGPASSWORD="$DB_PASS" postgres \
  pg_restore -U newapi -d new-api --clean --if-exists --no-owner /tmp/new-api.dump 2>&1 | tail -10
docker exec postgres rm /tmp/new-api.dump
ok "数据导入完成"

# 验证关键表行数
ROWS_USERS=$(docker exec -e PGPASSWORD="$DB_PASS" postgres psql -U newapi -d new-api -tAc "SELECT COUNT(*) FROM users" 2>/dev/null | tr -d ' ')
ROWS_PROMPTS=$(docker exec -e PGPASSWORD="$DB_PASS" postgres psql -U newapi -d new-api -tAc "SELECT COUNT(*) FROM prompt_archives" 2>/dev/null | tr -d ' ')
ROWS_LOGS=$(docker exec -e PGPASSWORD="$DB_PASS" postgres psql -U newapi -d new-api -tAc "SELECT COUNT(*) FROM logs" 2>/dev/null | tr -d ' ')
info "users 表 $ROWS_USERS 行, prompt_archives 表 ${ROWS_PROMPTS:-0} 行, logs 表 $ROWS_LOGS 行"

[[ "${ROWS_USERS:-0}" -ge "1" ]] || fail "users 表为空 — 数据导入异常"
ok "数据完整性验证通过"

# ============================================================
phase "阶段 8/9 :: 启动 new-api 容器"
# ============================================================

docker compose up -d new-api 2>&1 | tail -5
info "等 new-api 启动... (最多 60 秒)"
for i in $(seq 1 12); do
  CODE=$(curl -s -o /dev/null -w "%{http_code}" http://localhost:3000/api/status 2>/dev/null)
  echo "  ${i}x5s: /api/status = $CODE"
  if [[ "$CODE" == "200" ]]; then break; fi
  sleep 5
done
[[ "$CODE" == "200" ]] || fail "new-api 启动超时，看日志: docker compose logs new-api --tail 50"
ok "new-api 容器健康"

# ============================================================
phase "阶段 9/9 :: 端到端验证"
# ============================================================

echo "[A] 主页 /  →"
curl -s -o /dev/null -w "  HTTP %{http_code} (期望 200)\n" http://localhost:3000/

echo "[B] /api/status →"
curl -s http://localhost:3000/api/status | head -c 200
echo ""

echo "[C] /landing/prompts.html → "
curl -s -o /dev/null -w "  HTTP %{http_code} body %{size_download} bytes (期望 200, ~22KB)\n" http://localhost:3000/landing/prompts.html

echo "[D] /console/prompts (React 路由) →"
curl -s -o /dev/null -w "  HTTP %{http_code} (期望 200)\n" http://localhost:3000/console/prompts

echo "[E] DB 中的关键数据"
docker exec -e PGPASSWORD="$DB_PASS" postgres psql -U newapi -d new-api -c "
  SELECT 'users' AS tbl, COUNT(*) AS rows FROM users
  UNION ALL SELECT 'tokens', COUNT(*) FROM tokens
  UNION ALL SELECT 'channels', COUNT(*) FROM channels
  UNION ALL SELECT 'prompt_archives', COUNT(*) FROM prompt_archives
  UNION ALL SELECT 'audit_events', COUNT(*) FROM audit_events
  UNION ALL SELECT 'logs', COUNT(*) FROM logs
  ORDER BY tbl;
" 2>&1 | head -15

echo ""
echo "═════════════════════════════════════════════════════════════"
echo "  🎉 部署完成"
echo "═════════════════════════════════════════════════════════════"
echo ""
echo "  浏览器访问: http://localhost:3000"
echo "  登录:      admin / admin123（如果迁移前改过密码，用迁移后的）"
echo ""
echo "  Sidebar 应该看到:"
echo "    Chat: Playground, Chat"
echo "    Console: Dashboard, Token, Usage Logs, Prompt 历史, ..."
echo "    Personal: Wallet, ..."
echo "    Admin: Channel, ..."
echo ""
echo "  关键验证点:"
echo "    1. 登进去后 Prompt 历史菜单可见且能打开"
echo "    2. 6 条 prompt 历史数据可见（迁移过来的）"
echo "    3. 渠道里 Vendor Key 能解密（CRYPTO_SECRET 同步成功的硬证据）"
echo ""
echo "  下一步: bash $TARGET_REPO/migrate/verify.sh (深度健康检查)"
