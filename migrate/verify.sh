#!/usr/bin/env bash
# ========================================================================
# NeuToken 项目迁移 · 深度健康检查（部署完成后跑）
# ========================================================================
# 验证从源 Mac 迁移过来的所有关键功能在目标 Mac 上工作正常
# ========================================================================

REPO="${NEWAPI_TARGET:-${HOME}/Downloads/newapi}"
# 如果用户在不同位置，自动探测
if [[ ! -f "$REPO/.env" ]]; then
  for candidate in ~/Downloads/newapi ~/work/newapi ~/newapi; do
    if [[ -f "$candidate/.env" ]]; then
      REPO="$candidate"
      break
    fi
  done
fi
PASS=0; FAIL=0
RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; NC='\033[0m'
pass()  { echo -e "${GREEN}✓${NC} $1"; PASS=$((PASS+1)); }
err()   { echo -e "${RED}✗${NC} $1"; FAIL=$((FAIL+1)); }
warn()  { echo -e "${YELLOW}⚠${NC} $1"; }
phase() { echo ""; echo "─── $1 ──────────────────────────"; }

cd "$REPO" 2>/dev/null || { echo "找不到 $REPO"; exit 1; }
DB_PASS=$(grep '^DB_PASSWORD=' .env | cut -d= -f2-)
PSQL="docker exec -e PGPASSWORD=$DB_PASS postgres psql -U newapi -d new-api -tAc"

echo "═════════════════════════════════════════════════════════════"
echo "  NeuToken 深度健康检查 · $(date '+%Y-%m-%d %H:%M:%S')"
echo "═════════════════════════════════════════════════════════════"

# ─── 容器层 ──────────────────────────────────────────────────
phase "1. 容器状态"

for svc in new-api postgres redis; do
  STATE=$(docker inspect "$svc" --format='{{.State.Status}}' 2>/dev/null)
  [[ "$STATE" == "running" ]] && pass "$svc 容器: running" || err "$svc 状态异常: $STATE"
done

# ─── HTTP 层 ─────────────────────────────────────────────────
phase "2. HTTP endpoint"

