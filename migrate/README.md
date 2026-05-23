# NeuToken 项目迁移指南

> 源 Mac → 目标 Mac (M1 Pro 16G, macOS 26.5)，同 WiFi 网络

## 三件套

| 脚本 | 跑在哪 | 用时 | 作用 |
|---|---|---|---|
| `pack.sh` | 源 Mac | 5-15 分钟 | 打包源码 + DB + 镜像 + 密钥 → 单个 `.tar.gz` |
| `deploy.sh` | 目标 Mac | 10-20 分钟 | 9 阶段部署，每阶段检查通过才进下一步 |
| `verify.sh` | 目标 Mac | 30 秒 | 深度健康检查（容器/HTTP/数据/加密/安全 patch 全验） |

## 完整流程

### 第一步 · 源 Mac 上打包

```bash
cd ~/work/newapi
bash migrate/pack.sh
```

输出: `~/Desktop/neutoken-migrate-2026-05-12-HHMMSS.tar.gz`（约 700-900 MB）

打包过程会做 8 阶段检查（任何一步失败立即停）：

```
阶段 1/8  源 Mac 前置检查（Docker / .env / CRYPTO_SECRET 等）
阶段 2/8  创建打包暂存区
阶段 3/8  打包源码（排除 node_modules / dist / data / logs）
阶段 4/8  备份配置（.env / VERSION / docker-compose.yml）
阶段 5/8  PostgreSQL pg_dump（custom format，跨版本兼容）
阶段 6/8  保存 Docker 镜像 newapi-hardened（约 600 MB 压缩后）
阶段 7/8  生成 MANIFEST + sha256 校验和
阶段 8/8  打成单个 tar.gz
```

### 第二步 · 传输到目标 Mac

四种方式选一种：

```bash
# 方式 A · AirDrop（最方便）
# Finder 右键 .tar.gz → 共享 → AirDrop → 选目标 Mac

# 方式 B · scp（最快，需目标 Mac 先开 SSH）
# 目标 Mac:    系统设置 → 通用 → 共享 → 远程登录 ✓
# 源 Mac 跑:
scp ~/Desktop/neutoken-migrate-*.tar.gz <用户名>@<目标IP>:~/Desktop/

# 方式 C · 临时 HTTP server
# 源 Mac:
cd ~/Desktop && python3 -m http.server 8000
# 目标 Mac:
curl -O http://<源IP>:8000/neutoken-migrate-*.tar.gz

# 方式 D · U 盘 / 移动硬盘 / 共享文件夹
```

### 第三步 · 目标 Mac 上部署

```bash
cd ~/Desktop
tar -xzf neutoken-migrate-*.tar.gz
cd neutoken-migrate-*

bash source/migrate/deploy.sh
```

部署过程 9 阶段（每阶段都会等你按 Enter 才继续，方便观察）：

```
阶段 1/9  目标 Mac 环境预检（macOS / arch / 内存 / Docker / 端口）
阶段 2/9  验证迁移包完整性（sha256 校验所有文件）
阶段 3/9  部署源码到 ~/work/newapi/
阶段 4/9  加载 Docker 镜像 newapi-hardened
阶段 5/9  拉 postgres + redis base 镜像（从 Docker Hub）
阶段 6/9  启动 postgres + redis（等 healthy）
阶段 7/9  还原 PostgreSQL 数据（pg_restore）
阶段 8/9  启动 new-api 容器（等 /api/status = 200）
阶段 9/9  端到端验证（HTTP + DB + 路由）
```

### 第四步 · 深度健康检查

```bash
bash ~/work/newapi/migrate/verify.sh
```

6 个维度的检查：

```
1. 容器状态      new-api + postgres + redis 都 running
2. HTTP endpoint /api/status, /, /landing/prompts.html, /console/prompts, /api/prompts/me
3. 数据完整性    users / tokens / channels / prompt_archives / audit_events / logs 行数
4. 加密链路      channels.key 与 prompt_raw_enc 含 enc:v1: 前缀
5. env 配置      CRYPTO_SECRET / SESSION_SECRET / DB_PASSWORD / PROMPT_ARCHIVE_ENABLED
6. 安全 patch    H-3 AES-GCM / M-4 登录锁定 / D4 raw 清理 函数都在二进制里
```

最后输出 PASS / FAIL 数 + 浏览器手动验证步骤。

## 关键提醒

### CRYPTO_SECRET 同步是命脉

`.env` 里的 `CRYPTO_SECRET` 是 AES-GCM 加密的密钥派生输入。**两台 Mac 必须用同一个 SECRET**，否则：
- 渠道 vendor key 解密失败 → 所有 AI 调用 401
- 历史 prompt_archives 的 raw 字段解密失败 → 看不到历史 prompt 原文
- 但**脱敏版字段不受影响**（永久保留的 redacted 字段）

`pack.sh` 自动把 `.env` 打进包，`deploy.sh` 自动放到目标位置，所以正常流程不会丢。但如果你**手动改了某一边的 .env**，要确保改后两边一致。

### Docker Desktop 内存

M1 Pro 16GB → 给 Docker 8GB 是甜区。设置：

```
Docker Desktop → Settings → Resources → Memory: 8 GB → Apply & restart
```

否则前端 build 时 OOM（虽然这次部署用的是预编译镜像，理论上不需要再 build，但保险）。

### 端口冲突

目标机的 3000 端口不能被占用。如果占了：

```bash
lsof -nP -iTCP:3000 -sTCP:LISTEN   # 看谁占了
```

杀掉那个进程或改 docker-compose.yml 里的端口映射。

## 故障排查

### 部署失败时

`deploy.sh` 任何阶段失败都会 `exit 1` 并打印红色 ✗。常见情况：

| 症状 | 原因 | 修复 |
|---|---|---|
| 阶段 1 Docker daemon 未运行 | Docker Desktop 没开 | 打开 Docker Desktop 等绿灯 |
| 阶段 2 sha256 校验失败 | 传输中损坏 | 重新传一次 |
| 阶段 4 镜像加载失败 | tar.gz 损坏 | 重新打包 |
| 阶段 7 pg_restore 报错 | 目标库与源版本不兼容 | 升级目标 postgres 镜像 |
| 阶段 8 /api/status 超时 | 配置错误 | `docker compose logs new-api --tail 50` 看错 |

### 数据看不到（迁移后但 prompt_archives 表空）

源 Mac 上没数据。先确认源 Mac 的表行数：

```bash
docker exec postgres psql -U newapi -d new-api -c "SELECT COUNT(*) FROM prompt_archives"
```

### 安全加固 patch 没生效

迁移用的是预编译镜像，所有 14 个 patch 都在二进制里。`verify.sh` 的阶段 6 会验证。如果失败说明镜像本身有问题，重新在源 Mac 上 build 后再打包。

## 联系

如果遇到上面没覆盖的问题，截图 + 错误堆栈贴回会话，会立刻定位修复。
