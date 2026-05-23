#!/usr/bin/env bash
# ========================================================================
# NeuToken 项目迁移 · 打包阶段（在源 Mac 上跑）
# ========================================================================
# 输出: ~/Desktop/neutoken-migrate-YYYY-MM-DD-HHMMSS.tar.gz
#       含: 源码 + Docker 镜像 + PostgreSQL dump + .env 密钥 + manifest 校验
#
# 用法: bash migrate/pack.sh
# 用时: 5-15 分钟（看磁盘 + 数据量）
# ========================================================================
set -e  # 任何一步失败立即退出

REPO="${HOME}/work/newapi"
TS=$(date +%Y-%m-%d-%H%M%S)
STAGING="/tmp/neutoken-migrate-${TS}"
OUTPUT="${HOME}/Desktop/neutoken-migrate-${TS}.tar.gz"
IMAGE_NAME="newapi-hardened:latest"

# ─── 颜色 ───────────────────────────────────────────────────────────
RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; NC='\033[0m'
ok()    { echo -e "${GREEN}✓${NC} $1"; }
warn()  { echo -e "${YELLOW}⚠${NC} $1"; }
fail()  { echo -e "${RED}✗${NC} $1"; exit 1; }
phase() { echo ""; echo "═════════════════════════════════════════════════════════════"; echo "  $1"; echo "═════════════════════════════════════════════════════════════"; }

# ============================================================
phase "阶段 1/8 :: 源 Mac 前置检查"
# ============================================================

cd "$REPO" || fail "找不到 $REPO，请确认 newapi 项目路径"
ok "项目目录存在: $REPO"

command -v docker >/dev/null 2>&1 || fail "docker 命令不存在，请确认 Docker Desktop 已安装并启动"
ok "docker 命令可用"

docker ps >/dev/null 2>&1 || fail "Docker 未启动 — 打开 Docker Desktop 等它就绪"
ok "Docker daemon 运行中"

docker compose ps new-api 2>&1 | grep -q "running" || warn "new-api 容器未运行（继续打包，但目标机部署后才能验证）"

# 关键: CRYPTO_SECRET 不能丢
test -f .env || fail ".env 文件不存在 — 这里有 CRYPTO_SECRET，丢了 AES 加密数据全废"
grep -q "^CRYPTO_SECRET=" .env || fail ".env 里没有 CRYPTO_SECRET= 这一行 — 加密数据迁移会失败"
ok ".env 含 CRYPTO_SECRET (后 8 位: ...$(grep '^CRYPTO_SECRET=' .env | tail -c 9))"

grep -q "^DB_PASSWORD=" .env || fail ".env 里没有 DB_PASSWORD"
ok ".env 含 DB_PASSWORD"

docker images "$IMAGE_NAME" --format '{{.ID}}' | head -1 | grep -q . || fail "$IMAGE_NAME 镜像不存在，先 docker build 一次"
IMG_SIZE=$(docker images "$IMAGE_NAME" --format '{{.Size}}')
ok "镜像存在: $IMAGE_NAME ($IMG_SIZE)"

# 磁盘空间检查（至少 5GB 空闲）
FREE_GB=$(df -g / | awk 'NR==2 {print $4}')
[[ "$FREE_GB" -ge 5 ]] || fail "可用磁盘空间不足 5GB（当前 ${FREE_GB}GB）"
ok "可用磁盘 ${FREE_GB}GB"

# ============================================================
phase "阶段 2/8 :: 创建打包暂存区"
# ============================================================

rm -rf "$STAGING"
mkdir -p "$STAGING"/{source,db,image,config}
ok "暂存区创建: $STAGING"

# ============================================================
phase "阶段 3/8 :: 打包源码（排除 node_modules / dist / .git/objects）"
# ============================================================

# 用 rsync 排除大目录，比 tar 更精确
rsync -a --delete \
  --exclude='web/default/node_modules' \
  --exclude='web/default/dist' \
  --exclude='web/classic/node_modules' \
  --exclude='web/classic/dist' \
  --exclude='.git/objects/pack' \
  --exclude='secure_phase_two/*.bak' \
  --exclude='data/*' \
  --exclude='logs/*' \
  --exclude='glm_eval/checkpoint/*' \
  --exclude='glm_eval/results/*.json' \
  --exclude='glm_eval/.venv' \
  --exclude='__pycache__' \
  "$REPO/" "$STAGING/source/"
SRC_SIZE=$(du -sh "$STAGING/source" | awk '{print $1}')
ok "源码打包完成: $SRC_SIZE"

# ============================================================
phase "阶段 4/8 :: 备份配置（.env / VERSION / docker-compose.yml）"
# ============================================================

cp .env "$STAGING/config/.env" || fail ".env 备份失败"
cp VERSION "$STAGING/config/VERSION" 2>/dev/null || warn "VERSION 文件不存在（不致命）"
cp docker-compose.yml "$STAGING/config/docker-compose.yml" || fail "docker-compose.yml 备份失败"
ok "配置文件备份完成"

# ============================================================
phase "阶段 5/8 :: PostgreSQL dump（pg_dump custom format，跨版本兼容）"
# ============================================================

PG_RUNNING=$(docker ps --filter "name=postgres" --format '{{.Names}}' | head -1)
if [[ -z "$PG_RUNNING" ]]; then
  fail "postgres 容器未运行 — 启动 docker compose up -d 再跑本脚本"
fi
ok "postgres 容器运行中: $PG_RUNNING"