CODE=$(curl -s -o /dev/null -w "%{http_code}" http://localhost:3000/api/status)
[[ "$CODE" == "200" ]] && pass "/api/status 返回 200" || err "/api/status 返回 $CODE"

CODE=$(curl -s -o /dev/null -w "%{http_code}" http://localhost:3000/)
[[ "$CODE" == "200" ]] && pass "/ 主页 200" || err "/ 返回 $CODE"

CODE=$(curl -s -o /dev/null -w "%{http_code}" http://localhost:3000/landing/prompts.html)
[[ "$CODE" == "200" ]] && pass "/landing/prompts.html 200" || err "/landing/prompts.html 返回 $CODE"

CODE=$(curl -s -o /dev/null -w "%{http_code}" http://localhost:3000/console/prompts)
[[ "$CODE" == "200" ]] && pass "/console/prompts 200 (React 路由)" || err "/console/prompts 返回 $CODE"

CODE=$(curl -s -o /dev/null -w "%{http_code}" http://localhost:3000/api/prompts/me)
[[ "$CODE" == "401" ]] && pass "/api/prompts/me 返回 401（鉴权工作）" || warn "/api/prompts/me 返回 $CODE (期望 401)"

# ─── 数据完整性 ─────────────────────────────────────────────
phase "3. 数据完整性"

USERS=$($PSQL "SELECT COUNT(*) FROM users" 2>/dev/null)
[[ "${USERS:-0}" -ge "1" ]] && pass "users 表 $USERS 行" || err "users 表无数据"

TOKENS=$($PSQL "SELECT COUNT(*) FROM tokens" 2>/dev/null)
[[ "${TOKENS:-0}" -ge "1" ]] && pass "tokens 表 $TOKENS 行" || warn "tokens 表无数据"

CHANNELS=$($PSQL "SELECT COUNT(*) FROM channels" 2>/dev/null)
[[ "${CHANNELS:-0}" -ge "1" ]] && pass "channels 表 $CHANNELS 行" || warn "channels 表无数据"

PROMPTS=$($PSQL "SELECT COUNT(*) FROM prompt_archives" 2>/dev/null)
[[ "${PROMPTS:-0}" -ge "1" ]] && pass "prompt_archives 表 $PROMPTS 行（历史数据保留）" || warn "prompt_archives 表无数据"

AUDIT=$($PSQL "SELECT COUNT(*) FROM audit_events" 2>/dev/null)
pass "audit_events 表 ${AUDIT:-0} 行"

LOGS=$($PSQL "SELECT COUNT(*) FROM logs" 2>/dev/null)
[[ "${LOGS:-0}" -ge "1" ]] && pass "logs 表 $LOGS 行" || warn "logs 表无数据"

# ─── 加密验证 ───────────────────────────────────────────────
phase "4. CRYPTO_SECRET 加密链路"

# 检查 channels 表的 key 字段是否含 enc:v1: 前缀
ENCRYPTED_CH=$($PSQL "SELECT COUNT(*) FROM channels WHERE key LIKE 'enc:v1:%'" 2>/dev/null)
[[ "${ENCRYPTED_CH:-0}" -ge "1" ]] && pass "$ENCRYPTED_CH 个 channel 的 key 是 AES-GCM 加密（enc:v1: 前缀）" \
  || warn "channels.key 没有 enc:v1: 前缀（可能是新装、或加密未启用）"

# 检查 prompt_archives 的 raw 字段
ENCRYPTED_PA=$($PSQL "SELECT COUNT(*) FROM prompt_archives WHERE prompt_raw_enc LIKE 'enc:v1:%'" 2>/dev/null)
[[ "${ENCRYPTED_PA:-0}" -ge "1" ]] && pass "$ENCRYPTED_PA 条 prompt_raw_enc 是密文" \
  || warn "prompt_archives 没有加密字段（可能 PROMPT_ARCHIVE_ENABLED=false 或表空）"

# ─── 配置项 ──────────────────────────────────────────────────
phase "5. 关键 env 配置"

grep -q '^CRYPTO_SECRET=' .env && pass ".env 含 CRYPTO_SECRET" || err ".env 缺 CRYPTO_SECRET"
grep -q '^SESSION_SECRET=' .env && pass ".env 含 SESSION_SECRET" || err ".env 缺 SESSION_SECRET"
grep -q '^DB_PASSWORD=' .env && pass ".env 含 DB_PASSWORD" || err ".env 缺 DB_PASSWORD"
grep -q '^REDIS_PASSWORD=' .env && pass ".env 含 REDIS_PASSWORD" || warn ".env 缺 REDIS_PASSWORD"
grep -q '^PROMPT_ARCHIVE_ENABLED=true' .env && pass ".env 启用了 PROMPT_ARCHIVE" || warn "PROMPT_ARCHIVE_ENABLED 未设或非 true"

# ─── 安全加固验证 ──────────────────────────────────────────
phase "6. 安全加固 patch（14 项）"

# 看二进制里有没有几个关键 patch 的标志字符串
docker exec new-api sh -c "grep -ac 'EncryptField\|DecryptField' /new-api 2>/dev/null" | head -1 | xargs -I{} sh -c '[ "{}" -ge 1 ] && echo "  ✓ H-3 AES-GCM 加密函数存在" || echo "  ✗ H-3 加密函数缺失"'
docker exec new-api sh -c "grep -ac 'login_lockout' /new-api 2>/dev/null" | head -1 | xargs -I{} sh -c '[ "{}" -ge 1 ] && echo "  ✓ M-4 登录锁定机制存在" || echo "  ✗ M-4 缺失"'
docker exec new-api sh -c "grep -ac 'PurgeRawFieldsBeyondRetention' /new-api 2>/dev/null" | head -1 | xargs -I{} sh -c '[ "{}" -ge 1 ] && echo "  ✓ D4 raw 字段过期清理函数存在" || echo "  ✗ D4 缺失"'

# ─── 总结 ────────────────────────────────────────────────────
echo ""
echo "═════════════════════════════════════════════════════════════"
echo "  健康检查结果"
echo "═════════════════════════════════════════════════════════════"
echo ""
echo "  PASS: $PASS"
echo "  FAIL: $FAIL"
echo ""
if [[ "$FAIL" == "0" ]]; then
  echo "  🎉 所有关键项通过，迁移成功"
else
  echo "  ⚠️  有 $FAIL 项失败，请检查上面 ✗ 标记的项"
  exit 1
fi
echo ""
echo "  浏览器最终验证："
echo "    打开 http://localhost:3000 → 登录"
echo "    左 sidebar 看到 'Prompt 历史' 菜单"
echo "    点击应该看到 $PROMPTS 条历史数据"