DB_PASS=$(grep '^DB_PASSWORD=' .env | cut -d= -f2-)
docker exec -e PGPASSWORD="$DB_PASS" "$PG_RUNNING" \
  pg_dump -U newapi -d new-api -Fc -f /tmp/new-api.dump 2>&1 | head -3
docker cp "$PG_RUNNING:/tmp/new-api.dump" "$STAGING/db/new-api.dump" || fail "pg_dump 失败"
docker exec "$PG_RUNNING" rm /tmp/new-api.dump

DUMP_SIZE=$(du -sh "$STAGING/db/new-api.dump" | awk '{print $1}')
ok "PostgreSQL dump 完成: $DUMP_SIZE"

# 顺手 dump 一份表清单 + 行数，方便目标机验证
docker exec -e PGPASSWORD="$DB_PASS" "$PG_RUNNING" \
  psql -U newapi -d new-api -c "SELECT schemaname, relname, n_live_tup AS rows
                                  FROM pg_stat_user_tables
                                  ORDER BY relname" > "$STAGING/db/table_inventory.txt" 2>&1
ok "表清单备份: db/table_inventory.txt"

# ============================================================
phase "阶段 6/8 :: 保存 Docker 镜像（newapi + postgres + redis）"
# ============================================================

echo "  正在 docker save newapi-hardened... (约 30-90 秒)"
docker save "$IMAGE_NAME" | gzip -c > "$STAGING/image/newapi-hardened.tar.gz"
IMG_TAR_SIZE=$(du -sh "$STAGING/image/newapi-hardened.tar.gz" | awk '{print $1}')
ok "newapi-hardened 镜像保存完成: $IMG_TAR_SIZE"

# postgres + redis base 镜像也一并打包 — 避免目标机网络无法访问 docker.io 时卡死
echo "  正在 docker save postgres:15-alpine + redis:7-alpine... (约 30 秒)"
if docker images postgres:15-alpine --format '{{.ID}}' | grep -q .; then
  docker save postgres:15-alpine redis:7-alpine | gzip -c > "$STAGING/image/base-images.tar.gz"
  BASE_SIZE=$(du -sh "$STAGING/image/base-images.tar.gz" | awk '{print $1}')
  ok "base 镜像保存完成: $BASE_SIZE"
else
  warn "本机没有 postgres/redis 镜像缓存，跳过 base-images.tar.gz 打包"
  warn "目标机部署时需要能访问 docker.io 或配置 mirror"
fi

# ============================================================
phase "阶段 7/8 :: 生成 manifest + 校验和"
# ============================================================

cat > "$STAGING/MANIFEST.txt" <<EOF
NeuToken 迁移包 · 元信息
========================================
打包时间: $(date '+%Y-%m-%d %H:%M:%S %Z')
源 Mac:   $(hostname) ($(sw_vers -productVersion))
打包者:   $(whoami)
项目路径: $REPO

包含内容:
  source/      项目源码（已排除 node_modules / dist / data / logs）
  db/          PostgreSQL pg_dump（pg_dump -Fc 格式）+ 表清单
  image/       newapi-hardened:latest Docker 镜像 tar
  config/      .env + VERSION + docker-compose.yml

部署到目标机后:
  bash source/migrate/deploy.sh

关键提醒:
  ⚠️  .env 含 CRYPTO_SECRET — 必须用同一个 SECRET 才能解密历史 prompt
  ⚠️  目标机 Docker Desktop 内存建议 ≥ 8GB
EOF

(cd "$STAGING" && find . -type f -exec shasum -a 256 {} \; > MANIFEST.sha256)
ok "manifest + 校验和生成完成"

# ============================================================
phase "阶段 8/8 :: 打成单个 tar.gz"
# ============================================================

echo "  正在压缩... (约 1-3 分钟)"
tar -czf "$OUTPUT" -C /tmp "neutoken-migrate-${TS}"
FINAL_SIZE=$(du -sh "$OUTPUT" | awk '{print $1}')
ok "打包完成: $OUTPUT ($FINAL_SIZE)"

# 清理暂存区
rm -rf "$STAGING"
ok "暂存区清理完成"

echo ""
echo "═════════════════════════════════════════════════════════════"
echo "  🎉 打包完成"
echo "═════════════════════════════════════════════════════════════"
echo ""
echo "  文件:  $OUTPUT"
echo "  大小:  $FINAL_SIZE"
echo ""
echo "下一步 — 传到目标 Mac（4 种方式三选一）："
echo ""
echo "  方式 A · AirDrop（最方便）"
echo "    Finder 找到 $OUTPUT → 右键 → 共享 → AirDrop → 目标 Mac"
echo ""
echo "  方式 B · scp（最快，需要目标 Mac 开 SSH）"
echo "    目标 Mac: 系统设置 → 通用 → 共享 → 远程登录 打开"
echo "    源 Mac:"
echo "      scp '$OUTPUT' admin@目标IP:~/Desktop/"
echo ""
echo "  方式 C · 临时 HTTP 服务"
echo "    源 Mac (本目录):"
echo "      cd ~/Desktop && python3 -m http.server 8000"
echo "    目标 Mac:"
echo "      curl -O http://源IP:8000/$(basename "$OUTPUT")"
echo ""
echo "  方式 D · U 盘 / 移动硬盘"
echo "    把 $OUTPUT 拷到 U 盘 → 插目标 Mac"
echo ""
echo "传到目标 Mac 后:"
echo "  cd ~/Desktop"
echo "  tar -xzf $(basename "$OUTPUT")"
echo "  bash neutoken-migrate-${TS}/source/migrate/deploy.sh"
